package gitlab_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	glforge "github.com/yteraoka/kibitz/internal/forge/gitlab"
)

const token = "glpat-test"

var ref = forge.PRRef{Platform: event.PlatformGitLab, Owner: "yteraoka", Repo: "kibitz", Number: 16}

// fakeGitLab records requests and serves canned responses.
type fakeGitLab struct {
	t        *testing.T
	server   *httptest.Server
	mux      *http.ServeMux
	requests []recorded
}

type recorded struct {
	Method string
	Path   string
	Query  string
	Body   map[string]any
}

func newFake(t *testing.T) *fakeGitLab {
	t.Helper()

	f := &fakeGitLab{t: t, mux: http.NewServeMux()}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&rec.Body)
		}
		f.requests = append(f.requests, rec)

		// Every call authenticates the same way.
		if got := r.Header.Get("PRIVATE-TOKEN"); got != token {
			t.Errorf("PRIVATE-TOKEN = %q, want the access token", got)
		}
		f.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGitLab) handle(pattern string, status int, body any) {
	f.mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	})
}

func (f *fakeGitLab) client(t *testing.T) *glforge.Client {
	t.Helper()
	c, err := glforge.New(glforge.Config{BaseURL: f.server.URL, Token: token},
		glforge.WithHTTPClient(f.server.Client()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func (f *fakeGitLab) lastRequest() recorded {
	f.t.Helper()
	if len(f.requests) == 0 {
		f.t.Fatal("no requests were recorded")
	}
	return f.requests[len(f.requests)-1]
}

// The project is one URL segment, however many groups it is nested in.
func TestProjectPathIsEncoded(t *testing.T) {
	f := newFake(t)
	f.handle("/api/v4/projects/group%2Fsub%2Fkibitz/merge_requests/16", http.StatusOK,
		map[string]any{"iid": 16, "title": "t", "state": "opened"})

	nested := forge.PRRef{Platform: event.PlatformGitLab, Owner: "group/sub", Repo: "kibitz", Number: 16}
	if _, err := f.client(t).PullRequest(context.Background(), nested); err != nil {
		t.Fatalf("PullRequest: %v", err)
	}
}

func TestPullRequest(t *testing.T) {
	f := newFake(t)
	f.handle("/api/v4/projects/yteraoka%2Fkibitz/merge_requests/16", http.StatusOK, map[string]any{
		"id": 93, "iid": 16,
		"title": "Add the SQS subscriber", "description": "本文",
		"state": "opened", "draft": false,
		"source_branch": "feature/sqs", "target_branch": "main",
		"source_project_id": 42, "target_project_id": 42,
		"web_url":       "https://gitlab.example.com/yteraoka/kibitz/-/merge_requests/16",
		"author":        map[string]any{"id": 1001, "username": "yteraoka"},
		"changes_count": "3",
		"diff_refs": map[string]any{
			"base_sha": "base111", "head_sha": "head222", "start_sha": "start333",
		},
	})

	pr, err := f.client(t).PullRequest(context.Background(), ref)
	if err != nil {
		t.Fatalf("PullRequest: %v", err)
	}
	if pr.Number != 16 {
		t.Errorf("number = %d, want the iid", pr.Number)
	}
	if pr.State != "open" {
		t.Errorf("state = %q, want open for GitLab's opened", pr.State)
	}
	// The head is the one the diff is anchored to, not whatever sha happens
	// to be on the merge request.
	if pr.Source.SHA != "head222" {
		t.Errorf("head = %q, want the diff ref", pr.Source.SHA)
	}
	if pr.Author.Login != "yteraoka" {
		t.Errorf("author = %+v", pr.Author)
	}
	if pr.ChangedFiles != 3 {
		t.Errorf("changed files = %d", pr.ChangedFiles)
	}
}

func TestDiff(t *testing.T) {
	f := newFake(t)
	f.handle("/api/v4/projects/yteraoka%2Fkibitz/merge_requests/16/diffs", http.StatusOK, []map[string]any{
		{
			"old_path": "queue.go", "new_path": "queue.go",
			"diff": "@@ -1,2 +1,3 @@\n context\n+added\n-removed\n",
		},
		{
			"old_path": "old.go", "new_path": "new.go", "renamed_file": true,
			"diff": "@@ -1 +1 @@\n-a\n+b\n",
		},
		{"old_path": "gone.go", "new_path": "gone.go", "deleted_file": true, "diff": ""},
	})

	diff, err := f.client(t).Diff(context.Background(), ref)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(diff.Files) != 3 {
		t.Fatalf("%d files, want 3", len(diff.Files))
	}
	// GitLab does not count the lines, so kibitz counts them from the patch.
	if diff.Files[0].Additions != 1 || diff.Files[0].Deletions != 1 {
		t.Errorf("counts = +%d/-%d, want +1/-1", diff.Files[0].Additions, diff.Files[0].Deletions)
	}
	if diff.Files[1].Status != forge.FileRenamed || diff.Files[1].PreviousPath != "old.go" {
		t.Errorf("rename = %+v", diff.Files[1])
	}
	if diff.Files[2].Status != forge.FileRemoved {
		t.Errorf("deletion = %+v", diff.Files[2])
	}
}

func TestCompare(t *testing.T) {
	f := newFake(t)
	f.handle("/api/v4/projects/yteraoka%2Fkibitz/repository/compare", http.StatusOK, map[string]any{
		"diffs": []map[string]any{
			{"old_path": "queue.go", "new_path": "queue.go", "diff": "@@ -1 +1,2 @@\n a\n+b\n"},
		},
	})

	diff, err := f.client(t).Compare(context.Background(), ref, "old111", "new222")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(diff.Files) != 1 {
		t.Fatalf("%d files, want 1", len(diff.Files))
	}
	if q := f.lastRequest().Query; !strings.Contains(q, "from=old111") || !strings.Contains(q, "to=new222") {
		t.Errorf("query = %q", q)
	}
}

// A commit that no longer exists is an ordinary event after a force push, not
// a failure.
func TestCompareMissingCommit(t *testing.T) {
	f := newFake(t)
	f.handle("/api/v4/projects/yteraoka%2Fkibitz/repository/compare", http.StatusNotFound,
		map[string]any{"message": "404 Commit Not Found"})

	if _, err := f.client(t).Compare(context.Background(), ref, "gone", "head"); !errors.Is(err, forge.ErrNoCompare) {
		t.Fatalf("err = %v, want ErrNoCompare", err)
	}
}

func TestComments(t *testing.T) {
	f := newFake(t)
	f.handle("/api/v4/projects/yteraoka%2Fkibitz/merge_requests/16/discussions", http.StatusOK, []map[string]any{
		{
			"id": "disc1",
			"notes": []map[string]any{
				{"id": 1, "body": "ここで ctx を見ていません", "author": map[string]any{"username": "kibitz"},
					"position": map[string]any{"new_path": "queue.go", "new_line": 88}},
				{"id": 2, "body": "なぜですか?", "author": map[string]any{"username": "yteraoka"}},
			},
		},
		{
			"id":    "disc2",
			"notes": []map[string]any{{"id": 3, "body": "changed the description", "system": true}},
		},
	})

	comments, err := f.client(t).Comments(context.Background(), ref)
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	// The system note is GitLab talking to itself.
	if len(comments) != 2 {
		t.Fatalf("%d comments, want 2: %+v", len(comments), comments)
	}
	// A reply has to go back to the discussion, so every comment carries it.
	for _, c := range comments {
		if c.ThreadID != "disc1" {
			t.Errorf("thread = %q, want disc1: %+v", c.ThreadID, c)
		}
	}
	if comments[0].Path != "queue.go" || comments[0].Line != 88 {
		t.Errorf("position = %s:%d", comments[0].Path, comments[0].Line)
	}
}

func TestUpsertSummaryCreatesThenUpdates(t *testing.T) {
	const marker = "<!-- kibitz:summary -->"

	t.Run("creates when there is nothing to replace", func(t *testing.T) {
		f := newFake(t)
		f.handle("/api/v4/projects/yteraoka%2Fkibitz/merge_requests/16/notes", http.StatusOK, []map[string]any{})

		if err := f.client(t).UpsertSummary(context.Background(), ref, marker, "本文"); err != nil {
			t.Fatalf("UpsertSummary: %v", err)
		}
		last := f.lastRequest()
		if last.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", last.Method)
		}
		if body, _ := last.Body["body"].(string); !strings.HasPrefix(body, marker) {
			t.Errorf("body = %q, want it to carry the marker", body)
		}
	})

	t.Run("replaces its own note", func(t *testing.T) {
		f := newFake(t)
		f.mux.HandleFunc("/api/v4/projects/yteraoka%2Fkibitz/merge_requests/16/notes", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"id": 5, "body": "誰かのコメント"},
					{"id": 7, "body": marker + "\n前回のサマリ"},
				})
				return
			}
			t.Errorf("a new note was posted although one existed")
		})
		f.handle("/api/v4/projects/yteraoka%2Fkibitz/merge_requests/16/notes/7", http.StatusOK, map[string]any{"id": 7})

		if err := f.client(t).UpsertSummary(context.Background(), ref, marker, "新しいサマリ"); err != nil {
			t.Fatalf("UpsertSummary: %v", err)
		}
		if last := f.lastRequest(); last.Method != http.MethodPut || !strings.HasSuffix(last.Path, "/notes/7") {
			t.Errorf("request = %s %s, want a PUT to the existing note", last.Method, last.Path)
		}
	})
}

