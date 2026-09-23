package worker_test

import (
	"context"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/store/memory"
	"github.com/yteraoka/kibitz/internal/worker"
)

func TestParseBudgets(t *testing.T) {
	budgets, err := worker.ParseBudgets([]string{"acme/payments=200", " acme/* = 50 ", "*=10", ""})
	if err != nil {
		t.Fatalf("ParseBudgets: %v", err)
	}

	// The first matching pattern decides, which is what lets a specific
	// repository sit above its organization's ceiling.
	tests := map[string]float64{
		"acme/payments": 200,
		"acme/web":      50,
		"other/thing":   10,
	}
	for repo, want := range tests {
		got, ok := budgets.For(repo)
		if !ok || got != want {
			t.Errorf("%s got (%v, %v), want %v", repo, got, ok, want)
		}
	}
}

func TestBudgetsWithoutAMatchDoNotCap(t *testing.T) {
	budgets, err := worker.ParseBudgets([]string{"acme/*=50"})
	if err != nil {
		t.Fatalf("ParseBudgets: %v", err)
	}
	if _, ok := budgets.For("other/thing"); ok {
		t.Error("a repository no pattern matches came back capped")
	}
	if _, ok := worker.Budgets(nil).For("acme/web"); ok {
		t.Error("an empty configuration came back capped")
	}
}

func TestParseBudgetsRejectsWhatCannotBeMeant(t *testing.T) {
	tests := map[string]string{
		"no amount":    "acme/*",
		"not a number": "acme/*=soon",
		"no pattern":   "=50",
		"zero":         "acme/*=0",
		"negative":     "acme/*=-5",
	}
	for name, entry := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := worker.ParseBudgets([]string{entry}); err == nil {
				t.Errorf("ParseBudgets(%q) was accepted", entry)
			}
		})
	}
}

// usedResult is a run that reports having consumed something, which is what
// makes it cost money.
func usedResult(input, output int) *reviewer.Result {
	r := reviewResult()
	r.Usage = reviewer.Usage{InputTokens: input, OutputTokens: output}
	return r
}

// reviewsPosted counts the summaries that are actual reviews. A review with
// no findings posts a summary and no inline comments, so counting reviews
// would make "it ran and found nothing" indistinguishable from "it never
// ran".
func reviewsPosted(f *fakeForge) int {
	n := 0
	for _, marker := range f.markers {
		if marker == worker.SummaryMarker {
			n++
		}
	}
	return n
}

// budgetJob is a worker with prices, a ceiling and somewhere to keep the
// running total.
func budgetJob(t *testing.T, f *fakeForge, e *fakeEngine, entries []string) *worker.ReviewJob {
	t.Helper()

	j := newJob(t, f, e)
	j.Store = memory.New()
	j.Prices = reviewer.Prices{reviewer.PriceFallback: {Input: 1000, Output: 1000}}

	budgets, err := worker.ParseBudgets(entries)
	if err != nil {
		t.Fatalf("ParseBudgets: %v", err)
	}
	j.Budgets = budgets
	return j
}

// TestABudgetStopsTheNextReviewAndSaysWhy is the behaviour the ceiling is for.
// Stopping is half of it; saying so is the other half, because a bot that
// silently stops is indistinguishable from one that is broken.
func TestABudgetStopsTheNextReviewAndSaysWhy(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	// At $1000 per million tokens, one review of a million tokens spends the
	// whole $1 ceiling.
	e := &fakeEngine{results: []*reviewer.Result{usedResult(1_000_000, 0), usedResult(1_000_000, 0)}}
	j := budgetJob(t, f, e, []string{"yteraoka/*=1"})

	ctx := context.Background()
	if err := j.Handle(ctx, queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("first review: %v", err)
	}
	if reviewsPosted(f) != 1 {
		t.Fatalf("%d reviews ran before the budget was spent, want 1", reviewsPosted(f))
	}

	second := pullRequestEvent(event.KindPRUpdated, origin)
	second.ID, second.Source.DeliveryID = "github:d2", "d2"
	if err := j.Handle(ctx, queued(second)); err != nil {
		t.Fatalf("second review: %v", err)
	}

	if got := reviewsPosted(f); got != 1 {
		t.Errorf("%d reviews ran in total, want 1: the second went ahead after the budget was spent", got)
	}

	var said bool
	for i, marker := range f.markers {
		if marker == worker.BudgetMarker {
			said = true
			if !strings.Contains(f.summaries[i], "予算") {
				t.Errorf("the notice does not mention the budget:\n%s", f.summaries[i])
			}
		}
	}
	if !said {
		t.Errorf("nothing was posted to say the budget was gone; markers = %v", f.markers)
	}
}

func TestWithoutABudgetNothingIsStopped(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{usedResult(9_000_000, 0), usedResult(9_000_000, 0)}}
	j := budgetJob(t, f, e, nil)

	ctx := context.Background()
	for i, id := range []string{"d1", "d2"} {
		ev := pullRequestEvent(event.KindPROpened, origin)
		ev.ID, ev.Source.DeliveryID = "github:"+id, id
		if err := j.Handle(ctx, queued(ev)); err != nil {
			t.Fatalf("review %d: %v", i, err)
		}
	}

	for _, marker := range f.markers {
		if marker == worker.BudgetMarker {
			t.Error("an uncapped repository was told it was out of budget")
		}
	}
	if got := reviewsPosted(f); got != 2 {
		t.Errorf("%d reviews ran, want 2", got)
	}
}

// TestAnUnpricedModelIsNotCountedAsFree covers the case that would make a
// ceiling silently stop applying: with no price for the model, counting the
// run as zero would let the budget be blown without ever being reached.
func TestAnUnpricedModelIsNotCountedAsFree(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{usedResult(9_000_000, 0), usedResult(9_000_000, 0)}}

	j := budgetJob(t, f, e, []string{"yteraoka/*=1"})
	j.Prices = nil // nobody said what anything costs

	ctx := context.Background()
	for _, id := range []string{"d1", "d2"} {
		ev := pullRequestEvent(event.KindPROpened, origin)
		ev.ID, ev.Source.DeliveryID = "github:"+id, id
		if err := j.Handle(ctx, queued(ev)); err != nil {
			t.Fatalf("review %s: %v", id, err)
		}
	}

	// Reviews continue, because refusing to work over a number nobody
	// supplied would be worse. What must not happen is the spend being
	// recorded as zero and the operator never finding out.
	if got := reviewsPosted(f); got != 2 {
		t.Errorf("%d reviews ran, want 2", got)
	}
	for _, marker := range f.markers {
		if marker == worker.BudgetMarker {
			t.Error("an unpriced run was charged, so the ceiling was reached on a number nobody supplied")
		}
	}
}

// TestTheEstimatePricesEachPassOnItsOwnModel guards the number people decide
// by. Triage may run on a cheaper model, and pricing its half at the review
// model's rate would misreport the bill in whichever direction they differ.
func TestTheEstimatePricesEachPassOnItsOwnModel(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{usedResult(1_000_000, 0)}}

	j := newJob(t, f, e)
	j.Prices = reviewer.Prices{
		"review-model": {Input: 10, Output: 10},
		"triage-model": {Input: 1, Output: 1},
	}
	j.Model = "review-model"
	j.TriageModel = "triage-model"

	if err := j.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.summaries) == 0 {
		t.Fatal("no summary was posted")
	}

	// One million input tokens on the review model at $10 per million.
	if !strings.Contains(f.summaries[0], "$10.00") {
		t.Errorf("the estimate is not the review model's price:\n%s", f.summaries[0])
	}
}
