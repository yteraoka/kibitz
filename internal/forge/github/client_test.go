package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	ghforge "github.com/yteraoka/kibitz/internal/forge/github"
)

var ref = forge.PRRef{Platform: event.PlatformGitHub, Owner: "yteraoka", Repo: "kibitz", Number: 42}

func testKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

// fakeGitHub records requests and serves canned responses.
type fakeGitHub struct {
	t         *testing.T
	server    *httptest.Server
	mux       *http.ServeMux
	tokenHits atomic.Int32
	requests  []recorded
}

type recorded struct {
	Method string
	Path   string
	Query  string
	Body   map[string]any
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()

	f := &fakeGitHub{t: t, mux: http.NewServeMux()}
	f.mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("token request Authorization = %q, want a bearer JWT", got)
		}
		f.tokenHits.Add(1)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_installationtoken",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "access_tokens") {
			rec := recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery}
			if r.Body != nil {
				_ = json.NewDecoder(r.Body).Decode(&rec.Body)
			}
			f.requests = append(f.requests, rec)

			if got := r.Header.Get("Authorization"); got != "Bearer ghs_installationtoken" {
				t.Errorf("Authorization = %q, want the installation token", got)
			}
		}
		f.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGitHub) handle(pattern string, status int, body any) {
	f.mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	})
}

func (f *fakeGitHub) client(t *testing.T) *ghforge.Client {
	t.Helper()
	c, err := ghforge.New(ghforge.Config{
		AppID:          123,
		InstallationID: 456,
		PrivateKey:     testKey(t),
		BaseURL:        f.server.URL,
	}, ghforge.WithHTTPClient(f.server.Client()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func (f *fakeGitHub) lastRequest() recorded {
	f.t.Helper()
	if len(f.requests) == 0 {
		f.t.Fatal("no requests were recorded")
	}
	return f.requests[len(f.requests)-1]
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	key := testKey(t)

	if _, err := ghforge.New(ghforge.Config{InstallationID: 1, PrivateKey: key}); err == nil {
		t.Error("New accepted a config with no app id")
	}
	if _, err := ghforge.New(ghforge.Config{AppID: 1, PrivateKey: key}); err == nil {
		t.Error("New accepted a config with no installation id")
	}
	if _, err := ghforge.New(ghforge.Config{AppID: 1, InstallationID: 1, PrivateKey: "not a key"}); err == nil {
		t.Error("New accepted a malformed private key")
	}
}

func TestPrivateKeyFormats(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	// A key that made a detour through a secret manager often comes back as
	// PKCS#8 rather than the PKCS#1 GitHub hands out.
	encoded := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))
	if _, err := ghforge.New(ghforge.Config{AppID: 1, InstallationID: 1, PrivateKey: encoded}); err != nil {
		t.Errorf("New rejected a PKCS#8 key: %v", err)
	}
}

func TestInstallationTokenIsCached(t *testing.T) {
	f := newFakeGitHub(t)
	f.handle("GET /repos/yteraoka/kibitz/pulls/42", http.StatusOK, map[string]any{"number": 42})
	c := f.client(t)

	for range 3 {
		if _, err := c.PullRequest(context.Background(), ref); err != nil {
			t.Fatalf("PullRequest: %v", err)
		}
	}
	if got := f.tokenHits.Load(); got != 1 {
		t.Errorf("%d token requests, want 1: the installation token should be cached", got)
	}
}

func TestPullRequest(t *testing.T) {
	f := newFakeGitHub(t)
	f.handle("GET /repos/yteraoka/kibitz/pulls/42", http.StatusOK, map[string]any{
		"id": 998877, "number": 42, "title": "Add the SQS subscriber",
		"body": "why", "state": "open", "draft": false,
		"html_url": "https://github.com/yteraoka/kibitz/pull/42",
		"user":     map[string]any{"id": 1, "login": "yteraoka"},
		"head": map[string]any{
			"ref": "feat/sqs", "sha": "abc123",
			"repo": map[string]any{"full_name": "contributor/kibitz"},
		},
		"base": map[string]any{
			"ref": "main", "sha": "def456",
			"repo": map[string]any{"full_name": "yteraoka/kibitz"},
		},
		"changed_files": 12,
	})

	pr, err := f.client(t).PullRequest(context.Background(), ref)
	if err != nil {
		t.Fatalf("PullRequest: %v", err)
	}
	if pr.Number != 42 || pr.Title != "Add the SQS subscriber" {
		t.Errorf("pull request = %+v", pr)
	}
	if pr.Source.SHA != "abc123" || pr.Target.Branch != "main" {
		t.Errorf("refs = %+v / %+v", pr.Source, pr.Target)
	}
	// The head lives in another repository, so this is a fork.
	if !pr.IsFork {
		t.Error("IsFork = false, want true")
	}
}

func TestDiffPaginates(t *testing.T) {
	f := newFakeGitHub(t)

	page := 0
	f.mux.HandleFunc("GET /repos/yteraoka/kibitz/pulls/42/files", func(w http.ResponseWriter, r *http.Request) {
		page++
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
		if page == 1 {
			files := make([]map[string]any, 100)
			for i := range files {
				files[i] = map[string]any{
					"filename": fmt.Sprintf("file%d.go", i), "status": "modified",
					"additions": 1, "deletions": 1, "patch": "@@ -1 +1 @@",
				}
			}
			_ = json.NewEncoder(w).Encode(files)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"filename": "last.go", "status": "added", "additions": 5, "deletions": 0, "patch": "@@ -0,0 +1,5 @@"},
		})
	})

	diff, err := f.client(t).Diff(context.Background(), ref)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(diff.Files) != 101 {
		t.Fatalf("%d files, want 101 across two pages", len(diff.Files))
	}
	if diff.Truncated {
		t.Error("Truncated = true, want false")
	}
	if got := diff.Lines(); got != 205 {
		t.Errorf("Lines() = %d, want 205", got)
	}
}

