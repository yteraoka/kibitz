package azuredevops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/yteraoka/kibitz/internal/forge"
)

// React implements [forge.Reactor] with a like, which is the only reaction a
// pull request comment takes on Azure DevOps:
//
//	POST …/pullRequests/{id}/threads/{threadId}/comments/{commentId}/likes
//
// From Microsoft's own specification of the 7.1 API, which also puts it at
// api-version 7.1 rather than a preview. A like from the same identity twice
// is one like.
//
// A like is all there is, so a refusal cannot be said this way: it is
// reported as not supported rather than sent as a like, which would say the
// opposite.
func (c *Client) React(ctx context.Context, ref forge.CommentRef, reaction forge.Reaction) error {
	if reaction == forge.ReactionRefused {
		return fmt.Errorf("azuredevops: %w: a pull request comment takes no reaction but a like", forge.ErrNotSupported)
	}
	if ref.OnIssue {
		// A work item is not a pull request and its discussion takes no like.
		return fmt.Errorf("azuredevops: %w: reacting to a work item comment", forge.ErrNotSupported)
	}
	thread, err := strconv.Atoi(strings.TrimSpace(ref.ThreadID))
	if err != nil {
		return errors.New("azuredevops: reacting to a comment needs its thread")
	}
	comment, err := strconv.Atoi(strings.TrimSpace(ref.CommentID))
	if err != nil {
		return errors.New("azuredevops: no comment to react to")
	}
	pr := forge.PRRef{Owner: ref.Owner, Repo: ref.Repo, Project: ref.Project, Number: ref.Number}
	path := c.prPath(pr, fmt.Sprintf("/threads/%d/comments/%d/likes", thread, comment))
	return c.do(ctx, http.MethodPost, path, nil, nil, nil)
}

var _ forge.Reactor = (*Client)(nil)
