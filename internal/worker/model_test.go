package worker_test

import (
	"context"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/policy"
	"github.com/yteraoka/kibitz/internal/reviewer"
)

// reviewWithModel runs one review command with args and returns the model the
// engine was asked to use and the summary that was posted.
func reviewWithModel(t *testing.T, allowed []string, args ...string) (string, string) {
	t.Helper()
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}
	job := newJob(t, f, e)
	job.Model = "google-vertex/gemini-3.1-pro-preview"
	job.CommandModels = allowed

	ev := pullRequestEvent(event.KindCommand, origin)
	ev.Command = &event.Command{Name: policy.CommandReview, Args: args}
	if err := job.Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(e.requests) != 1 || len(f.summaries) == 0 {
		t.Fatalf("%d requests, %d summaries", len(e.requests), len(f.summaries))
	}
	return e.requests[0].Model, f.summaries[len(f.summaries)-1]
}

// A model the deployment allows is used for that review, and the summary
// says so: nothing else in it names the model.
func TestAReviewCommandMayChooseAnAllowedModel(t *testing.T) {
	for _, args := range [][]string{
		{"--model", "google-vertex/gemini-3.8-flash"},
		{"--model=google-vertex/gemini-3.8-flash"},
		{"--focus", "security", "--model", "google-vertex/gemini-3.8-flash"},
	} {
		model, summary := reviewWithModel(t, []string{"google-vertex/gemini-*"}, args...)
		if model != "google-vertex/gemini-3.8-flash" {
			t.Errorf("%v: Model = %q, want the one the command asked for", args, model)
		}
		if !strings.Contains(summary, "`google-vertex/gemini-3.8-flash` でレビューしました") {
			t.Errorf("%v: the summary does not name the model:\n%s", args, summary)
		}
	}
}

// Anybody who may comment may ask for a review, so the model is not theirs
// to choose unless the operator said which ones are. The review still runs,
// on the usual model, and says why.
func TestAReviewCommandCannotChooseAModelThatIsNotAllowed(t *testing.T) {
	cases := map[string]struct {
		allowed []string
		note    string
	}{
		"no list":     {nil, "この kibitz ではコメントでモデルを指定できません"},
		"not on list": {[]string{"google-vertex/gemini-*"}, "使えるのは `google-vertex/gemini-*`"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			model, summary := reviewWithModel(t, c.allowed, "--model", "google-vertex/claude-opus-5@default")
			if model != "google-vertex/gemini-3.1-pro-preview" {
				t.Errorf("Model = %q, want the configured one", model)
			}
			for _, want := range []string{"`google-vertex/claude-opus-5@default` は使えないため", c.note} {
				if !strings.Contains(summary, want) {
					t.Errorf("the summary does not say %q:\n%s", want, summary)
				}
			}
		})
	}
}

// Without --model nothing changes and nothing is said.
func TestAReviewCommandWithoutAModelSaysNothingAboutIt(t *testing.T) {
	model, summary := reviewWithModel(t, []string{"*"}, "--focus", "security")
	if model != "google-vertex/gemini-3.1-pro-preview" {
		t.Errorf("Model = %q", model)
	}
	if strings.Contains(summary, "でレビューしました") {
		t.Errorf("the summary talks about the model:\n%s", summary)
	}
}

// The help text tells people about --model only where it works, and which
// models it takes.
func TestHelpListsTheModelsACommandMayChoose(t *testing.T) {
	for _, allowed := range [][]string{nil, {"google-vertex/gemini-*"}} {
		origin, _, _ := originRepo(t)
		f := defaultForge()
		job := newJob(t, f, &fakeEngine{})
		job.CommandModels = allowed

		ev := pullRequestEvent(event.KindCommand, origin)
		ev.Command = &event.Command{Name: policy.CommandHelp}
		if err := job.Handle(context.Background(), queued(ev)); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		help := f.summaries[0]
		if got := strings.Contains(help, "--model"); got != (len(allowed) > 0) {
			t.Errorf("allowed %v: help mentions --model = %v:\n%s", allowed, got, help)
		}
		if len(allowed) > 0 && !strings.Contains(help, "`google-vertex/gemini-*`") {
			t.Errorf("help does not list the models:\n%s", help)
		}
	}
}