func TestDiffTruncatesAtMaxFiles(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("GET /repos/yteraoka/kibitz/pulls/42/files", func(w http.ResponseWriter, _ *http.Request) {
		files := make([]map[string]any, 100)
		for i := range files {
			files[i] = map[string]any{"filename": fmt.Sprintf("file%d.go", i), "status": "modified"}
		}
		_ = json.NewEncoder(w).Encode(files)
	})

	c, err := ghforge.New(ghforge.Config{
		AppID: 1, InstallationID: 1, PrivateKey: testKey(t),
		BaseURL: f.server.URL, MaxFiles: 10,
	}, ghforge.WithHTTPClient(f.server.Client()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	diff, err := c.Diff(context.Background(), ref)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(diff.Files) != 10 {
		t.Errorf("%d files, want the configured maximum of 10", len(diff.Files))
	}
	if !diff.Truncated {
		t.Error("Truncated = false, want true")
	}
}

func TestComments(t *testing.T) {
	f := newFakeGitHub(t)
	f.handle("GET /repos/yteraoka/kibitz/issues/42/comments", http.StatusOK, []map[string]any{
		{"id": 1, "body": "looks good", "user": map[string]any{"login": "yteraoka"}},
	})
	f.handle("GET /repos/yteraoka/kibitz/pulls/42/comments", http.StatusOK, []map[string]any{
		{"id": 2, "body": "this leaks", "path": "main.go", "line": 10,
			"user": map[string]any{"login": "reviewer"}},
		{"id": 3, "body": "agreed", "path": "main.go", "line": 10, "in_reply_to_id": 2,
			"user": map[string]any{"login": "yteraoka"}},
	})

	comments, err := f.client(t).Comments(context.Background(), ref)
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(comments) != 3 {
		t.Fatalf("%d comments, want 3", len(comments))
	}
	// A reply belongs to the thread it answers, which is how kibitz knows
	// which discussion it is joining.
	if comments[2].ThreadID != "2" {
		t.Errorf("reply thread = %q, want 2", comments[2].ThreadID)
	}
	if comments[1].Path != "main.go" || comments[1].Line != 10 {
		t.Errorf("review comment position = %s:%d", comments[1].Path, comments[1].Line)
	}
}

func TestCreateReview(t *testing.T) {
	f := newFakeGitHub(t)
	f.handle("POST /repos/yteraoka/kibitz/pulls/42/reviews", http.StatusOK, map[string]any{"id": 1})

	err := f.client(t).CreateReview(context.Background(), ref, forge.Review{
		Summary:   "2 findings",
		CommitSHA: "abc123",
		Comments: []forge.InlineComment{
			{Path: "main.go", Line: 10, Body: "leaks a goroutine"},
			{Path: "queue.go", Line: 20, EndLine: 24, Body: "narrow this", Suggestion: "case <-ctx.Done():\n\treturn"},
		},
	})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}

	body := f.lastRequest().Body
	if body["event"] != "COMMENT" {
		t.Errorf("event = %v, want COMMENT: kibitz must not approve or request changes", body["event"])
	}
	if body["commit_id"] != "abc123" {
		t.Errorf("commit_id = %v", body["commit_id"])
	}

	comments, ok := body["comments"].([]any)
	if !ok || len(comments) != 2 {
		t.Fatalf("comments = %v, want 2", body["comments"])
	}

	single := comments[0].(map[string]any)
	if single["line"].(float64) != 10 {
		t.Errorf("single-line comment line = %v", single["line"])
	}
	if _, present := single["start_line"]; present {
		t.Error("a single-line comment must not carry start_line")
	}

	multi := comments[1].(map[string]any)
	if multi["start_line"].(float64) != 20 || multi["line"].(float64) != 24 {
		t.Errorf("multi-line comment span = %v..%v", multi["start_line"], multi["line"])
	}
	if !strings.Contains(multi["body"].(string), "```suggestion") {
		t.Errorf("suggestion was not rendered: %v", multi["body"])
	}
}

func TestUpsertSummaryCreatesThenUpdates(t *testing.T) {
	const marker = "<!-- kibitz:summary -->"

	t.Run("creates when absent", func(t *testing.T) {
		f := newFakeGitHub(t)
		f.handle("GET /repos/yteraoka/kibitz/issues/42/comments", http.StatusOK, []map[string]any{
			{"id": 1, "body": "unrelated", "user": map[string]any{"login": "yteraoka"}},
		})
		f.handle("POST /repos/yteraoka/kibitz/issues/42/comments", http.StatusCreated, map[string]any{"id": 2})

		if err := f.client(t).UpsertSummary(context.Background(), ref, marker, "first review"); err != nil {
			t.Fatalf("UpsertSummary: %v", err)
		}
		last := f.lastRequest()
		if last.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", last.Method)
		}
		if !strings.Contains(last.Body["body"].(string), marker) {
			t.Error("the marker was not written into the comment")
		}
	})

	t.Run("updates its own comment", func(t *testing.T) {
		f := newFakeGitHub(t)
		f.handle("GET /repos/yteraoka/kibitz/issues/42/comments", http.StatusOK, []map[string]any{
			{"id": 1, "body": "unrelated", "user": map[string]any{"login": "yteraoka"}},
			{"id": 7, "body": marker + "\nprevious review", "user": map[string]any{"login": "kibitz[bot]"}},
		})
		f.handle("PATCH /repos/yteraoka/kibitz/issues/comments/7", http.StatusOK, map[string]any{"id": 7})

		if err := f.client(t).UpsertSummary(context.Background(), ref, marker, "second review"); err != nil {
			t.Fatalf("UpsertSummary: %v", err)
		}
		last := f.lastRequest()
		if last.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH: a repeated review replaces its summary", last.Method)
		}
		if !strings.Contains(last.Path, "/comments/7") {
			t.Errorf("path = %s, want the existing comment", last.Path)
		}
	})
}

