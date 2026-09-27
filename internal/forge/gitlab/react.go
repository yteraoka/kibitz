package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/yteraoka/kibitz/internal/forge"
)

// Reaction is the emoji kibitz awards a note to say it has seen it.
const Reaction = "eyes"

// React implements [forge.Reactor] with an emoji reaction on the note, which
// GitLab calls award emoji:
//
//	POST /projects/:id/merge_requests/:iid/notes/:note_id/award_emoji?name=eyes
//	POST /projects/:id/issues/:iid/notes/:note_id/award_emoji?name=eyes
//
// Taken from GitLab's own API documentation (doc/api/emoji_reactions.md).
func (c *Client) React(ctx context.Context, ref forge.CommentRef) error {
	id := strings.TrimSpace(ref.CommentID)
	if id == "" {
		return errors.New("gitlab: no note to react to")
	}
	project := projectPath(forge.PRRef{Owner: ref.Owner, Repo: ref.Repo})
	kind := "merge_requests"
	if ref.OnIssue {
		kind = "issues"
	}
	path := fmt.Sprintf("%s/%s/%d/notes/%s/award_emoji?name=%s",
		project, kind, ref.Number, url.PathEscape(id), url.QueryEscape(Reaction))

	err := c.do(ctx, http.MethodPost, path, nil, nil)
	// An emoji the same user already awarded is refused with a message saying
	// so. That is the outcome asked for, and a redelivered webhook must not
	// be reported as a failure because of it. The documentation does not say
	// which status carries it, so the message is what is checked.
	var apiErr *APIError
	if errors.As(err, &apiErr) && strings.Contains(strings.ToLower(apiErr.Message), "already been taken") {
		return nil
	}
	return err
}

var _ forge.Reactor = (*Client)(nil)
