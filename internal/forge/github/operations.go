package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
)

// PullRequest implements [forge.Client].
func (c *Client) PullRequest(ctx context.Context, ref forge.PRRef) (*event.PullRequest, error) {
	var pr pullRequest
	if err := c.do(ctx, http.MethodGet, repoPath(ref, "/pulls/"+strconv.Itoa(ref.Number)), nil, &pr); err != nil {
		return nil, err
	}
	return pr.normalize(ref), nil
}

// Diff implements [forge.Client]. GitHub returns the patch per file, which is
// what the reviewer needs; the whole-diff media type would have to be parsed
// back apart.
func (c *Client) Diff(ctx context.Context, ref forge.PRRef) (*forge.Diff, error) {
	diff := &forge.Diff{}

	for page := 1; page <= maxPages; page++ {
		path := fmt.Sprintf("%s/pulls/%d/files?per_page=%d&page=%d",
			repoPath(ref, ""), ref.Number, pageSize, page)

		var files []changedFile
		if err := c.do(ctx, http.MethodGet, path, nil, &files); err != nil {
			return nil, err
		}
		for _, f := range files {
			if len(diff.Files) >= c.maxFiles {
				diff.Truncated = true
				return diff, nil
			}
			diff.Files = append(diff.Files, f.normalize())
		}
		if len(files) < pageSize {
			return diff, nil
		}
	}

	diff.Truncated = true
	return diff, nil
}

// Compare implements [forge.Client]. It asks GitHub what changed between two
// commits, which is what makes a second review of a pull request look only at
// what was pushed since the first one.
//
// A commit that no longer exists — the usual outcome of a force push — is
// reported as [forge.ErrNoCompare] rather than as a failure: reviewing the
// whole diff is still correct.
func (c *Client) Compare(ctx context.Context, ref forge.PRRef, base, head string) (*forge.Diff, error) {
	if base == "" || head == "" {
		return nil, forge.ErrNoCompare
	}
	if base == head {
		return &forge.Diff{}, nil
	}

	diff := &forge.Diff{}
	for page := 1; page <= maxPages; page++ {
		path := fmt.Sprintf("%s/compare/%s...%s?per_page=%d&page=%d",
			repoPath(ref, ""), url.PathEscape(base), url.PathEscape(head), pageSize, page)

		var body struct {
			Files []changedFile `json:"files"`
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &body); err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				return nil, fmt.Errorf("%w: %s...%s", forge.ErrNoCompare, base, head)
			}
			return nil, err
		}
		for _, f := range body.Files {
			if len(diff.Files) >= c.maxFiles {
				diff.Truncated = true
				return diff, nil
			}
			diff.Files = append(diff.Files, f.normalize())
		}
		if len(body.Files) < pageSize {
			return diff, nil
		}
	}

	diff.Truncated = true
	return diff, nil
}

// Comments implements [forge.Client]. Both kinds are returned: conversation
// comments and comments anchored to a line, because a finding may already have
// been raised in either place.
func (c *Client) Comments(ctx context.Context, ref forge.PRRef) ([]forge.Comment, error) {
	var out []forge.Comment

	for page := 1; page <= maxPages; page++ {
		path := fmt.Sprintf("%s/issues/%d/comments?per_page=%d&page=%d",
			repoPath(ref, ""), ref.Number, pageSize, page)

		var comments []issueComment
		if err := c.do(ctx, http.MethodGet, path, nil, &comments); err != nil {
			return nil, err
		}
		for _, cm := range comments {
			out = append(out, cm.normalize())
		}
		if len(comments) < pageSize {
			break
		}
	}

	for page := 1; page <= maxPages; page++ {
		path := fmt.Sprintf("%s/pulls/%d/comments?per_page=%d&page=%d",
			repoPath(ref, ""), ref.Number, pageSize, page)

		var comments []reviewComment
		if err := c.do(ctx, http.MethodGet, path, nil, &comments); err != nil {
			return nil, err
		}
		for _, cm := range comments {
			out = append(out, cm.normalize())
		}
		if len(comments) < pageSize {
			break
		}
	}

	return out, nil
}

// CreateReview implements [forge.Client]. Findings are posted as one review
// rather than as individual comments: it is a single notification for the
// author and a single write against the rate limit.
func (c *Client) CreateReview(ctx context.Context, ref forge.PRRef, r forge.Review) error {
	body := createReviewRequest{
		Body: r.Summary,
		// kibitz never approves or requests changes; a human decides that.
		Event:    "COMMENT",
		CommitID: r.CommitSHA,
	}
	for _, cm := range r.Comments {
		body.Comments = append(body.Comments, reviewCommentRequest{
			Path:      cm.Path,
			Line:      commentEndLine(cm),
			StartLine: commentStartLine(cm),
			Side:      "RIGHT",
			StartSide: startSide(cm),
			Body:      renderComment(cm),
		})
	}

	path := fmt.Sprintf("%s/pulls/%d/reviews", repoPath(ref, ""), ref.Number)
	err := c.do(ctx, http.MethodPost, path, body, nil)

	// GitHub answers 422 when a comment names a line that is not part of the
	// diff, and rejects the entire review rather than that one comment.
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnprocessableEntity {
		return fmt.Errorf("%w: %w", forge.ErrInvalidPosition, err)
	}
	return err
}

