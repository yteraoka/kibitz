package forge_test

import (
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
)

func TestCommentOf(t *testing.T) {
	repo := event.Repository{Owner: "acme", Name: "web", Project: "proj", FullName: "acme/web"}

	t.Run("a comment on an issue", func(t *testing.T) {
		ev := &event.ReviewEvent{
			Source:     event.Source{Platform: event.PlatformGitLab},
			Repository: repo,
			Issue:      &event.Issue{Number: 7},
			Comment:    &event.Comment{ID: "501", ThreadID: "d1"},
		}
		ref, ok := forge.CommentOf(ev)
		if !ok {
			t.Fatal("no comment found")
		}
		want := forge.CommentRef{
			Platform: event.PlatformGitLab, Owner: "acme", Repo: "web", Project: "proj",
			Number: 7, OnIssue: true, CommentID: "501", ThreadID: "d1",
		}
		if ref != want {
			t.Errorf("got %+v\nwant %+v", ref, want)
		}
	})

	t.Run("a comment on a line of the diff", func(t *testing.T) {
		ev := &event.ReviewEvent{
			Repository:  repo,
			PullRequest: &event.PullRequest{Number: 3},
			Comment:     &event.Comment{ID: "9", Path: "main.go", Line: 4},
		}
		ref, ok := forge.CommentOf(ev)
		if !ok || !ref.Inline || ref.OnIssue || ref.Number != 3 {
			t.Errorf("got %+v, %v", ref, ok)
		}
	})

	for name, ev := range map[string]*event.ReviewEvent{
		"no event":             nil,
		"no comment":           {Repository: repo, PullRequest: &event.PullRequest{Number: 3}},
		"a comment with no id": {Repository: repo, PullRequest: &event.PullRequest{Number: 3}, Comment: &event.Comment{}},
		"nothing it is on":     {Repository: repo, Comment: &event.Comment{ID: "9"}},
	} {
		t.Run(name, func(t *testing.T) {
			if ref, ok := forge.CommentOf(ev); ok {
				t.Errorf("found %+v", ref)
			}
		})
	}
}
