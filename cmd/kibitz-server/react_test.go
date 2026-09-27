package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/config"
	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
)

func testKey(t *testing.T) config.Secret {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	return config.Secret(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

// The token the server mints can react and do nothing more. This is the claim
// ADR-0020 rests on, so it is checked against what actually goes over the
// wire rather than against the configuration.
func TestTheServersGitHubTokenCannotPush(t *testing.T) {
	var asked struct {
		Permissions map[string]string `json:"permissions"`
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/456/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&asked)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "ghs_narrow", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	mux.HandleFunc("POST /repos/acme/web/issues/comments/1/reactions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	reactors, err := newReactors(config.Reactions{
		Enabled: true,
		Forges: config.Forges{GitHub: config.GitHubApp{
			AppID: 123, InstallationID: 456, PrivateKey: testKey(t), BaseURL: server.URL,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reactor, ok := reactors[event.PlatformGitHub]
	if !ok {
		t.Fatal("no GitHub reactor")
	}
	if err := reactor.React(context.Background(), forge.CommentRef{Owner: "acme", Repo: "web", Number: 1, CommentID: "1"}); err != nil {
		t.Fatalf("React: %v", err)
	}

	want := map[string]string{"issues": "write", "pull_requests": "write"}
	if !maps.Equal(asked.Permissions, want) {
		t.Errorf("the token asked for %v, want exactly %v", asked.Permissions, want)
	}
}

func TestReactorsOnlyForConfiguredPlatforms(t *testing.T) {
	forges := config.Forges{
		GitLab: config.GitLabAuth{Token: "glpat-x"},
	}

	reactors, err := newReactors(config.Reactions{Enabled: true, Forges: forges})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reactors[event.PlatformGitLab]; !ok || len(reactors) != 1 {
		t.Errorf("reactors for %v, want GitLab only", slices.Collect(maps.Keys(reactors)))
	}

	off, err := newReactors(config.Reactions{Enabled: false, Forges: forges})
	if err != nil {
		t.Fatal(err)
	}
	if len(off) != 0 {
		t.Errorf("reactors for %v with reactions off", slices.Collect(maps.Keys(off)))
	}
}