func TestReplyToThread(t *testing.T) {
	t.Run("answers a review thread", func(t *testing.T) {
		f := newFakeGitHub(t)
		f.handle("POST /repos/yteraoka/kibitz/pulls/42/comments/666/replies", http.StatusCreated, map[string]any{"id": 1})

		if err := f.client(t).ReplyToThread(context.Background(), ref, "666", "because the lease expires"); err != nil {
			t.Fatalf("ReplyToThread: %v", err)
		}
		if got := f.lastRequest().Path; !strings.HasSuffix(got, "/comments/666/replies") {
			t.Errorf("path = %s", got)
		}
	})

	t.Run("falls back to a conversation comment", func(t *testing.T) {
		f := newFakeGitHub(t)
		f.handle("POST /repos/yteraoka/kibitz/issues/42/comments", http.StatusCreated, map[string]any{"id": 1})

		if err := f.client(t).ReplyToThread(context.Background(), ref, "", "answer"); err != nil {
			t.Fatalf("ReplyToThread: %v", err)
		}
		if got := f.lastRequest().Path; !strings.HasSuffix(got, "/issues/42/comments") {
			t.Errorf("path = %s, want a conversation comment", got)
		}
	})
}

func TestCloneAuthKeepsTheTokenOutOfTheURL(t *testing.T) {
	f := newFakeGitHub(t)

	cred, err := f.client(t).CloneAuth(context.Background(), ref)
	if err != nil {
		t.Fatalf("CloneAuth: %v", err)
	}
	if cred.Username != "x-access-token" {
		t.Errorf("username = %q", cred.Username)
	}
	want := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_installationtoken"))
	if cred.AuthHeader != want {
		t.Errorf("auth header = %q", cred.AuthHeader)
	}
}

