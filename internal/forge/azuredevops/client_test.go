package azuredevops_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/forge/azuredevops"
)

const token = "pat-secret"

// call records one request the client made, so a test can assert on the
// address and the body rather than only on what came back.
type call struct {
	Method string
	Path   string
	Query  map[string][]string
	Body   map[string]any
	Auth   string
}

// query reads one query parameter of a recorded request.
func (c call) query(key string) string {
	if v := c.Query[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// stub is an Azure DevOps that answers with canned bodies.
//
// It is guarded because one of the clients under test is concurrent: a diff
// fetches the blobs of every changed file in parallel, so without the lock
// the recorder would be a data race rather than a record.
type stub struct {
	t         *testing.T
	mu        sync.Mutex
	responses map[string]any
	status    map[string]int
	raw       map[string][]byte
	calls     []call
}

func newStub(t *testing.T) *stub {
	t.Helper()
	return &stub{
		t:         t,
		responses: map[string]any{},
		status:    map[string]int{},
		raw:       map[string][]byte{},
	}
}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := call{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Auth: r.Header.Get("Authorization")}
	if data, _ := io.ReadAll(r.Body); len(data) > 0 {
		_ = json.Unmarshal(data, &rec.Body)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, rec)

	key := r.Method + " " + r.URL.Path
	if code, ok := s.status[key]; ok {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"message":"nope","typeKey":"GitItemNotFoundException"}`))
		return
	}
	if data, ok := s.raw[key]; ok {
		_, _ = w.Write(data)
		return
	}
	body, ok := s.responses[key]
	if !ok {
		body = map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (s *stub) client(opts ...azuredevops.Option) (*azuredevops.Client, *httptest.Server) {
	s.t.Helper()

	srv := httptest.NewServer(s)
	s.t.Cleanup(srv.Close)

	opts = append(opts, azuredevops.WithHTTPClient(srv.Client()))
	c, err := azuredevops.New(azuredevops.Config{OrganizationURL: srv.URL, Token: token}, opts...)
	if err != nil {
		s.t.Fatalf("New: %v", err)
	}
	return c, srv
}

// clientWith builds a client with a file cap, which is the one knob a diff
// against this platform really needs: every file costs two requests.
func (s *stub) clientWith(maxFiles int) (*azuredevops.Client, *httptest.Server) {
	s.t.Helper()

	srv := httptest.NewServer(s)
	s.t.Cleanup(srv.Close)

	c, err := azuredevops.New(
		azuredevops.Config{OrganizationURL: srv.URL, Token: token, MaxFiles: maxFiles},
		azuredevops.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		s.t.Fatalf("New: %v", err)
	}
	return c, srv
}

func testRef() forge.PRRef {
	return forge.PRRef{
		Platform: event.PlatformAzureDevOps,
		Owner:    "fabrikam",
		Project:  "Fabrikam",
		Repo:     "kibitz",
		Number:   42,
	}
}

const prBase = "/Fabrikam/_apis/git/repositories/kibitz/pullRequests/42"

func TestNewRejectsAnEmptyToken(t *testing.T) {
	if _, err := azuredevops.New(azuredevops.Config{OrganizationURL: "https://dev.azure.com/x"}); err == nil {
		t.Error("a client was built without a token")
	}
	if _, err := azuredevops.New(azuredevops.Config{Token: "t"}); err == nil {
		t.Error("a client was built without an organization url")
	}
}

func TestPullRequest(t *testing.T) {
	s := newStub(t)
	s.responses["GET "+prBase] = map[string]any{
		"pullRequestId":         42,
		"status":                "active",
		"title":                 "Add the SQS subscriber",
		"description":           "本文",
		"sourceRefName":         "refs/heads/topic",
		"targetRefName":         "refs/heads/main",
		"isDraft":               true,
		"createdBy":             map[string]any{"uniqueName": "a@example.com", "displayName": "Ada"},
		"lastMergeSourceCommit": map[string]any{"commitId": "abc1234"},
		"lastMergeTargetCommit": map[string]any{"commitId": "def5678"},
	}
	c, _ := s.client()

	pr, err := c.PullRequest(context.Background(), testRef())
	if err != nil {
		t.Fatalf("PullRequest: %v", err)
	}

	if pr.Number != 42 || pr.Title != "Add the SQS subscriber" {
		t.Errorf("pr = %+v", pr)
	}
	if pr.Source.Branch != "topic" || pr.Target.Branch != "main" {
		t.Errorf("branches = %s -> %s", pr.Source.Branch, pr.Target.Branch)
	}
	if pr.Source.SHA != "abc1234" {
		t.Errorf("Source.SHA = %q", pr.Source.SHA)
	}
	if !pr.Draft {
		t.Error("Draft is false")
	}
	if pr.Author.Login != "a@example.com" {
		t.Errorf("Author.Login = %q, want the sign-in address", pr.Author.Login)
	}
	// The api-version is pinned: an unpinned request gets whatever shape the
	// instance prefers today.
	if got := s.calls[0].Query["api-version"]; len(got) != 1 || got[0] != "7.1" {
		t.Errorf("api-version = %v", got)
	}
}

// A personal access token is the password of basic auth with no user, which
// is the form Azure DevOps takes. Sending it as a bearer would be rejected.
func TestPersonalAccessTokenIsBasicAuth(t *testing.T) {
	s := newStub(t)
	s.responses["GET "+prBase] = map[string]any{"pullRequestId": 42}
	c, _ := s.client()

	if _, err := c.PullRequest(context.Background(), testRef()); err != nil {
		t.Fatalf("PullRequest: %v", err)
	}

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+token))
	if s.calls[0].Auth != want {
		t.Errorf("Authorization = %q, want %q", s.calls[0].Auth, want)
	}
}

func TestEntraTokenIsABearer(t *testing.T) {
	s := newStub(t)
	s.responses["GET "+prBase] = map[string]any{"pullRequestId": 42}

	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	c, err := azuredevops.New(
		azuredevops.Config{OrganizationURL: srv.URL, Token: "entra", TokenIsBearer: true},
		azuredevops.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := c.PullRequest(context.Background(), testRef()); err != nil {
		t.Fatalf("PullRequest: %v", err)
	}
	if s.calls[0].Auth != "Bearer entra" {
		t.Errorf("Authorization = %q", s.calls[0].Auth)
	}
}

// Azure DevOps has no flat comment list. Threads carry the file and line, and
// flattening has to copy them down or every existing finding loses its place.
func TestCommentsFlattensThreads(t *testing.T) {
	s := newStub(t)
	s.responses["GET "+prBase+"/threads"] = map[string]any{
		"count": 3,
		"value": []any{
			map[string]any{
				"id": 5,
				"threadContext": map[string]any{
					"filePath":       "/internal/queue.go",
					"rightFileStart": map[string]any{"line": 88, "offset": 1},
				},
				"comments": []any{
					map[string]any{"id": 1, "content": "ctx を見ていない", "commentType": "text",
						"author": map[string]any{"uniqueName": "a@example.com"}},
				},
			},
			// A thread the service wrote: a vote, a branch update.
			map[string]any{
				"id":       6,
				"comments": []any{map[string]any{"id": 2, "content": "Ada voted", "commentType": "system"}},
			},
			// A deleted thread is gone, whatever it holds.
			map[string]any{
				"id": 7, "isDeleted": true,
				"comments": []any{map[string]any{"id": 3, "content": "消えた", "commentType": "text"}},
			},
		},
	}
	c, _ := s.client()

	comments, err := c.Comments(context.Background(), testRef())
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}

	if len(comments) != 1 {
		t.Fatalf("got %d comments, want 1: %+v", len(comments), comments)
	}
	got := comments[0]
	if got.Path != "internal/queue.go" || got.Line != 88 {
		t.Errorf("position = %s:%d, want internal/queue.go:88", got.Path, got.Line)
	}
	if got.ThreadID != "5" || got.ID != "1" {
		t.Errorf("ids = thread %q comment %q", got.ThreadID, got.ID)
	}
	if got.Author.Login != "a@example.com" {
		t.Errorf("Author.Login = %q", got.Author.Login)
	}
}

// A review is a set of threads here: there is no review object to post.
func TestCreateReviewPostsOneThreadPerFinding(t *testing.T) {
	s := newStub(t)
	c, _ := s.client()

	err := c.CreateReview(context.Background(), testRef(), forge.Review{
		CommitSHA: "abc1234",
		Comments: []forge.InlineComment{
			{Path: "internal/queue.go", Line: 88, EndLine: 92, Body: "ctx を見ていない"},
			{Path: "internal/store.go", Line: 12, Body: "直す", Suggestion: "return nil"},
		},
	})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}

	if len(s.calls) != 2 {
		t.Fatalf("made %d calls, want one per finding", len(s.calls))
	}
	for _, got := range s.calls {
		if got.Method != http.MethodPost || got.Path != prBase+"/threads" {
			t.Errorf("call = %s %s", got.Method, got.Path)
		}
	}

	first := s.calls[0].Body
	ctxBlock, _ := first["threadContext"].(map[string]any)
	if ctxBlock["filePath"] != "/internal/queue.go" {
		t.Errorf("filePath = %v, want a repository-rooted path", ctxBlock["filePath"])
	}
	start, _ := ctxBlock["rightFileStart"].(map[string]any)
	end, _ := ctxBlock["rightFileEnd"].(map[string]any)
	if start["line"] != float64(88) || end["line"] != float64(92) {
		t.Errorf("span = %v..%v, want 88..92", start["line"], end["line"])
	}
	// A finding is something somebody has to deal with; an unset status is
	// filed as unknown and hides from the default filter.
	if first["status"] != "active" {
		t.Errorf("status = %v, want active", first["status"])
	}

	body := commentContent(t, s.calls[1].Body)
	if !strings.Contains(body, "```suggestion\nreturn nil\n```") {
		t.Errorf("the suggestion was not rendered:\n%s", body)
	}
}

// A finding with no line still lands on its file rather than failing.
func TestFindingWithoutALineAnchorsToTheFile(t *testing.T) {
	s := newStub(t)
	c, _ := s.client()

	err := c.CreateReview(context.Background(), testRef(), forge.Review{
		Comments: []forge.InlineComment{{Path: "go.mod", Body: "気になる"}},
	})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}

	ctxBlock, _ := s.calls[0].Body["threadContext"].(map[string]any)
	if ctxBlock["filePath"] != "/go.mod" {
		t.Errorf("filePath = %v", ctxBlock["filePath"])
	}
	if _, ok := ctxBlock["rightFileStart"]; ok {
		t.Error("a finding with no line was given a position")
	}
}

// Posting stops at nothing: a failure on one finding still lets the rest
// through, because a partial review is worth more than none.
func TestCreateReviewReportsPartialFailure(t *testing.T) {
	s := newStub(t)
	s.status["POST "+prBase+"/threads"] = http.StatusBadRequest
	c, _ := s.client()

	err := c.CreateReview(context.Background(), testRef(), forge.Review{
		Comments: []forge.InlineComment{
			{Path: "a.go", Line: 1, Body: "x"},
			{Path: "b.go", Line: 2, Body: "y"},
		},
	})
	if err == nil {
		t.Fatal("a failed post was reported as success")
	}
	if len(s.calls) != 2 {
		t.Errorf("made %d calls, want both attempted", len(s.calls))
	}
	if !strings.Contains(err.Error(), "a.go:1") || !strings.Contains(err.Error(), "b.go:2") {
		t.Errorf("the error does not name what failed: %v", err)
	}
}

// A repeated review replaces its summary instead of stacking a new one.
func TestUpsertSummaryUpdatesTheMarkedComment(t *testing.T) {
	const marker = "<!-- kibitz:summary -->"

	s := newStub(t)
	s.responses["GET "+prBase+"/threads"] = map[string]any{
		"value": []any{
			map[string]any{"id": 9, "comments": []any{
				map[string]any{"id": 3, "content": "前回のまとめ\n\n" + marker, "commentType": "text"},
			}},
		},
	}
	c, _ := s.client()

	if err := c.UpsertSummary(context.Background(), testRef(), marker, "今回のまとめ"); err != nil {
		t.Fatalf("UpsertSummary: %v", err)
	}

	last := s.calls[len(s.calls)-1]
	if last.Method != http.MethodPatch {
		t.Fatalf("last call = %s %s, want a PATCH", last.Method, last.Path)
	}
	if last.Path != prBase+"/threads/9/comments/3" {
		t.Errorf("path = %s", last.Path)
	}
	if body := commentContent(t, last.Body); !strings.Contains(body, "今回のまとめ") || !strings.Contains(body, marker) {
		t.Errorf("content = %q", body)
	}
}

func TestUpsertSummaryCreatesTheFirstOne(t *testing.T) {
	const marker = "<!-- kibitz:summary -->"

	s := newStub(t)
	s.responses["GET "+prBase+"/threads"] = map[string]any{"value": []any{}}
	c, _ := s.client()

	if err := c.UpsertSummary(context.Background(), testRef(), marker, "はじめてのまとめ"); err != nil {
		t.Fatalf("UpsertSummary: %v", err)
	}

	last := s.calls[len(s.calls)-1]
	if last.Method != http.MethodPost || last.Path != prBase+"/threads" {
		t.Fatalf("last call = %s %s", last.Method, last.Path)
	}
	// The summary is not something anybody has to resolve.
	if last.Body["status"] != "closed" {
		t.Errorf("status = %v, want closed", last.Body["status"])
	}
}

func TestReplyToThread(t *testing.T) {
	s := newStub(t)
	c, _ := s.client()

	if err := c.ReplyToThread(context.Background(), testRef(), "5", "そうです"); err != nil {
		t.Fatalf("ReplyToThread: %v", err)
	}

	got := s.calls[0]
	if got.Method != http.MethodPost || got.Path != prBase+"/threads/5/comments" {
		t.Errorf("call = %s %s", got.Method, got.Path)
	}
	if commentContent(t, got.Body) != "そうです" {
		t.Errorf("content = %v", got.Body["content"])
	}
}

func TestReplyToThreadRejectsANonNumericID(t *testing.T) {
	s := newStub(t)
	c, _ := s.client()

	if err := c.ReplyToThread(context.Background(), testRef(), "abc", "x"); err == nil {
		t.Error("a non-numeric thread id was accepted")
	}
	if len(s.calls) != 0 {
		t.Error("a request was made with a bad thread id")
	}
}

// Two calls: the item says which blob, the blob says what is in it. There is
// no documented parameter that folds them into one.
func TestReadFile(t *testing.T) {
	s := newStub(t)
	s.responses["GET /Fabrikam/_apis/git/repositories/kibitz/items"] = map[string]any{
		"objectId": "b10b1d",
		"isFolder": false,
	}
	s.raw["GET /Fabrikam/_apis/git/repositories/kibitz/blobs/b10b1d"] = []byte("review:\n  enabled: true\n")
	c, _ := s.client()

	data, err := c.ReadFile(context.Background(), testRef(), ".kibitz.yaml")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "review:\n  enabled: true\n" {
		t.Errorf("content = %q", data)
	}

	// No version descriptor: the default branch is the point. Settings read
	// from the branch under review would let whoever opened the pull request
	// decide how it gets reviewed.
	items := s.calls[0]
	if got := items.Query["scopePath"]; len(got) != 1 || got[0] != "/.kibitz.yaml" {
		t.Errorf("scopePath = %v", got)
	}
	for _, name := range []string{"versionDescriptor.version", "versionDescriptor.versionType"} {
		if _, ok := items.Query[name]; ok {
			t.Errorf("%s was sent; the default branch is the point", name)
		}
	}
}

func TestReadFileReportsAMissingFile(t *testing.T) {
	s := newStub(t)
	s.status["GET /Fabrikam/_apis/git/repositories/kibitz/items"] = http.StatusNotFound
	c, _ := s.client()

	_, err := c.ReadFile(context.Background(), testRef(), ".kibitz.yaml")
	if !errors.Is(err, forge.ErrFileNotFound) {
		t.Errorf("err = %v, want ErrFileNotFound", err)
	}
}

func TestReadFileRejectsAFolder(t *testing.T) {
	s := newStub(t)
	s.responses["GET /Fabrikam/_apis/git/repositories/kibitz/items"] = map[string]any{
		"objectId": "tree1d", "isFolder": true,
	}
	c, _ := s.client()

	if _, err := c.ReadFile(context.Background(), testRef(), "docs"); !errors.Is(err, forge.ErrFileNotFound) {
		t.Errorf("err = %v, want ErrFileNotFound", err)
	}
}

// The credential travels as a header so it cannot surface in git's own error
// output, which is where a remote URL ends up.
func TestCloneAuthKeepsTheTokenInAHeader(t *testing.T) {
	s := newStub(t)
	c, _ := s.client()

	cred, err := c.CloneAuth(context.Background(), testRef())
	if err != nil {
		t.Fatalf("CloneAuth: %v", err)
	}
	want := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(":"+token))
	if cred.AuthHeader != want {
		t.Errorf("AuthHeader = %q", cred.AuthHeader)
	}
	if cred.Token != token {
		t.Errorf("Token = %q", cred.Token)
	}
}

// An unauthenticated request to Azure DevOps is answered with a sign-in page
// rather than a 401, so a wrong token produces HTML. Saying so is the
// difference between "the token is wrong" and an afternoon.
func TestASignInPageIsReportedAsSuch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNonAuthoritativeInfo)
		_, _ = w.Write([]byte("<html><head><title>Sign In</title></head></html>"))
	}))
	t.Cleanup(srv.Close)

	c, err := azuredevops.New(
		azuredevops.Config{OrganizationURL: srv.URL, Token: "wrong"},
		azuredevops.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.PullRequest(context.Background(), testRef())
	if err == nil || !strings.Contains(err.Error(), "sign-in page") {
		t.Errorf("err = %v, want it to name the sign-in page", err)
	}
}

func TestPlatform(t *testing.T) {
	s := newStub(t)
	c, _ := s.client()
	if c.Platform() != event.PlatformAzureDevOps {
		t.Errorf("Platform = %q", c.Platform())
	}
}

// commentContent digs the text out of a threads or comments request body,
// whichever shape it was.
func commentContent(t *testing.T, body map[string]any) string {
	t.Helper()

	if content, ok := body["content"].(string); ok {
		return content
	}
	comments, ok := body["comments"].([]any)
	if !ok || len(comments) == 0 {
		t.Fatalf("no comment in %v", body)
	}
	first, _ := comments[0].(map[string]any)
	content, _ := first["content"].(string)
	return content
}
