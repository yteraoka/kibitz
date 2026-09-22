package azuredevops

import (
	"context"
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
//
// On this platform the call is not an optimization. The service hook that
// started the job is unsigned, so its resource is a claim; this is what turns
// it into a fact.
func (c *Client) PullRequest(ctx context.Context, ref forge.PRRef) (*event.PullRequest, error) {
	var pr pullRequest
	if err := c.do(ctx, http.MethodGet, c.prPath(ref, ""), nil, nil, &pr); err != nil {
		return nil, err
	}
	if pr.PullRequestID == 0 {
		return nil, fmt.Errorf("azuredevops: %s came back without a pull request id", ref)
	}
	return pr.normalize(), nil
}

// Comments implements [forge.Client].
//
// Azure DevOps has no flat comment list: it returns threads, each carrying
// its own file and line, so the positions are copied down onto the comments
// as they are flattened.
func (c *Client) Comments(ctx context.Context, ref forge.PRRef) ([]forge.Comment, error) {
	threads, err := c.threads(ctx, ref)
	if err != nil {
		return nil, err
	}

	var out []forge.Comment
	for i := range threads {
		out = append(out, threads[i].normalize()...)
	}
	return out, nil
}

// threads lists the discussions on a pull request.
func (c *Client) threads(ctx context.Context, ref forge.PRRef) ([]commentThread, error) {
	var body struct {
		Value []commentThread `json:"value"`
		Count int             `json:"count"`
	}
	if err := c.do(ctx, http.MethodGet, c.prPath(ref, "/threads"), nil, nil, &body); err != nil {
		return nil, err
	}
	return body.Value, nil
}

// CreateReview implements [forge.Client].
//
// There is no review object on this platform: a review is a set of threads,
// posted one at a time. That has a consequence worth knowing — the findings
// appear as they are posted rather than all at once, and a failure halfway
// through leaves the ones before it standing. Posting them anyway is the
// right trade: a partial review is worth more than none, and the summary
// comment says what was examined either way.
func (c *Client) CreateReview(ctx context.Context, ref forge.PRRef, r forge.Review) error {
	var failures []error
	for _, comment := range r.Comments {
		if err := c.postInline(ctx, ref, comment); err != nil {
			failures = append(failures, fmt.Errorf("%s:%d: %w", comment.Path, comment.Line, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("azuredevops: posting %d of %d findings failed: %w",
			len(failures), len(r.Comments), errors.Join(failures...))
	}
	return nil
}

// postInline creates one thread anchored to a line of the diff.
func (c *Client) postInline(ctx context.Context, ref forge.PRRef, comment forge.InlineComment) error {
	body := comment.Body
	if comment.Suggestion != "" {
		body = withSuggestion(body, comment.Suggestion)
	}

	thread := commentThread{
		Comments: []threadComment{{Content: body, CommentType: "text"}},
		// "active" is a thread somebody still has to deal with, which is what
		// a finding is. Leaving it unset makes Azure DevOps file it as
		// unknown, and unknown threads do not show in the default filter.
		Status:        "active",
		ThreadContext: positionOf(comment),
	}
	return c.do(ctx, http.MethodPost, c.prPath(ref, "/threads"), nil, thread, nil)
}

// positionOf places a finding on the right-hand side of the diff — the file
// as it is after the change, which is the version being reviewed.
func positionOf(comment forge.InlineComment) *threadContext {
	line := comment.Line
	if line <= 0 {
		// Without a line there is nothing to anchor to, and a thread with a
		// file but no position lands at the top of the file rather than
		// failing.
		if comment.Path == "" {
			return nil
		}
		return &threadContext{FilePath: apiPath(comment.Path)}
	}

	end := comment.EndLine
	if end < line {
		end = line
	}
	return &threadContext{
		FilePath: apiPath(comment.Path),
		// Offsets count characters from 0, so starting at 1 means "the start
		// of the line" and ending at 1 on the line after the span would be
		// wrong. The end is the same line's start, which Azure DevOps reads
		// as the whole line.
		RightFileStart: &position{Line: line, Offset: 1},
		RightFileEnd:   &position{Line: end, Offset: 1},
	}
}

// withSuggestion renders a replacement the way Azure DevOps understands it.
func withSuggestion(body, suggestion string) string {
	return body + "\n\n```suggestion\n" + strings.TrimRight(suggestion, "\n") + "\n```"
}

// UpsertSummary implements [forge.Client].
//
// The marker is a hidden string in the body, which is how a later review
// finds the comment it wrote last time. Azure DevOps has no hidden comment
// property that survives a round trip reliably, so the marker lives in the
// text like it does on the other platforms.
func (c *Client) UpsertSummary(ctx context.Context, ref forge.PRRef, marker, body string) error {
	content := body + "\n\n" + marker

	threads, err := c.threads(ctx, ref)
	if err == nil {
		if threadID, commentID, ok := findMarked(threads, marker); ok {
			path := c.prPath(ref, fmt.Sprintf("/threads/%d/comments/%d", threadID, commentID))
			update := threadComment{Content: content}
			if err := c.do(ctx, http.MethodPatch, path, nil, update, nil); err == nil {
				return nil
			}
			// The comment was found but could not be updated — deleted since
			// the listing, most likely. Posting a new one is better than
			// failing the job over the shape of a summary.
		}
	}

	thread := commentThread{
		Comments: []threadComment{{Content: content, CommentType: "text"}},
		// The summary is not something anybody has to resolve.
		Status: "closed",
	}
	return c.do(ctx, http.MethodPost, c.prPath(ref, "/threads"), nil, thread, nil)
}

// findMarked locates the comment carrying the marker.
func findMarked(threads []commentThread, marker string) (threadID, commentID int, ok bool) {
	for _, t := range threads {
		if t.IsDeleted {
			continue
		}
		for _, comment := range t.Comments {
			if !comment.IsDeleted && strings.Contains(comment.Content, marker) {
				return t.ID, comment.ID, true
			}
		}
	}
	return 0, 0, false
}

// ReplyToThread implements [forge.Client].
func (c *Client) ReplyToThread(ctx context.Context, ref forge.PRRef, threadID, body string) error {
	id, err := strconv.Atoi(strings.TrimSpace(threadID))
	if err != nil {
		return fmt.Errorf("azuredevops: %q is not a thread id: %w", threadID, err)
	}

	path := c.prPath(ref, fmt.Sprintf("/threads/%d/comments", id))
	reply := threadComment{Content: body, CommentType: "text"}
	return c.do(ctx, http.MethodPost, path, nil, reply, nil)
}

// ReadFile implements [forge.Client].
//
// It takes two calls. Azure DevOps's items endpoint answers with metadata —
// including the blob's object id — and the blob endpoint answers with the
// bytes; there is no documented parameter that folds them into one. The
// undocumented includeContent exists and is not used: a settings file that
// stops being readable because an unlisted parameter changed is a worse
// outcome than one extra request on the handful of files this reads.
func (c *Client) ReadFile(ctx context.Context, ref forge.PRRef, path string) ([]byte, error) {
	query := url.Values{}
	query.Set("scopePath", apiPath(path))
	// No version descriptor: the default is the repository's default branch,
	// which is the point. Settings read from the branch under review would
	// let whoever opened the pull request decide how it gets reviewed
	// (docs/security.md).
	var item struct {
		ObjectID string `json:"objectId"`
		IsFolder bool   `json:"isFolder"`
	}
	if err := c.do(ctx, http.MethodGet, c.repoPath(ref)+"/items", query, nil, &item); err != nil {
		if notFound(err) {
			return nil, forge.ErrFileNotFound
		}
		return nil, err
	}
	if item.IsFolder || item.ObjectID == "" {
		return nil, forge.ErrFileNotFound
	}

	blobs := url.Values{}
	blobs.Set("$format", "octetstream")
	data, err := c.raw(ctx, http.MethodGet, c.repoPath(ref)+"/blobs/"+url.PathEscape(item.ObjectID), blobs, nil)
	if err != nil {
		if notFound(err) {
			return nil, forge.ErrFileNotFound
		}
		return nil, err
	}
	return data, nil
}

// notFound reports whether an error is Azure DevOps saying the thing is not
// there.
func notFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.NotFound()
}

// CloneAuth implements [forge.Client].
//
// The same credential that reads the API fetches the code, in the same
// position: a personal access token is the password of basic authentication
// with an empty user. It travels as an HTTP header rather than in the remote
// URL, so it cannot surface in git's own error output.
func (c *Client) CloneAuth(_ context.Context, _ forge.PRRef) (forge.CloneCredential, error) {
	credential := forge.CloneCredential{Username: "", Token: c.token}
	if c.bearer {
		credential.AuthHeader = "Authorization: Bearer " + c.token
		return credential, nil
	}
	credential.AuthHeader = "Authorization: Basic " + basic(c.token)
	return credential, nil
}
