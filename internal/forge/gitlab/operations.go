package gitlab

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
	var mr apiMergeRequest
	if err := c.do(ctx, http.MethodGet, mergeRequestPath(ref, ""), nil, &mr); err != nil {
		return nil, err
	}
	return mr.normalize(), nil
}

// Diff implements [forge.Client].
func (c *Client) Diff(ctx context.Context, ref forge.PRRef) (*forge.Diff, error) {
	diff := &forge.Diff{}

	for page := 1; page <= maxPages; page++ {
		path := fmt.Sprintf("%s?per_page=%d&page=%d", mergeRequestPath(ref, "/diffs"), pageSize, page)

		var files []apiDiff
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

// Compare implements [forge.Client]. A commit that no longer exists — the
// usual outcome of a force push — is reported as [forge.ErrNoCompare] rather
// than as a failure.
func (c *Client) Compare(ctx context.Context, ref forge.PRRef, base, head string) (*forge.Diff, error) {
	if base == "" || head == "" {
		return nil, forge.ErrNoCompare
	}
	if base == head {
		return &forge.Diff{}, nil
	}

	path := fmt.Sprintf("%s/repository/compare?from=%s&to=%s&straight=true",
		projectPath(ref), url.QueryEscape(base), url.QueryEscape(head))

	var body struct {
		Diffs          []apiDiff `json:"diffs"`
		CompareTimeout bool      `json:"compare_timeout"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &body); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %s...%s", forge.ErrNoCompare, base, head)
		}
		return nil, err
	}

	diff := &forge.Diff{Truncated: body.CompareTimeout}
	for _, f := range body.Diffs {
		if len(diff.Files) >= c.maxFiles {
			diff.Truncated = true
			break
		}
		diff.Files = append(diff.Files, f.normalize())
	}
	return diff, nil
}

// Comments implements [forge.Client]. Discussions are listed rather than
// notes, because a reply has to go back to the discussion it belongs to and
// only this listing says which that is.
func (c *Client) Comments(ctx context.Context, ref forge.PRRef) ([]forge.Comment, error) {
	var out []forge.Comment

	for page := 1; page <= maxPages; page++ {
		path := fmt.Sprintf("%s?per_page=%d&page=%d", mergeRequestPath(ref, "/discussions"), pageSize, page)

		var discussions []apiDiscussion
		if err := c.do(ctx, http.MethodGet, path, nil, &discussions); err != nil {
			return nil, err
		}
		for _, d := range discussions {
			for _, n := range d.Notes {
				// GitLab writes notes of its own ("changed the description"),
				// which are not anybody's review.
				if n.System {
					continue
				}
				out = append(out, n.normalize(d.ID))
			}
		}
		if len(discussions) < pageSize {
			return out, nil
		}
	}
	return out, nil
}

// UpsertSummary implements [forge.Client]. The marker is an HTML comment, so
// it identifies kibitz's own note without being visible in it.
func (c *Client) UpsertSummary(ctx context.Context, ref forge.PRRef, marker, body string) error {
	existing, err := c.findNote(ctx, ref, marker)
	if err != nil {
		return err
	}

	payload := map[string]string{"body": marker + "\n" + body}
	if existing != 0 {
		path := mergeRequestPath(ref, "/notes/"+strconv.FormatInt(existing, 10))
		return c.do(ctx, http.MethodPut, path, payload, nil)
	}
	return c.do(ctx, http.MethodPost, mergeRequestPath(ref, "/notes"), payload, nil)
}

// findNote looks for kibitz's own note, so that repeated reviews replace their
// summary instead of stacking up.
func (c *Client) findNote(ctx context.Context, ref forge.PRRef, marker string) (int64, error) {
	for page := 1; page <= maxPages; page++ {
		path := fmt.Sprintf("%s?per_page=%d&page=%d", mergeRequestPath(ref, "/notes"), pageSize, page)

		var notes []apiNote
		if err := c.do(ctx, http.MethodGet, path, nil, &notes); err != nil {
			return 0, err
		}
		for _, n := range notes {
			if strings.Contains(n.Body, marker) {
				return n.ID, nil
			}
		}
		if len(notes) < pageSize {
			return 0, nil
		}
	}
	return 0, nil
}

// ReplyToThread implements [forge.Client]. GitLab calls a thread a discussion.
func (c *Client) ReplyToThread(ctx context.Context, ref forge.PRRef, threadID, body string) error {
	if threadID == "" {
		// Nothing to reply to: say it on the merge request instead of
		// dropping the answer.
		return c.do(ctx, http.MethodPost, mergeRequestPath(ref, "/notes"), map[string]string{"body": body}, nil)
	}

	path := mergeRequestPath(ref, "/discussions/"+url.PathEscape(threadID)+"/notes")
	return c.do(ctx, http.MethodPost, path, map[string]string{"body": body}, nil)
}

// CreateReview implements [forge.Client].
//
// GitLab has no single call that posts a review with its comments, so each
// finding becomes a discussion of its own. They are posted one at a time, and
// one that GitLab rejects does not take the rest with it: a finding kibitz
// cannot anchor is still worth saying.
func (c *Client) CreateReview(ctx context.Context, ref forge.PRRef, r forge.Review) error {
	if len(r.Comments) == 0 {
		return nil
	}

	refs, err := c.diffRefs(ctx, ref, r.CommitSHA)
	if err != nil {
		return err
	}
	if !refs.complete() {
		// Without the three commits GitLab will not anchor anything, and
		// there is no point trying each one.
		return fmt.Errorf("%w: the merge request has no diff refs yet", forge.ErrInvalidPosition)
	}

	var rejected int
	for _, comment := range r.Comments {
		if err := c.createDiffDiscussion(ctx, ref, refs, comment); err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest {
				// GitLab refuses a position it cannot place in the diff.
				rejected++
				continue
			}
			return err
		}
	}
	if rejected == len(r.Comments) {
		return fmt.Errorf("%w: GitLab rejected every position", forge.ErrInvalidPosition)
	}
	return nil
}

// createDiffDiscussion posts one finding against one line.
//
// Both paths are required, and which line number is sent decides which side
// of the diff the comment lands on: an added line carries only new_line, a
// removed line only old_line. Sending both would place it on an unchanged
// line, which is not where the finding is.
func (c *Client) createDiffDiscussion(ctx context.Context, ref forge.PRRef, refs diffRefs, comment forge.InlineComment) error {
	position := map[string]any{
		"position_type": "text",
		"base_sha":      refs.BaseSHA,
		"head_sha":      refs.HeadSHA,
		"start_sha":     refs.StartSHA,
		"new_path":      comment.Path,
		"old_path":      comment.Path,
		"new_line":      comment.Line,
	}

	payload := map[string]any{
		"body":     body(comment),
		"position": position,
	}
	return c.do(ctx, http.MethodPost, mergeRequestPath(ref, "/discussions"), payload, nil)
}

// body renders one finding. GitLab's suggestion syntax is a fenced block
// tagged "suggestion", and it applies to the line the comment is on.
func body(comment forge.InlineComment) string {
	var b strings.Builder
	b.WriteString(comment.Body)

	// A range is stated rather than anchored: GitLab's multi-line positions
	// need a line_code kibitz cannot compute without the file's blob, and a
	// comment on the first line that names the range is more useful than none.
	if comment.EndLine > comment.Line {
		fmt.Fprintf(&b, "\n\n_対象: %d行目〜%d行目_", comment.Line, comment.EndLine)
	}
	if comment.Suggestion != "" {
		b.WriteString("\n\n```suggestion\n")
		b.WriteString(strings.TrimRight(comment.Suggestion, "\n"))
		b.WriteString("\n```\n")
	}
	return b.String()
}

// diffRefs fetches the three commits a diff note must be anchored to.
func (c *Client) diffRefs(ctx context.Context, ref forge.PRRef, headSHA string) (diffRefs, error) {
	var mr apiMergeRequest
	if err := c.do(ctx, http.MethodGet, mergeRequestPath(ref, ""), nil, &mr); err != nil {
		return diffRefs{}, err
	}

	// The review was produced against a particular commit. If the merge
	// request has moved on since, the positions no longer describe it.
	if headSHA != "" && mr.DiffRefs.HeadSHA != "" && mr.DiffRefs.HeadSHA != headSHA {
		return diffRefs{}, fmt.Errorf("%w: the merge request moved from %s to %s while it was being reviewed",
			forge.ErrInvalidPosition, headSHA, mr.DiffRefs.HeadSHA)
	}
	return mr.DiffRefs, nil
}

// CloneAuth implements [forge.Client]. GitLab accepts an access token as the
// password over HTTPS, with a username it ignores; "oauth2" is the one its own
// documentation uses.
func (c *Client) CloneAuth(context.Context, forge.PRRef) (forge.CloneCredential, error) {
	return forge.CloneCredential{Username: "oauth2", Token: c.token}, nil
}

// maxFileBytes caps what [Client.ReadFile] will accept. The only file kibitz
// reads this way is its own settings.
const maxFileBytes = 256 << 10

// ReadFile implements [forge.Client].
//
// GitLab requires a ref, and documents "HEAD" as the way to say "whatever the
// default branch is" — which is the branch the settings have to come from.
// The whole path goes in one URL segment, so its separators are encoded too.
func (c *Client) ReadFile(ctx context.Context, ref forge.PRRef, path string) ([]byte, error) {
	var file struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		Size     int    `json:"size"`
	}
	endpoint := fmt.Sprintf("%s/repository/files/%s?ref=HEAD", projectPath(ref), escapeFilePath(path))
	if err := c.do(ctx, http.MethodGet, endpoint, nil, &file); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return nil, forge.ErrFileNotFound
		}
		return nil, err
	}
	if file.Size > maxFileBytes {
		return nil, fmt.Errorf("gitlab: %s is %d bytes, over the %d byte limit", path, file.Size, maxFileBytes)
	}
	if file.Encoding != "base64" {
		return nil, fmt.Errorf("gitlab: %s came back %s-encoded, which kibitz cannot read", path, file.Encoding)
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(file.Content), ""))
	if err != nil {
		return nil, fmt.Errorf("gitlab: decoding %s: %w", path, err)
	}
	return decoded, nil
}

// escapeFilePath encodes a path as one URL segment, separators included,
// which is the form the files API asks for.
func escapeFilePath(path string) string {
	return strings.ReplaceAll(url.PathEscape(path), "/", "%2F")
}
