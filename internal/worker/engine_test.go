package worker_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/policy"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/store/memory"
	"github.com/yteraoka/kibitz/internal/worker"
)

// geminiOnly is an engine that, like adk, runs Gemini only.
type geminiOnly struct{ fakeEngine }

func (e *geminiOnly) CanRun(model string) error {
	if !strings.Contains(model, "gemini-") {
		return errors.New("not Gemini")
	}
	return nil
}

// engineJob is a job whose own engine is "opencode" and which a review
// command may switch to "pi" and to "adk", which runs Gemini only.
func engineJob(t *testing.T, f *fakeForge, results ...*reviewer.Result) (*worker.ReviewJob, *fakeEngine, *fakeEngine, *fakeEngine) {
	t.Helper()
	usual := &fakeEngine{results: results}
	pi := &fakeEngine{results: results}
	adk := &geminiOnly{fakeEngine{results: results}}
	job := newJob(t, f, usual)
	job.Model = "google-vertex/gemini-3.1-pro-preview"
	job.EngineName = "opencode"
	job.CommandEngines = map[string]reviewer.Engine{"opencode": usual, "pi": pi, "adk": adk}
	return job, usual, pi, &adk.fakeEngine
}

func reviewCommand(t *testing.T, job *worker.ReviewJob, origin string, args ...string) {
	t.Helper()
	ev := pullRequestEvent(event.KindCommand, origin)
	ev.Command = &event.Command{Name: policy.CommandReview, Args: args}
	if err := job.Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

// An engine the deployment allows runs that review, and the summary says so.
func TestAReviewCommandMayChooseAnAllowedEngine(t *testing.T) {
	for _, args := range [][]string{
		{"--engine", "pi"},
		{"--engine=pi"},
		{"--focus", "security", "--engine", "pi"},
	} {
		origin, _, _ := originRepo(t)
		f := defaultForge()
		job, usual, pi, _ := engineJob(t, f, reviewResult())
		reviewCommand(t, job, origin, args...)

		if len(usual.requests) != 0 || len(pi.requests) != 1 {
			t.Fatalf("%v: usual ran %d times, pi %d; want pi once", args, len(usual.requests), len(pi.requests))
		}
		if summary := f.summaries[len(f.summaries)-1]; !strings.Contains(summary, "エンジン `pi` でレビューしました") {
			t.Errorf("%v: the summary does not name the engine:\n%s", args, summary)
		}
	}
}

// --engine and --model go together: the engine is asked about the model the
// command chose, not the usual one.
func TestAReviewCommandMayChooseTheEngineAndTheModel(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	job, _, _, adk := engineJob(t, f, reviewResult())
	job.Model = "google-vertex/claude-opus-5@default"
	job.CommandModels = []string{"google-vertex/gemini-*"}
	reviewCommand(t, job, origin, "--engine", "adk", "--model", "google-vertex/gemini-3.8-flash")

	if len(adk.requests) != 1 || adk.requests[0].Model != "google-vertex/gemini-3.8-flash" {
		t.Fatalf("adk requests = %+v, want one on the model asked for", adk.requests)
	}
}

// An engine that is not allowed, or cannot run the model, does not stop the
// review: it runs on the usual engine, and the summary says why.
func TestAReviewCommandCannotChooseAnEngineThatIsNotAllowed(t *testing.T) {
	cases := map[string]struct {
		engines map[string]bool
		model   string
		asked   string
		note    string
	}{
		"no list":     {nil, "", "pi", "この kibitz ではコメントでエンジンを指定できません"},
		"not on list": {map[string]bool{"adk": true}, "", "pi", "使えるのは `adk`"},
		"unknown":     {map[string]bool{"pi": true}, "", "codex", "使えるのは `pi`"},
		"wrong model": {map[string]bool{"adk": true}, "google-vertex/claude-opus-5@default", "adk", "モデル `google-vertex/claude-opus-5@default` を実行できません"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			origin, _, _ := originRepo(t)
			f := defaultForge()
			job, usual, _, _ := engineJob(t, f, reviewResult())
			for engine := range job.CommandEngines {
				if !c.engines[engine] {
					delete(job.CommandEngines, engine)
				}
			}
			if c.model != "" {
				job.Model = c.model
			}
			reviewCommand(t, job, origin, "--engine", c.asked)

			if len(usual.requests) != 1 {
				t.Fatalf("the usual engine ran %d times, want once", len(usual.requests))
			}
			summary := f.summaries[len(f.summaries)-1]
			for _, want := range []string{"`" + c.asked + "` は使えないため、`opencode` でレビューしました", c.note} {
				if !strings.Contains(summary, want) {
					t.Errorf("the summary does not say %q:\n%s", want, summary)
				}
			}
		})
	}
}