// UpsertSummary implements [forge.Client]. The marker is an HTML comment in
// the body, invisible in the rendered page, which is how kibitz finds the
// comment it posted last time instead of stacking a new one on every push.
func (c *Client) UpsertSummary(ctx context.Context, ref forge.PRRef, marker, body string) error {
	if marker == "" {
		return fmt.Errorf("github: a summary marker is required")
	}
	content := marker + "\n" + body

	existing, err := c.findMarkedComment(ctx, ref, marker)
	if err != nil {
		return err
	}
	if existing != 0 {
		path := fmt.Sprintf("%s/issues/comments/%d", repoPath(ref, ""), existing)
		return c.do(ctx, http.MethodPatch, path, map[string]string{"body": content}, nil)
	}

	path := fmt.Sprintf("%s/issues/%d/comments", repoPath(ref, ""), ref.Number)
	return c.do(ctx, http.MethodPost, path, map[string]string{"body": content}, nil)
}

// findMarkedComment returns the id of kibitz's own marked comment, or 0.
func (c *Client) findMarkedComment(ctx context.Context, ref forge.PRRef, marker string) (int64, error) {
	for page := 1; page <= maxPages; page++ {
		path := fmt.Sprintf("%s/issues/%d/comments?per_page=%d&page=%d",
			repoPath(ref, ""), ref.Number, pageSize, page)

		var comments []issueComment
		if err := c.do(ctx, http.MethodGet, path, nil, &comments); err != nil {
			return 0, err
		}
		for _, cm := range comments {
			if strings.Contains(cm.Body, marker) {
				return cm.ID, nil
			}
		}
		if len(comments) < pageSize {
			return 0, nil
		}
	}
	return 0, nil
}

// ReplyToThread implements [forge.Client].
func (c *Client) ReplyToThread(ctx context.Context, ref forge.PRRef, threadID, body string) error {
	if threadID == "" {
		// A conversation comment has no thread of its own, so the reply is a
		// new conversation comment.
		path := fmt.Sprintf("%s/issues/%d/comments", repoPath(ref, ""), ref.Number)
		return c.do(ctx, http.MethodPost, path, map[string]string{"body": body}, nil)
	}

	id, err := strconv.ParseInt(threadID, 10, 64)
	if err != nil {
		return fmt.Errorf("github: thread id %q is not a comment id: %w", threadID, err)
	}
	path := fmt.Sprintf("%s/pulls/%d/comments/%d/replies", repoPath(ref, ""), ref.Number, id)
	return c.do(ctx, http.MethodPost, path, map[string]string{"body": body}, nil)
}

// CloneAuth implements [forge.Client]. The token goes into an HTTP header
// rather than the remote URL, so it cannot leak through git's error output or
// the reflog.
func (c *Client) CloneAuth(ctx context.Context, _ forge.PRRef) (forge.CloneCredential, error) {
	token, err := c.auth.installationToken(ctx)
	if err != nil {
		return forge.CloneCredential{}, err
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return forge.CloneCredential{
		Username:   "x-access-token",
		Token:      token,
		AuthHeader: "Authorization: Basic " + basic,
	}, nil
}

// renderComment turns a finding into GitHub markdown, including the suggested
// change block when the reviewer proposed one.
func renderComment(cm forge.InlineComment) string {
	if cm.Suggestion == "" {
		return cm.Body
	}
	var b strings.Builder
	b.WriteString(cm.Body)
	b.WriteString("\n\n```suggestion\n")
	b.WriteString(strings.TrimRight(cm.Suggestion, "\n"))
	b.WriteString("\n```")
	return b.String()
}

// GitHub anchors a multi-line comment with start_line..line; a single-line
// comment must not carry a start_line at all.
func commentEndLine(cm forge.InlineComment) int {
	if cm.EndLine > cm.Line {
		return cm.EndLine
	}
	return cm.Line
}

func commentStartLine(cm forge.InlineComment) int {
	if cm.EndLine > cm.Line {
		return cm.Line
	}
	return 0
}

func startSide(cm forge.InlineComment) string {
	if cm.EndLine > cm.Line {
		return "RIGHT"
	}
	return ""
}