func TestReplyToThread(t *testing.T) {
	f := newFake(t)
	f.handle("/api/v4/projects/yteraoka%2Fkibitz/merge_requests/16/discussions/disc1/notes", http.StatusOK, map[string]any{"id": 9})

	if err := f.client(t).ReplyToThread(context.Background(), ref, "disc1", "答えです"); err != nil {
		t.Fatalf("ReplyToThread: %v", err)
	}
	if last := f.lastRequest(); !strings.HasSuffix(last.Path, "/discussions/disc1/notes") {
		t.Errorf("path = %q", last.Path)
	}
}

// An answer with nowhere to go is still worth posting.
func TestReplyWithoutAThread(t *testing.T) {
	f := newFake(t)
	f.handle("/api/v4/projects/yteraoka%2Fkibitz/merge_requests/16/notes", http.StatusOK, map[string]any{"id": 9})

	if err := f.client(t).ReplyToThread(context.Background(), ref, "", "答えです"); err != nil {
		t.Fatalf("ReplyToThread: %v", err)
	}
	if last := f.lastRequest(); !strings.HasSuffix(last.Path, "/merge_requests/16/notes") {
		t.Errorf("path = %q, want the merge request itself", last.Path)
	}
}

func TestCreateReviewPostsOneDiscussionPerFinding(t *testing.T) {
	f := newFake(t)
	f.handle("/api/v4/projects/yteraoka%2Fkibitz/merge_requests/16", http.StatusOK, map[string]any{
		"iid": 16, "state": "opened",
		"diff_refs": map[string]any{"base_sha": "base111", "head_sha": "head222", "start_sha": "start333"},
	})
	f.handle("/api/v4/projects/yteraoka%2Fkibitz/merge_requests/16/discussions", http.StatusOK, map[string]any{"id": "new"})

	err := f.client(t).CreateReview(context.Background(), ref, forge.Review{
		CommitSHA: "head222",
		Comments: []forge.InlineComment{
			{Path: "queue.go", Line: 88, Body: "ctx を見ていません", Suggestion: "return ctx.Err()"},
			{Path: "store.go", Line: 12, EndLine: 20, Body: "範囲の指摘"},
		},
	})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}

	var posted []recorded
	for _, r := range f.requests {
		if r.Method == http.MethodPost {
			posted = append(posted, r)
		}
	}
	if len(posted) != 2 {
		t.Fatalf("%d discussions posted, want one per finding", len(posted))
	}

	position, ok := posted[0].Body["position"].(map[string]any)
	if !ok {
		t.Fatalf("no position: %+v", posted[0].Body)
	}
	for _, key := range []string{"base_sha", "head_sha", "start_sha", "new_path", "old_path", "new_line"} {
		if position[key] == nil {
			t.Errorf("position has no %s: %+v", key, position)
		}
	}
	// Both paths are required, and an added line carries only new_line.
	if position["old_path"] != "queue.go" || position["new_path"] != "queue.go" {
		t.Errorf("paths = %v / %v", position["old_path"], position["new_path"])
	}
	if _, present := position["old_line"]; present {
		t.Errorf("old_line was sent for an added line: %+v", position)
	}
	if body, _ := posted[0].Body["body"].(string); !strings.Contains(body, "```suggestion") {
		t.Errorf("body does not carry the suggestion: %q", body)
	}
	// A range kibitz cannot anchor is stated instead of dropped.
	if body, _ := posted[1].Body["body"].(string); !strings.Contains(body, "12行目〜20行目") {
		t.Errorf("body does not state the range: %q", body)
	}
}

