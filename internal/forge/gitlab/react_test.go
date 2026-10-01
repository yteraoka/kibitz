package gitlab_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/yteraoka/kibitz/internal/forge"
)

func TestReactAwardsAnEmojiToTheNote(t *testing.T) {
	for name, tc := range map[string]struct {
		ref  forge.CommentRef
		path string
	}{
		"merge request": {forge.CommentRef{Owner: "yteraoka", Repo: "kibitz", Number: 16, CommentID: "501"},
			"/api/v4/projects/yteraoka/kibitz/merge_requests/16/notes/501/award_emoji"},
		"issue": {forge.CommentRef{Owner: "yteraoka", Repo: "kibitz", Number: 3, CommentID: "502", OnIssue: true},
			"/api/v4/projects/yteraoka/kibitz/issues/3/notes/502/award_emoji"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.mux.HandleFunc("POST /", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
			})
			if err := f.client(t).React(context.Background(), tc.ref, forge.ReactionSeen); err != nil {
				t.Fatalf("React: %v", err)
			}
			got := f.lastRequest()
			// The project is URL-encoded in the request and decoded by the
			// server, so this is the decoded form.
			if got.Path != tc.path {
				t.Errorf("path = %s, want %s", got.Path, tc.path)
			}
			if got.Query != "name=eyes" {
				t.Errorf("query = %q, want name=eyes", got.Query)
			}
		})
	}
}

// An emoji already awarded is refused with a message saying so; that is the
// result that was asked for.
func TestReactingTwiceIsNotAnError(t *testing.T) {
	f := newFake(t)
	f.handle("POST /", http.StatusNotFound, map[string]any{"message": "404 Award Emoji Name has already been taken"})
	if err := f.client(t).React(context.Background(), forge.CommentRef{Owner: "yteraoka", Repo: "kibitz", Number: 16, CommentID: "501"}, forge.ReactionSeen); err != nil {
		t.Errorf("React: %v", err)
	}
}

// Any other refusal stays a refusal.
func TestAReactionThatIsRefusedIsAnError(t *testing.T) {
	f := newFake(t)
	f.handle("POST /", http.StatusForbidden, map[string]any{"message": "403 Forbidden"})
	if err := f.client(t).React(context.Background(), forge.CommentRef{Owner: "yteraoka", Repo: "kibitz", Number: 16, CommentID: "501"}, forge.ReactionSeen); err == nil {
		t.Error("React returned nil for a 403")
	}
}

// A refusal is the no-entry sign on the same note.
func TestReactRefusal(t *testing.T) {
	f := newFake(t)
	f.mux.HandleFunc("POST /", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	ref := forge.CommentRef{Owner: "yteraoka", Repo: "kibitz", Number: 16, CommentID: "501"}
	if err := f.client(t).React(context.Background(), ref, forge.ReactionRefused); err != nil {
		t.Fatalf("React: %v", err)
	}
	if got := f.lastRequest().Query; got != "name=no_entry_sign" {
		t.Errorf("query = %q, want name=no_entry_sign", got)
	}
}
