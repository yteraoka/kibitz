package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/forge"
	ghforge "github.com/yteraoka/kibitz/internal/forge/github"
)

func TestReactUsesTheEndpointForTheKindOfComment(t *testing.T) {
	for name, tc := range map[string]struct {
		ref  forge.CommentRef
		path string
	}{
		"conversation": {forge.CommentRef{Owner: "yteraoka", Repo: "kibitz", Number: 42, CommentID: "111"},
			"/repos/yteraoka/kibitz/issues/comments/111/reactions"},
		"inline": {forge.CommentRef{Owner: "yteraoka", Repo: "kibitz", Number: 42, CommentID: "222", Inline: true},
			"/repos/yteraoka/kibitz/pulls/comments/222/reactions"},
		"issue": {forge.CommentRef{Owner: "yteraoka", Repo: "kibitz", Number: 7, CommentID: "333", OnIssue: true},
			"/repos/yteraoka/kibitz/issues/comments/333/reactions"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.handle("POST "+tc.path, http.StatusCreated, map[string]any{"id": 1, "content": "eyes"})

			if err := f.client(t).React(context.Background(), tc.ref); err != nil {
				t.Fatalf("React: %v", err)
			}
			got := f.lastRequest()
			if got.Path != tc.path {
				t.Errorf("path = %s, want %s", got.Path, tc.path)
			}
			if got.Body["content"] != "eyes" {
				t.Errorf("content = %v, want eyes", got.Body["content"])
			}
		})
	}
}

// Reacting again answers 200 rather than 201. A redelivered webhook must not
// turn that into an error.
func TestReactingTwiceIsNotAnError(t *testing.T) {
	f := newFakeGitHub(t)
	f.handle("POST /repos/yteraoka/kibitz/issues/comments/111/reactions", http.StatusOK, map[string]any{"id": 1})
	if err := f.client(t).React(context.Background(), forge.CommentRef{Owner: "yteraoka", Repo: "kibitz", CommentID: "111"}); err != nil {
		t.Errorf("React: %v", err)
	}
}

// The webhook server's tokens are minted with only the permissions asked for.
// That is what keeps a token the public-facing server holds from being able
// to push a commit (ADR-0020).
func TestTokensAreNarrowedWhenPermissionsAreGiven(t *testing.T) {
	var asked map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/456/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&asked)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "ghs_narrow", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	mux.HandleFunc("POST /repos/yteraoka/kibitz/issues/comments/1/reactions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, err := ghforge.New(ghforge.Config{
		AppID: 123, InstallationID: 456, PrivateKey: testKey(t), BaseURL: server.URL,
		Permissions: map[string]string{"issues": "write", "pull_requests": "write"},
	}, ghforge.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.React(context.Background(), forge.CommentRef{Owner: "yteraoka", Repo: "kibitz", CommentID: "1"}); err != nil {
		t.Fatalf("React: %v", err)
	}

	permissions, _ := asked["permissions"].(map[string]any)
	if len(permissions) != 2 || permissions["issues"] != "write" || permissions["pull_requests"] != "write" {
		t.Errorf("the token was asked for %v, want exactly issues and pull_requests", asked)
	}
	if _, ok := permissions["contents"]; ok {
		t.Error("the token was asked for contents, which is what pushes a commit")
	}
}

// Without permissions the request carries no body, which is how every token
// was minted before and what the worker still does.
func TestTokensAreNotNarrowedByDefault(t *testing.T) {
	var body []byte
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/456/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		body = buf[:n]
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "ghs_full", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	mux.HandleFunc("POST /repos/yteraoka/kibitz/issues/comments/1/reactions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, err := ghforge.New(ghforge.Config{AppID: 123, InstallationID: 456, PrivateKey: testKey(t), BaseURL: server.URL},
		ghforge.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.React(context.Background(), forge.CommentRef{Owner: "yteraoka", Repo: "kibitz", CommentID: "1"}); err != nil {
		t.Fatalf("React: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("the token request carried %q; the worker's tokens must stay as they were", body)
	}
}