func TestAPIErrors(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		headers   map[string]string
		temporary bool
	}{
		{name: "not found", status: http.StatusNotFound},
		{name: "unprocessable", status: http.StatusUnprocessableEntity},
		{name: "server error", status: http.StatusInternalServerError, temporary: true},
		{
			name:      "rate limited",
			status:    http.StatusTooManyRequests,
			headers:   map[string]string{"Retry-After": "30"},
			temporary: true,
		},
		{
			name:      "secondary rate limit",
			status:    http.StatusForbidden,
			headers:   map[string]string{"Retry-After": "60"},
			temporary: true,
		},
		{name: "forbidden", status: http.StatusForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.mux.HandleFunc("GET /repos/yteraoka/kibitz/pulls/42", func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "nope"})
			})

			_, err := f.client(t).PullRequest(context.Background(), ref)
			var apiErr *ghforge.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want an APIError", err)
			}
			if apiErr.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", apiErr.StatusCode, tc.status)
			}
			if apiErr.Temporary() != tc.temporary {
				t.Errorf("Temporary() = %v, want %v", apiErr.Temporary(), tc.temporary)
			}
			if apiErr.Message != "nope" {
				t.Errorf("message = %q, want GitHub's own", apiErr.Message)
			}
		})
	}
}

func TestRefOf(t *testing.T) {
	ev := &event.ReviewEvent{
		Source:      event.Source{Platform: event.PlatformGitHub},
		Repository:  event.Repository{Owner: "yteraoka", Name: "kibitz", FullName: "yteraoka/kibitz"},
		PullRequest: &event.PullRequest{Number: 42},
	}
	got := forge.RefOf(ev)
	if got != ref {
		t.Errorf("RefOf = %+v, want %+v", got, ref)
	}
	if got.String() != "yteraoka/kibitz#42" {
		t.Errorf("String() = %q", got.String())
	}
}