// Asking for the engine the deployment already runs changes nothing and
// says nothing.
func TestAReviewCommandAskingForTheUsualEngineSaysNothing(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	job, usual, _, _ := engineJob(t, f, reviewResult())
	reviewCommand(t, job, origin, "--engine", "opencode")

	if len(usual.requests) != 1 {
		t.Fatalf("the usual engine ran %d times", len(usual.requests))
	}
	if summary := f.summaries[len(f.summaries)-1]; strings.Contains(summary, "エンジン") {
		t.Errorf("the summary talks about the engine:\n%s", summary)
	}
}

// The triage pass runs on the engine asked for when it can run the triage
// model, and on the usual one when it cannot, rather than failing and
// reviewing everything.
func TestTriageFollowsTheEngineACommandAskedFor(t *testing.T) {
	triage := &reviewer.Result{Triage: &reviewer.TriageOutput{SchemaVersion: 1, Paths: []string{"file01.go"}}}
	for _, c := range []struct {
		triageModel string
		onAsked     bool
	}{
		{"google-vertex/gemini-3.5-flash-lite", true},
		{"google-vertex/claude-haiku-5@default", false},
	} {
		origin, _, _ := originRepo(t)
		f := defaultForge()
		f.diff = bigDiff(20)
		job, usual, _, adk := engineJob(t, f)
		usual.results = []*reviewer.Result{triage}
		adk.results = []*reviewer.Result{triage, reviewResult()}
		if !c.onAsked {
			adk.results = []*reviewer.Result{reviewResult()}
		}
		job.MaxDiffLines = 100
		job.TriageModel = c.triageModel
		reviewCommand(t, job, origin, "--engine", "adk")

		ranTriage := usual
		if c.onAsked {
			ranTriage = adk
		}
		if len(ranTriage.requests) == 0 || ranTriage.requests[0].Mode != reviewer.ModeTriage {
			t.Errorf("%s: triage did not run where expected (usual %d runs, adk %d runs)",
				c.triageModel, len(usual.requests), len(adk.requests))
		}
		if last := adk.requests[len(adk.requests)-1]; last.Mode != reviewer.ModeReview || len(last.Diff.Files) != 1 {
			t.Errorf("%s: the review did not run on adk with the selection: %+v", c.triageModel, last)
		}
	}
}

// A question runs on the usual engine, which cannot continue another engine's
// conversation, so a review on an engine a command asked for leaves the
// stored session alone.
func TestAReviewOnAnotherEngineDoesNotReplaceTheSession(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	job, usual, pi, _ := engineJob(t, f)
	job.Store = memory.New()
	usual.results = []*reviewer.Result{
		{Summary: "問題なし", SessionID: "ses_usual", RawOutput: &reviewer.Output{SchemaVersion: 1, Summary: "問題なし"}},
		{Reply: "こうです。"},
	}
	pi.results = []*reviewer.Result{
		{Summary: "問題なし", SessionID: "pi_session", RawOutput: &reviewer.Output{SchemaVersion: 1, Summary: "問題なし"}},
	}

	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("review: %v", err)
	}
	reviewCommand(t, job, origin, "--engine", "pi")

	ev := pullRequestEvent(event.KindCommentCreated, origin)
	ev.Comment = &event.Comment{ID: "9", ThreadID: "7", Body: "@kibitz なぜ?"}
	if err := job.Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if got := usual.requests[1].SessionID; got != "ses_usual" {
		t.Errorf("session = %q, want the usual engine's", got)
	}
}

// The help text tells people about --engine only where it works, and which
// engines it takes.
func TestHelpListsTheEnginesACommandMayChoose(t *testing.T) {
	for _, engines := range []map[string]reviewer.Engine{nil, {"pi": &fakeEngine{}, "adk": &fakeEngine{}}} {
		origin, _, _ := originRepo(t)
		f := defaultForge()
		job := newJob(t, f, &fakeEngine{})
		job.CommandEngines = engines

		ev := pullRequestEvent(event.KindCommand, origin)
		ev.Command = &event.Command{Name: policy.CommandHelp}
		if err := job.Handle(context.Background(), queued(ev)); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		help := f.summaries[0]
		if got := strings.Contains(help, "--engine"); got != (len(engines) > 0) {
			t.Errorf("engines %v: help mentions --engine = %v:\n%s", engines, got, help)
		}
		if len(engines) > 0 && !strings.Contains(help, "`adk`、`pi`") {
			t.Errorf("help does not list the engines in order:\n%s", help)
		}
	}
}
