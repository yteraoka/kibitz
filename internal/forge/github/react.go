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

// Reactions kibitz puts on a comment. "eyes" is the one people already read
// as "looking at it"; "-1" is the plainest no GitHub's short list has.
const (
	Reaction        = "eyes"
	RefusedReaction = "-1"
)

// React implements [forge.Reactor].
//
// GitHub keeps the two kinds of comment on a pull request apart: one in the
// conversation is an issue comment, one on a line of the diff is a review
// comment, and each has its own reactions endpoint. A comment on an issue is
// an issue comment too.
//
// Reacting twice answers 200 instead of 201 and changes nothing, so a
// redelivered webhook does not stack reactions.
func (c *Client) React(ctx context.Context, ref forge.CommentRef, reaction forge.Reaction) error {
	content := Reaction
	if reaction == forge.ReactionRefused {
		content = RefusedReaction
	}
	id := strings.TrimSpace(ref.CommentID)
	if id == "" {
		return errors.New("github: no comment to react to")
	}
	kind := "issues"
	if ref.Inline {
		kind = "pulls"
	}
	repo := forge.PRRef{Owner: ref.Owner, Repo: ref.Repo}
	path := repoPath(repo, fmt.Sprintf("/%s/comments/%s/reactions", kind, url.PathEscape(id)))
	return c.do(ctx, http.MethodPost, path, map[string]string{"content": content}, nil)
}

var _ forge.Reactor = (*Client)(nil)
