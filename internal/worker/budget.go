package worker

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/policy"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/store"
)

// spendTTL is how long a month's running total is kept. The key already names
// the month, so this only decides when the row stops taking up space; two
// months is long enough to still answer "what did we spend in October" in
// early November.
const spendTTL = 70 * 24 * time.Hour

// Budget caps what one set of repositories may cost in a calendar month.
type Budget struct {
	// Pattern matches "owner/name" with the same wildcards the repository
	// allow list uses.
	Pattern string
	// Amount is the ceiling, in the currency the model prices are quoted in.
	Amount float64
}

// Budgets is an ordered list of ceilings. The first pattern that matches a
// repository decides, so the specific ones go before "*".
//
// A budget is deployment configuration, never repository configuration. A
// repository that could raise its own ceiling does not have one, and settings
// read from a pull request would let whoever opened it decide what kibitz may
// spend on them (docs/security.md, ADR-0011).
type Budgets []Budget

// ParseBudgets reads the configured form: one entry per pattern, as
//
//	pattern=amount
//
// where the amount is in the same currency as the model prices.
//
//	acme/payments=200
//	acme/*=50
//	*=10
func ParseBudgets(entries []string) (Budgets, error) {
	var budgets Budgets
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		pattern, amount, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("budget %q is not pattern=amount", entry)
		}
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			return nil, fmt.Errorf("budget %q has no repository pattern", entry)
		}

		value, err := strconv.ParseFloat(strings.TrimSpace(amount), 64)
		if err != nil {
			return nil, fmt.Errorf("budget for %q: %q is not a number", pattern, amount)
		}
		if value <= 0 {
			// Zero would read as "no budget" in one direction and "review
			// nothing" in the other. Whichever was meant, saying it outright
			// is better than picking one.
			return nil, fmt.Errorf("budget for %q must be greater than 0; remove the entry to leave it uncapped", pattern)
		}
		budgets = append(budgets, Budget{Pattern: pattern, Amount: value})
	}
	return budgets, nil
}

// For returns the ceiling that applies to a repository.
func (b Budgets) For(repo string) (float64, bool) {
	for _, budget := range b {
		if policy.Match(budget.Pattern, repo) {
			return budget.Amount, true
		}
	}
	return 0, false
}

// pass is one call to the agent: what it used, and the model that used it.
//
// The two are kept together because a review that needed a triage pass may
// have run the two halves on different models, and pricing both at the
// review model's rate would misreport whichever half was cheaper.
type pass struct {
	Model string
	Usage reviewer.Usage
}

// cost prices a set of passes. It reports false when any of them ran on a
// model nobody priced, because a total missing one of its halves is worse
// than no total: it reads as a real number.
func (j *ReviewJob) cost(passes ...pass) (float64, bool) {
	var total float64
	var priced bool

	for _, p := range passes {
		if p.Usage.Tokens() == 0 {
			continue
		}
		amount, ok := j.Prices.Cost(p.Model, p.Usage)
		if !ok {
			return 0, false
		}
		total += amount
		priced = true
	}
	return total, priced
}

// exhausted reports whether a repository has already spent its month.
//
// The ceiling stops the next review rather than capping the one running: a
// single review's cost is not known until it has been paid. That is the right
// way round — the alternative is abandoning work halfway and billing for it
// anyway — but it does mean the total can end a little over.
func (j *ReviewJob) exhausted(ctx context.Context, ev *event.ReviewEvent) (spent, limit float64, over bool) {
	repo := ev.Repository.FullName
	limit, ok := j.Budgets.For(repo)
	if !ok || j.Store == nil {
		return 0, 0, false
	}

	micros, err := j.Store.Incr(ctx, store.SpendKey(repo, time.Now()), 0, spendTTL)
	if err != nil {
		// A total that cannot be read must not stop reviews. The budget is
		// there to bound a surprise, and refusing to work because the store
		// is unreachable would turn one outage into two.
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "could not read the month's spend; the budget is not being enforced",
			slog.String("repository", repo),
			slog.String("error", err.Error()),
		)
		return 0, limit, false
	}

	spent = fromMicros(micros)
	return spent, limit, spent >= limit
}