// BotLogin asks GitHub what the app is called rather than being told, so that
// a hand-written account name cannot be quietly wrong.
func TestBotLogin(t *testing.T) {
	var path, auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 123, "slug": "kibitz", "name": "kibitz"})
	}))
	defer server.Close()

	c, err := ghforge.New(ghforge.Config{
		AppID: 123, InstallationID: 456, PrivateKey: testKey(t), BaseURL: server.URL,
	}, ghforge.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	login, err := c.BotLogin(context.Background())
	if err != nil {
		t.Fatalf("BotLogin: %v", err)
	}
	if login != "kibitz[bot]" {
		t.Errorf("login = %q, want kibitz[bot]", login)
	}
	if path != "/app" {
		t.Errorf("path = %q, want /app", path)
	}
	// The app describes itself; no installation is entitled to ask, so this
	// one call authenticates with the app JWT rather than a token.
	if !strings.HasPrefix(auth, "Bearer ey") {
		t.Errorf("Authorization = %q, want the app JWT", auth)
	}
}

func TestBotLoginFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   any
	}{
		{name: "not found", status: http.StatusNotFound, body: map[string]any{"message": "Not Found"}},
		{name: "no slug", status: http.StatusOK, body: map[string]any{"id": 123}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(tc.body)
			}))
			defer server.Close()

			c, err := ghforge.New(ghforge.Config{
				AppID: 123, InstallationID: 456, PrivateKey: testKey(t), BaseURL: server.URL,
			}, ghforge.WithHTTPClient(server.Client()))
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			if _, err := c.BotLogin(context.Background()); err == nil {
				t.Error("BotLogin succeeded, want an error")
			}
		})
	}
}

func TestReadFileReadsTheDefaultBranch(t *testing.T) {
	f := newFakeGitHub(t)
	// The API wraps its base64 at 60 columns, which a strict decoder rejects.
	encoded := base64.StdEncoding.EncodeToString([]byte("version: 1\nreview:\n  language: ja\n"))
	f.handle("GET /repos/yteraoka/kibitz/contents/.kibitz.yaml", http.StatusOK, map[string]any{
		"type": "file", "encoding": "base64", "size": 33,
		"content": encoded[:8] + "\n" + encoded[8:],
	})

	got, err := f.client(t).ReadFile(context.Background(), ref, ".kibitz.yaml")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if want := "version: 1\nreview:\n  language: ja\n"; string(got) != want {
		t.Errorf("ReadFile = %q, want %q", got, want)
	}
	// No ref: that is what makes GitHub answer from the default branch, and
	// the default branch is the only one these settings may come from.
	if q := f.lastRequest().Query; q != "" {
		t.Errorf("query = %q, want none so that the default branch answers", q)
	}
}

// A repository without a settings file is the ordinary case, not a failure.
func TestReadFileReportsAMissingFile(t *testing.T) {
	f := newFakeGitHub(t)
	f.handle("GET /repos/yteraoka/kibitz/contents/.kibitz.yaml", http.StatusNotFound,
		map[string]any{"message": "Not Found"})

	_, err := f.client(t).ReadFile(context.Background(), ref, ".kibitz.yaml")
	if !errors.Is(err, forge.ErrFileNotFound) {
		t.Errorf("err = %v, want ErrFileNotFound", err)
	}
}

// Over a megabyte GitHub answers with the metadata and no content, which would
// otherwise decode to an empty file and read as "no settings at all".
func TestReadFileRefusesWhatItCannotDecode(t *testing.T) {
	f := newFakeGitHub(t)
	f.handle("GET /repos/yteraoka/kibitz/contents/big.yaml", http.StatusOK, map[string]any{
		"type": "file", "encoding": "none", "content": "", "size": 2 << 20,
	})

	if _, err := f.client(t).ReadFile(context.Background(), ref, "big.yaml"); err == nil {
		t.Error("a file too large to read came back as empty")
	}
}
