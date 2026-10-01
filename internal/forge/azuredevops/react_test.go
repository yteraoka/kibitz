package azuredevops_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
)

func TestReactLikesTheCommentInItsThread(t *testing.T) {
	s := newStub(t)
	c, _ := s.client()

	err := c.React(context.Background(), forge.CommentRef{
		Platform: event.PlatformAzureDevOps, Owner: "fabrikam", Project: "Fabrikam", Repo: "kibitz",
		Number: 42, ThreadID: "7", CommentID: "3",
	}, forge.ReactionSeen)
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	if len(s.calls) != 1 {
		t.Fatalf("%d calls, want 1", len(s.calls))
	}
	got := s.calls[0]
	if got.Method != "POST" || !strings.HasSuffix(got.Path, "/pullRequests/42/threads/7/comments/3/likes") {
		t.Errorf("%s %s", got.Method, got.Path)
	}
	if v := got.Query["api-version"]; len(v) != 1 || v[0] != "7.1" {
		t.Errorf("api-version = %q, want 7.1 as Microsoft's specification gives it", v)
	}
}

func TestReactNeedsTheThread(t *testing.T) {
	s := newStub(t)
	c, _ := s.client()
	if err := c.React(context.Background(), forge.CommentRef{Number: 42, CommentID: "3"}, forge.ReactionSeen); err == nil {
		t.Error("React returned nil without a thread")
	}
	if len(s.calls) != 0 {
		t.Error("a request was made without a thread")
	}
}

func TestReactDoesNotLikeWorkItems(t *testing.T) {
	s := newStub(t)
	c, _ := s.client()
	err := c.React(context.Background(), forge.CommentRef{Number: 5, ThreadID: "1", CommentID: "1", OnIssue: true}, forge.ReactionSeen)
	if !errors.Is(err, forge.ErrNotSupported) {
		t.Errorf("error = %v, want ErrNotSupported", err)
	}
}

// A like is the only reaction there is, and a refusal sent as a like would say
// the opposite. It is reported as unsupported and nothing is sent.
func TestReactRefusalIsNotSupported(t *testing.T) {
	s := newStub(t)
	c, _ := s.client()
	err := c.React(context.Background(), forge.CommentRef{Number: 42, ThreadID: "7", CommentID: "3"}, forge.ReactionRefused)
	if !errors.Is(err, forge.ErrNotSupported) {
		t.Fatalf("err = %v, want ErrNotSupported", err)
	}
	if len(s.calls) != 0 {
		t.Errorf("%d calls, want none", len(s.calls))
	}
}