// charge adds what a job cost to the repository's running total.
//
// A model with no configured price is not charged as zero: that would let a
// budget be exceeded without ever being reached. It is reported instead, once
// per job, because a ceiling that silently stops applying is worse than none.
func (j *ReviewJob) charge(ctx context.Context, ev *event.ReviewEvent, passes ...pass) {
	repo := ev.Repository.FullName
	limit, budgeted := j.Budgets.For(repo)
	if j.Store == nil {
		return
	}

	amount, ok := j.cost(passes...)
	if !ok {
		if budgeted {
			j.Logger.LogAttrs(ctx, slog.LevelWarn,
				"a run has no configured price, so it is not counted against the budget",
				slog.String("repository", repo),
				slog.Float64("budget", limit),
				slog.String("hint", "set a fallback price with KIBITZ_MODEL_PRICES=*=input/output"),
			)
		}
		return
	}
	if amount <= 0 {
		return
	}

	total, err := j.Store.Incr(ctx, store.SpendKey(repo, time.Now()), toMicros(amount), spendTTL)
	if err != nil {
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "could not record what the job cost",
			slog.String("repository", repo),
			slog.String("error", err.Error()),
		)
		return
	}

	attrs := []slog.Attr{
		slog.String("repository", repo),
		slog.Float64("cost", amount),
		slog.Float64("spent_this_month", fromMicros(total)),
	}
	if budgeted {
		attrs = append(attrs, slog.Float64("budget", limit))
	}
	j.Logger.LogAttrs(ctx, slog.LevelInfo, "recorded what the job cost", attrs...)
}

// triageModel is the model the triage pass runs on: the one configured for
// it, or the review's own when none is.
func (j *ReviewJob) triageModel(reviewModel string) string {
	if j.TriageModel != "" {
		return j.TriageModel
	}
	return reviewModel
}

// reportExhausted stops a job that the month's budget will not pay for, and
// says so on the pull request.
//
// Saying so is the point. A bot that simply stops is indistinguishable from
// one that is broken, and the author waiting for a review has no way to tell
// which. The notice replaces itself, so a repository at its ceiling leaves
// one comment per pull request rather than one per push.
func (j *ReviewJob) reportExhausted(ctx context.Context, client forge.Client, ref forge.PRRef, ev *event.ReviewEvent) bool {
	spent, limit, over := j.exhausted(ctx, ev)
	if !over {
		return false
	}

	j.Logger.LogAttrs(ctx, slog.LevelWarn, "the repository has spent its budget for the month; skipping",
		slog.String("ref", ref.String()),
		slog.Float64("spent", spent),
		slog.Float64("budget", limit),
	)

	body := fmt.Sprintf(
		"今月の予算を使い切ったため、このリポジトリのレビューを停止しています。\n\n"+
			"使用 %s / 予算 %s (%s 時点)。来月の初日に自動で再開します。\n\n"+
			"それより早く再開する必要がある場合は、この kibitz の運用者に予算の引き上げを依頼してください。"+
			"予算はデプロイ側の設定で、リポジトリ側からは変更できません。\n",
		money(spent, j.currency()), money(limit, j.currency()),
		time.Now().UTC().Format("2006-01-02 15:04 UTC"))

	if err := client.UpsertSummary(ctx, ref, BudgetMarker, body); err != nil {
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "could not say that the budget is exhausted",
			slog.String("ref", ref.String()),
			slog.String("error", err.Error()),
		)
	}
	return true
}

// toMicros converts an amount of currency to the integer the counter holds.
// Money is counted in millionths rather than as a float because the store
// adds, and a running total of floats drifts.
func toMicros(amount float64) int64 {
	return int64(math.Round(amount * 1e6))
}

func fromMicros(micros int64) float64 {
	return float64(micros) / 1e6
}