// Reviewing takes minutes; the merge request can move in that time, and the
// positions then describe a diff nobody is looking at.
func TestCreateReviewRefusesAMovedHead(t *testing.T) {
	f := newFake(t)
	f.handle("/api/v4/projects/yteraoka%2Fkibitz/merge_requests/16", http.StatusOK, map[string]any{
		"iid": 16, "diff_refs": map[string]any{"base_sha": "b", "head_sha": "moved", "start_sha": "s"},
	})

	err := f.client(t).CreateReview(context.Background(), ref, forge.Review{
		CommitSHA: "head222",
		Comments:  []forge.InlineComment{{Path: "queue.go", Line: 88, Body: "x"}},
	})
	if !errors.Is(err, forge.ErrInvalidPosition) {
		t.Fatalf("err = %v, want ErrInvalidPosition", err)
	}
}

// A position GitLab will not place is one finding lost, not the whole review.
func TestCreateReviewKeepsGoingAfterARejectedPosition(t *testing.T) {
	f := newFake(t)
	f.handle("/api/v4/projects/yteraoka%2Fkibitz/merge_requests/16", http.StatusOK, map[string]any{
		"iid": 16, "diff_refs": map[string]any{"base_sha": "b", "head_sha": "h", "start_sha": "s"},
	})

	var posts int
	f.mux.HandleFunc("/api/v4/projects/yteraoka%2Fkibitz/merge_requests/16/discussions", func(w http.ResponseWriter, _ *http.Request) {
		posts++
		if posts == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "400 Bad request - Note {:line_code=>[\"can't be blank\"]}"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok"})
	})

	err := f.client(t).CreateReview(context.Background(), ref, forge.Review{
		Comments: []forge.InlineComment{
			{Path: "gone.go", Line: 1, Body: "置けない"},
			{Path: "queue.go", Line: 88, Body: "置ける"},
		},
	})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	if posts != 2 {
		t.Errorf("%d posts, want the second finding to have been tried", posts)
	}
}

func TestNewRejectsAMissingToken(t *testing.T) {
	if _, err := glforge.New(glforge.Config{}); err == nil {
		t.Fatal("New succeeded without a token")
	}
}

func TestCloneAuth(t *testing.T) {
	f := newFake(t)

	cred, err := f.client(t).CloneAuth(context.Background(), ref)
	if err != nil {
		t.Fatalf("CloneAuth: %v", err)
	}
	if cred.Username != "oauth2" || cred.Token != token {
		t.Errorf("credential = %+v", cred)
	}
}
