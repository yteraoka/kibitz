package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
)

// repo is the repository-level reference implement mode writes against: it
// names a repository and no pull request.
var repo = forge.PRRef{Platform: event.PlatformGitHub, Owner: "yteraoka", Repo: "kibitz"}

func TestCreatePullRequestOpensADraft(t *testing.T) {
	f := newFakeGitHub(t)
	f.handle("POST /repos/yteraoka/kibitz/pulls", http.StatusCreated, map[string]any{
		"number":   99,
		"html_url": "https://github.com/yteraoka/kibitz/pull/99",
		"draft":    true,
	})

	created, err := f.client(t).CreatePullRequest(context.Background(), repo, forge.NewPullRequest{
		Head:  "kibitz/issue-12",
		Base:  "main",
		Title: "[kibitz] #12 Support Azure DevOps",
		Body:  "AI が書きました",
		Draft: true,
	})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	if created.Number != 99 || !created.Draft {
		t.Errorf("created = %+v, want a draft numbered 99", created)
	}

	body := f.lastRequest().Body
	for key, want := range map[string]any{
		"head": "kibitz/issue-12", "base": "main", "draft": true,
	} {
		if body[key] != want {
			t.Errorf("%s = %v, want %v", key, body[key], want)
		}
	}
}

// A redelivered message must not open a second pull request for the same
// branch. GitHub says so with a 422, which is an answer and not a failure.
func TestCreatePullRequestReturnsTheExistingOne(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("POST /repos/yteraoka/kibitz/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": "Validation Failed: A pull request already exists for yteraoka:kibitz/issue-12.",
		})
	})
	f.handle("GET /repos/yteraoka/kibitz/pulls", http.StatusOK, []map[string]any{{
		"number":   77,
		"html_url": "https://github.com/yteraoka/kibitz/pull/77",
		"draft":    true,
		"head":     map[string]any{"ref": "kibitz/issue-12"},
	}})

	created, err := f.client(t).CreatePullRequest(context.Background(), repo, forge.NewPullRequest{
		Head: "kibitz/issue-12", Base: "main", Title: "x", Draft: true,
	})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	if created.Number != 77 {
		t.Errorf("Number = %d, want the pull request that already existed (77)", created.Number)
	}
}

// Draft pull requests are not available on every plan. A change that is already
// pushed is worth an ordinary pull request rather than nothing.
func TestCreatePullRequestFallsBackWhenDraftsAreUnavailable(t *testing.T) {
	f := newFakeGitHub(t)
	attempts := 0
	f.mux.HandleFunc("POST /repos/yteraoka/kibitz/pulls", func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		// The harness has already consumed the body to record it, so what was
		// sent is read from there rather than from the request.
		if f.lastRequest().Body["draft"] == true {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": "Draft pull requests are not supported in this repository.",
			})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 100, "html_url": "https://example.com/pull/100", "draft": false,
		})
	})
	// Asked before the fallback, and there is nothing there.
	f.handle("GET /repos/yteraoka/kibitz/pulls", http.StatusOK, []map[string]any{})

	created, err := f.client(t).CreatePullRequest(context.Background(), repo, forge.NewPullRequest{
		Head: "kibitz/issue-12", Base: "main", Title: "x", Draft: true,
	})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want the draft attempt and the fallback", attempts)
	}
	if created.Draft {
		t.Error("Draft = true; the caller has to be able to tell it did not get a draft")
	}
}

// Every other 422 stays a failure. A base branch that does not exist is not
// something to retry as anything else.
func TestCreatePullRequestDoesNotRetryOtherRejections(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("POST /repos/yteraoka/kibitz/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Validation Failed: base is invalid"})
	})
	f.handle("GET /repos/yteraoka/kibitz/pulls", http.StatusOK, []map[string]any{})

	if _, err := f.client(t).CreatePullRequest(context.Background(), repo, forge.NewPullRequest{
		Head: "kibitz/issue-12", Base: "nope", Title: "x", Draft: true,
	}); err == nil {
		t.Fatal("CreatePullRequest returned nil error for a base that does not exist")
	}
}

func TestCreatePullRequestNeedsBothBranches(t *testing.T) {
	f := newFakeGitHub(t)
	client := f.client(t)

	for name, req := range map[string]forge.NewPullRequest{
		"no head": {Base: "main"},
		"no base": {Head: "kibitz/issue-12"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := client.CreatePullRequest(context.Background(), repo, req); err == nil {
				t.Error("CreatePullRequest returned nil error")
			}
		})
	}
}

func TestFindPullRequestQualifiesTheHeadWithTheOwner(t *testing.T) {
	f := newFakeGitHub(t)
	f.handle("GET /repos/yteraoka/kibitz/pulls", http.StatusOK, []map[string]any{{
		"number": 77, "html_url": "https://example.com/pull/77",
		"head": map[string]any{"ref": "kibitz/issue-12"},
	}})

	found, err := f.client(t).FindPullRequest(context.Background(), repo, "kibitz/issue-12")
	if err != nil {
		t.Fatalf("FindPullRequest: %v", err)
	}
	if found == nil || found.Number != 77 {
		t.Fatalf("found = %+v, want 77", found)
	}
	if query := f.lastRequest().Query; !strings.Contains(query, "head=yteraoka%3Akibitz%2Fissue-12") {
		t.Errorf("query = %q, want the head qualified with the owner", query)
	}
}

// A branch of the same name on a fork is not this branch. GitHub filters, and
// the client checks the answer rather than trusting it.
func TestFindPullRequestIgnoresAnotherBranch(t *testing.T) {
	f := newFakeGitHub(t)
	f.handle("GET /repos/yteraoka/kibitz/pulls", http.StatusOK, []map[string]any{{
		"number": 77, "html_url": "https://example.com/pull/77",
		"head": map[string]any{"ref": "somebody-elses-branch"},
	}})

	found, err := f.client(t).FindPullRequest(context.Background(), repo, "kibitz/issue-12")
	if err != nil {
		t.Fatalf("FindPullRequest: %v", err)
	}
	if found != nil {
		t.Errorf("found = %+v, want nil", found)
	}
}

func TestFindPullRequestIsNilWhenThereIsNone(t *testing.T) {
	f := newFakeGitHub(t)
	f.handle("GET /repos/yteraoka/kibitz/pulls", http.StatusOK, []map[string]any{})

	found, err := f.client(t).FindPullRequest(context.Background(), repo, "kibitz/issue-12")
	if err != nil {
		t.Fatalf("FindPullRequest: %v", err)
	}
	if found != nil {
		t.Errorf("found = %+v, want nil", found)
	}
}
