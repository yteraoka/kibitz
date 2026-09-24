package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/yteraoka/kibitz/internal/forge"
)

// The write half of the client: opening the pull request implement mode's
// change ends up in. Nothing here is used by a review.

// FindPullRequest implements [forge.Writer].
//
// The head is qualified with the owner, which is what GitHub requires and also
// what keeps the answer to "is there a pull request for this branch" from
// matching a branch of the same name on somebody's fork.
func (c *Client) FindPullRequest(ctx context.Context, ref forge.PRRef, head string) (*forge.PullRequestInfo, error) {
	head = strings.TrimSpace(head)
	if head == "" {
		return nil, errors.New("github: no head branch given")
	}

	path := fmt.Sprintf("%s/pulls?state=open&head=%s&per_page=%d",
		repoPath(ref, ""), url.QueryEscape(ref.Owner+":"+head), pageSize)

	var found []pullRequest
	if err := c.do(ctx, http.MethodGet, path, nil, &found); err != nil {
		return nil, err
	}
	for _, pr := range found {
		// The filter is GitHub's, but it is checked again here: asking for one
		// branch and acting on another one's pull request would attach a
		// comment to a change nobody made.
		if pr.Head.Ref != head {
			continue
		}
		return &forge.PullRequestInfo{Number: pr.Number, URL: pr.HTMLURL, Draft: pr.Draft}, nil
	}
	return nil, nil
}

// CreatePullRequest implements [forge.Writer].
func (c *Client) CreatePullRequest(ctx context.Context, ref forge.PRRef, req forge.NewPullRequest) (*forge.PullRequestInfo, error) {
	if strings.TrimSpace(req.Head) == "" || strings.TrimSpace(req.Base) == "" {
		return nil, errors.New("github: a pull request needs a head and a base branch")
	}

	created, err := c.openPullRequest(ctx, ref, req)
	if err == nil {
		return created, nil
	}

	// Everything worth recovering from comes back as a 422, so the body has to
	// be read to tell which one it is.
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnprocessableEntity {
		return nil, err
	}

	// A pull request for this branch already exists. That is the ordinary
	// outcome of a redelivered job, so it is an answer and not a failure.
	if existing, findErr := c.FindPullRequest(ctx, ref, req.Head); findErr == nil && existing != nil {
		return existing, nil
	}

	// Draft pull requests are not available on every plan, and GitHub says so
	// with the same status code. An ordinary pull request is worth having:
	// refusing to open one would throw away a change that is already pushed.
	if req.Draft && mentionsDraft(apiErr.Message) {
		req.Draft = false
		return c.openPullRequest(ctx, ref, req)
	}
	return nil, err
}

func (c *Client) openPullRequest(ctx context.Context, ref forge.PRRef, req forge.NewPullRequest) (*forge.PullRequestInfo, error) {
	body := struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body,omitempty"`
		Draft bool   `json:"draft"`
	}{Title: req.Title, Head: req.Head, Base: req.Base, Body: req.Body, Draft: req.Draft}

	var pr pullRequest
	if err := c.do(ctx, http.MethodPost, repoPath(ref, "/pulls"), body, &pr); err != nil {
		return nil, err
	}
	return &forge.PullRequestInfo{Number: pr.Number, URL: pr.HTMLURL, Draft: pr.Draft}, nil
}

// mentionsDraft reports whether GitHub's complaint is about the draft flag.
//
// Matching on the message is unpleasant and it is the only signal there is:
// the status code is the same one used for a branch with no commits and for a
// base that does not exist, and those must not be retried as anything.
func mentionsDraft(message string) bool {
	return strings.Contains(strings.ToLower(message), "draft")
}

var _ forge.Writer = (*Client)(nil)
