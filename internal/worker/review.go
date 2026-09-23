package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/policy"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/reviewer/opencode"
	"github.com/yteraoka/kibitz/internal/store"
	"github.com/yteraoka/kibitz/internal/telemetry"
	"github.com/yteraoka/kibitz/internal/workspace"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Markers identify kibitz's own comments so that they can be replaced rather
// than stacked, and so kibitz can recognize its own writing.
const (
	SummaryMarker = "<!-- kibitz:summary -->"
	FailureMarker = "<!-- kibitz:failure -->"
	// BudgetMarker identifies the notice that says the month's budget is
	// gone. It replaces itself rather than stacking, so a repository at its
	// ceiling gets one notice per pull request instead of one per push.
	BudgetMarker = "<!-- kibitz:budget -->"
	HelpMarker   = "<!-- kibitz:help -->"
	IgnoreMarker = "<!-- kibitz:ignore -->"
)

// ReviewJob runs one review or one answer from start to finish: read the pull
// request, fetch it, run the agent, check what came back, post it.
type ReviewJob struct {
	Forges     map[event.Platform]forge.Client
	Engine     reviewer.Engine
	Workspace  workspace.Config
	Limits     reviewer.Limits
	Logger     *slog.Logger
	Language   string
	Guidelines string
	Model      string
	// MaxDiffLines is how large a change may be before it is triaged rather
	// than reviewed whole. Zero disables triage.
	MaxDiffLines int
	// TriageModel runs the triage pass. It reads file names, not code, so a
	// cheaper model is usually the right one. Empty means [ReviewJob.Model].
	TriageModel string
	// GuidelineFiles are the repository's own convention files, read from its
	// default branch and given to the agent as instructions. Nil means
	// [DefaultGuidelineFiles]; an empty slice reads none.
	GuidelineFiles []string
	// ReferenceDocs are glob patterns for the repository's decision records.
	// Nil means [DefaultReferenceDocs]; an empty slice indexes none.
	ReferenceDocs []string
	// MCP is the external tool servers this deployment offers. A repository
	// enables the ones it wants by name; anything it names that is not here
	// is reported rather than silently skipped.
	MCP opencode.Catalog
	// Prices estimates what a review cost, for the summary comment. It is
	// configured rather than known: prices differ by provider, region and
	// contract, and they change. With none configured the summary reports
	// tokens and says nothing about money.
	Prices reviewer.Prices
	// Currency is the symbol the estimate is written with. It follows
	// whatever the prices were given in, because printing a dollar sign in
	// front of a yen figure is worse than printing nothing.
	Currency string
	// Budgets cap what a repository may cost in a calendar month. With none
	// configured nothing is capped, which is the behaviour of every
	// deployment that has not thought about it yet.
	Budgets Budgets
	// Mention is how a comment addresses kibitz. It is only used to write the
	// help text, which would otherwise tell people to use a token this
	// deployment does not answer to.
	Mention string
	// SkipDraft leaves draft pull requests alone until they are marked ready.
	SkipDraft bool
	// Store remembers which commits have already been reviewed and how much
	// has been posted lately. It may be nil, in which case neither check runs.
	Store store.Store
	// MaxPostsPerHour caps how much kibitz writes to one pull request in an
	// hour. Zero means 10.
	MaxPostsPerHour int
	// ReviewedTTL is how long a reviewed commit is remembered.
	ReviewedTTL time.Duration
	// SessionTTL is how long the agent's conversation about one pull request,
	// and an "ignore" asked for on it, are remembered. Zero means a week.
	SessionTTL time.Duration
	// Metrics records what was posted and what was discarded. May be nil.
	Metrics *telemetry.Metrics
}

// Handle implements [Handler].
func (j *ReviewJob) Handle(ctx context.Context, job *Job) error {
	ev := job.Event
	client, ok := j.Forges[ev.Source.Platform]
	if !ok {
		// Acknowledged rather than retried: a platform this build cannot talk
		// to will not become supported by trying again.
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "no client for this platform; skipping",
			slog.String("platform", string(ev.Source.Platform)),
		)
		return nil
	}
	ref := forge.RefOf(ev)

	switch ev.Kind {
	case event.KindPROpened, event.KindPRUpdated, event.KindPRReadyForReview, event.KindPRReviewRequested:
		return j.review(ctx, client, ref, ev)

	case event.KindCommand:
		return j.command(ctx, client, ref, ev)

	case event.KindCommentCreated:
		// A mention without a command is a question.
		return j.answer(ctx, client, ref, ev)

	case event.KindPRClosed, event.KindPRMerged:
		// The conversation is over: nothing kibitz remembers about this pull
		// request is worth keeping, and an "ignore" that outlived it would
		// silently apply to a pull request nobody can reopen the discussion
		// on.
		return j.forget(ctx, ev)

	default:
		return nil
	}
}

func (j *ReviewJob) command(ctx context.Context, client forge.Client, ref forge.PRRef, ev *event.ReviewEvent) error {
	name := ""
	if ev.Command != nil {
		name = ev.Command.Name
	}

	switch name {
	case policy.CommandReview:
		// Asking for a review is asking to be reviewed again, so it lifts an
		// earlier "ignore" rather than being refused by it.
		j.unignore(ctx, ev)
		return j.review(ctx, client, ref, ev)
	case policy.CommandAnswer, policy.CommandExplain:
		return j.answer(ctx, client, ref, ev)
	case policy.CommandHelp:
		return client.UpsertSummary(ctx, ref, HelpMarker, helpText(j.mention()))
	case policy.CommandIgnore:
		return j.acknowledgeIgnore(ctx, client, ref, ev)
	case policy.CommandImplement, policy.CommandPlan:
		// Phase 8.
		j.Logger.LogAttrs(ctx, slog.LevelInfo, "command is not implemented yet",
			slog.String("command", name),
		)
		return nil
	default:
		return nil
	}
}

func (j *ReviewJob) review(ctx context.Context, client forge.Client, ref forge.PRRef, ev *event.ReviewEvent) error {
	// The event carries a snapshot from whenever the webhook fired, which is
	// already stale by the time a job starts, so the current state is fetched.
	pr, err := client.PullRequest(ctx, ref)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", ref, err)
	}
	if pr.State == "closed" && !pr.Merged {
		j.Logger.LogAttrs(ctx, slog.LevelInfo, "pull request is closed; skipping", slog.String("ref", ref.String()))
		return nil
	}

	// The repository's own settings, read from its default branch. They can
	// turn a review off, narrow it, or say nothing at all.
	settings := j.settingsFor(ctx, client, ref)

	if !settings.Reviews(ev.Kind) {
		j.Logger.LogAttrs(ctx, slog.LevelInfo, "the repository's settings do not ask for this review; skipping",
			slog.String("ref", ref.String()),
			slog.String("kind", string(ev.Kind)),
		)
		return nil
	}
	if pr.Draft && settings.SkipDraft && ev.Kind != event.KindCommand {
		j.Logger.LogAttrs(ctx, slog.LevelInfo, "pull request is a draft; skipping", slog.String("ref", ref.String()))
		return nil
	}

	// Asked to stay out. A command has already lifted the flag by the time it
	// gets here, so this only stops the automatic reviews.
	if j.ignored(ctx, ev) {
		j.Logger.LogAttrs(ctx, slog.LevelInfo, "this pull request is being ignored; skipping",
			slog.String("ref", ref.String()),
		)
		return nil
	}

	// Out of money for the month. This is checked before the diff rather
	// than after, because being over the ceiling means nothing is going to
	// be reviewed either way and the diff is not free — on Azure DevOps it
	// is two API calls per changed file.
	if j.reportExhausted(ctx, client, ref, ev) {
		return nil
	}

	// An explicit command is a request to review again, even if this commit
	// was reviewed before; anything else is skipped as already done.
	if ev.Kind != event.KindCommand {
		reviewed, err := j.alreadyReviewed(ctx, ev, pr.Source.SHA)
		if err != nil {
			return err
		}
		if reviewed {
			j.Logger.LogAttrs(ctx, slog.LevelInfo, "this commit has already been reviewed; skipping",
				slog.String("ref", ref.String()),
				slog.String("head", pr.Source.SHA),
			)
			return nil
		}
	}

	ctx, diffSpan := telemetry.Tracer().Start(ctx, "review.diff")
	diff, err := client.Diff(ctx, ref)
	diffSpan.End()
	if err != nil {
		return fmt.Errorf("fetching the diff of %s: %w", ref, err)
	}
	diff, ignored := filterPaths(diff, settings.PathsIgnore)
	settings.ignored = ignored
	if len(ignored) > 0 {
		j.Logger.LogAttrs(ctx, slog.LevelInfo, "files excluded by the repository's settings",
			slog.String("ref", ref.String()),
			slog.Int("files", len(ignored)),
		)
	}
	if len(diff.Files) == 0 {
		j.Logger.LogAttrs(ctx, slog.LevelInfo, "nothing to review", slog.String("ref", ref.String()))
		return nil
	}

	// A second review of the same pull request looks at what was pushed since
	// the first one. The whole diff is still what findings are checked
	// against, so a comment can never land outside it.
	changed, since := j.incremental(ctx, client, ref, ev, diff, pr.Source.SHA)

	// Existing comments are context, not a hard requirement: failing the whole
	// review because they could not be listed would be worse than repeating a
	// point someone already made.
	comments, err := client.Comments(ctx, ref)
	if err != nil {
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "could not list existing comments",
			slog.String("ref", ref.String()),
			slog.String("error", err.Error()),
		)
	}

	ws, err := j.prepareWorkspace(ctx, client, ref, ev, pr)
	if err != nil {
		return err
	}
	defer func() {
		if err := ws.Close(); err != nil {
			j.Logger.LogAttrs(context.WithoutCancel(ctx), slog.LevelWarn, "could not remove the workspace",
				slog.String("error", err.Error()),
			)
		}
	}()

	references := j.referenceDocs(ctx, ws)

	// Too large to review in one pass: a first pass decides what to read.
	selection := j.triage(ctx, ws, ev, pr, changed, settings.Settings)

	req := reviewer.Request{
		Mode:             reviewer.ModeReview,
		WorkspaceDir:     ws.Dir,
		Event:            ev,
		PullRequest:      pr,
		Diff:             selection.Diff,
		FullDiff:         diff,
		ExistingComments: kibitzExcluded(comments, ev),
		Language:         settings.Language,
		Model:            settings.Model,
		Guidelines:       settings.Guidelines,
		Focus:            focusOf(ev, settings.Settings),
		References:       references,
		MCP:              settings.mcp,
		HeadSHA:          ws.HeadSHA,
		SinceSHA:         since,
	}

	agentCtx, agentSpan := telemetry.Tracer().Start(ctx, "review.agent",
		trace.WithAttributes(attribute.String("kibitz.model", settings.Model)))
	result, err := j.runWithRetry(agentCtx, req)
	if result != nil {
		// A review starts fresh — its job is to look at the code, not to be
		// anchored by what it said last time — but the conversation it opens
		// is what a later "why?" continues.
		j.rememberSession(ctx, ev, result.SessionID)
	}
	if err != nil {
		agentSpan.RecordError(err)
		agentSpan.SetStatus(codes.Error, err.Error())
		agentSpan.End()
		return err
	}
	agentSpan.SetAttributes(
		attribute.Int("kibitz.input_tokens", result.Usage.Input()),
		attribute.Int("kibitz.output_tokens", result.Usage.Output()),
	)
	agentSpan.End()

	sanitized := reviewer.Sanitize(result.RawOutput, reviewer.NewPositions(diff), settings.Limits)
	j.Logger.LogAttrs(ctx, slog.LevelInfo, "review produced findings",
		slog.String("ref", ref.String()),
		slog.String("head", ws.HeadSHA),
		slog.String("since", since),
		slog.Int("files_reviewed", len(selection.Diff.Files)),
		slog.Int("files_skipped", len(selection.Skipped)),
		slog.Int("findings", len(sanitized.Findings)),
		slog.Int("dropped", sanitized.Dropped()),
		slog.Int("out_of_diff", sanitized.OutOfDiff),
		slog.Int("input_tokens", result.Usage.Input()),
		slog.Int("cached_tokens", result.Usage.CacheReadTokens+result.Usage.CacheWriteTokens),
		slog.Int("output_tokens", result.Usage.Output()),
		slog.Duration("agent_duration", result.Usage.Duration),
		slog.Int("tool_calls", result.Tools.Total()),
		slog.Int("reference_docs", len(references)),
		slog.Int("docs_searched", result.Tools.Searches),
		slog.Any("docs_read", result.Tools.Documents),
	)

	j.record(ev, result, sanitized, settings.Model)
	j.charge(ctx, ev, pass{Model: settings.Model, Usage: result.Usage},
		pass{Model: j.triageModel(settings.Model), Usage: selection.Usage})
	j.rememberReviewed(ctx, ev, ws.HeadSHA)

	ctx, postSpan := telemetry.Tracer().Start(ctx, "review.post",
		trace.WithAttributes(telemetry.AttrFindings.Int(len(sanitized.Findings))))
	defer postSpan.End()
	return j.post(ctx, client, ref, ev, result, sanitized, ws.HeadSHA, since, selection, settings)
}

// incremental narrows the diff to what has been pushed since the last review,
// and reports which commit that was.
//
// It returns the whole diff whenever it cannot do better: nothing reviewed
// before, an explicit --full, a comparison the forge could not answer (the
// usual outcome of a force push), or a comparison that came back empty. A
// review of too much is a cost; a review of too little is a missed bug.
func (j *ReviewJob) incremental(ctx context.Context, client forge.Client, ref forge.PRRef, ev *event.ReviewEvent, full *forge.Diff, headSHA string) (*forge.Diff, string) {
	if headSHA == "" || hasFlag(ev, "--full") {
		return full, ""
	}

	since := j.lastReviewed(ctx, ev)
	if since == "" || since == headSHA {
		return full, ""
	}

	partial, err := client.Compare(ctx, ref, since, headSHA)
	if err != nil {
		level := slog.LevelWarn
		if errors.Is(err, forge.ErrNoCompare) {
			// A rebase or a force push; reviewing everything is the answer.
			level = slog.LevelInfo
		}
		j.Logger.LogAttrs(ctx, level, "could not compare with the last review; reviewing the whole diff",
			slog.String("ref", ref.String()),
			slog.String("since", since),
			slog.String("error", err.Error()),
		)
		return full, ""
	}
	if len(partial.Files) == 0 {
		return full, ""
	}

	j.Logger.LogAttrs(ctx, slog.LevelInfo, "reviewing only what is new",
		slog.String("ref", ref.String()),
		slog.String("since", since),
		slog.Int("files", len(partial.Files)),
		slog.Int("files_in_full_diff", len(full.Files)),
	)
	return partial, since
}

// hasFlag reports whether a command carried an argument.
func hasFlag(ev *event.ReviewEvent, flag string) bool {
	if ev.Command == nil {
		return false
	}
	for _, arg := range ev.Command.Args {
		if strings.EqualFold(arg, flag) {
			return true
		}
	}
	return false
}

// record reports what the run produced, which is how cost and noise are
// tracked over time.
func (j *ReviewJob) record(ev *event.ReviewEvent, result *reviewer.Result, sanitized reviewer.Sanitized, model string) {
	if j.Metrics == nil {
		return
	}
	platform := string(ev.Source.Platform)

	for _, f := range sanitized.Findings {
		j.Metrics.CommentsPosted.WithLabelValues(platform, string(f.Severity)).Inc()
	}
	for reason, n := range map[string]int{
		"out_of_diff":    sanitized.OutOfDiff,
		"below_severity": sanitized.BelowSeverity,
		"duplicate":      sanitized.Duplicate,
		"over_limit":     sanitized.Excess,
	} {
		if n > 0 {
			j.Metrics.FindingsDropped.WithLabelValues(reason).Add(float64(n))
		}
	}
	if model != "" {
		for direction, n := range map[string]int{
			"input":       result.Usage.InputTokens,
			"cache_read":  result.Usage.CacheReadTokens,
			"cache_write": result.Usage.CacheWriteTokens,
			"output":      result.Usage.OutputTokens,
			"reasoning":   result.Usage.ReasoningTokens,
		} {
			j.Metrics.AgentTokens.WithLabelValues(model, direction).Add(float64(n))
		}
	}
	j.recordTools(result.Tools)
}

// recordTools reports what the agent did with its tools.
//
// The reference document counters are the ones with a decision behind them:
// the index costs a line of every prompt and two tool definitions of every
// request, and a repository whose records are never opened is paying for
// both. A zero here over a repository that has decision records is the
// argument for narrowing the patterns, or for switching the index off.
func (j *ReviewJob) recordTools(tools reviewer.ToolUse) {
	if j.Metrics == nil {
		return
	}
	for name, calls := range tools.Calls {
		failed := tools.Failed[name]
		if ok := calls - failed; ok > 0 {
			j.Metrics.AgentToolCalls.WithLabelValues(name, "ok").Add(float64(ok))
		}
		if failed > 0 {
			j.Metrics.AgentToolCalls.WithLabelValues(name, "error").Add(float64(failed))
		}
	}
	if tools.Searches > 0 {
		j.Metrics.ReferenceDocs.WithLabelValues("search").Add(float64(tools.Searches))
	}
	if n := len(tools.Documents); n > 0 {
		j.Metrics.ReferenceDocs.WithLabelValues("read").Add(float64(n))
	}
}

// runWithRetry gives the agent one more attempt when its output did not meet
// the contract, telling it what was wrong. Anything else fails the job so the
// queue redelivers it.
func (j *ReviewJob) runWithRetry(ctx context.Context, req reviewer.Request) (*reviewer.Result, error) {
	result, err := j.Engine.Run(ctx, req)
	if err == nil {
		return result, nil
	}

	var outputErr *reviewer.OutputError
	if !errors.As(err, &outputErr) {
		return nil, err
	}

	j.Logger.LogAttrs(ctx, slog.LevelWarn, "the agent's output did not meet the contract; retrying once",
		slog.String("error", err.Error()),
	)
	req.Feedback = err.Error()
	req.SessionID = "" // a fresh session, so the bad output is not context

	result, err = j.Engine.Run(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("the agent did not produce usable output: %w", err)
	}
	return result, nil
}

func (j *ReviewJob) prepareWorkspace(ctx context.Context, client forge.Client, ref forge.PRRef, ev *event.ReviewEvent, pr *event.PullRequest) (*workspace.Workspace, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "review.workspace")
	defer span.End()

	cred, err := client.CloneAuth(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("getting clone credentials for %s: %w", ref, err)
	}

	cloneURL := ev.Repository.CloneURL
	if cloneURL == "" {
		return nil, fmt.Errorf("no clone url for %s", ref)
	}

	ws, err := workspace.Prepare(ctx, j.Workspace, workspace.Spec{
		CloneURL:   cloneURL,
		HeadRef:    forge.HeadRef(ev.Source.Platform, ref.Number),
		BaseBranch: pr.Target.Branch,
		Credential: cred,
	}, j.Logger)
	if err != nil {
		return nil, fmt.Errorf("preparing a workspace for %s: %w", ref, err)
	}
	return ws, nil
}

// alreadyReviewed reports whether this exact commit has already been reviewed,
// which happens when a webhook is redelivered or when two events resolve to
// the same head.
func (j *ReviewJob) alreadyReviewed(ctx context.Context, ev *event.ReviewEvent, headSHA string) (bool, error) {
	if j.Store == nil || headSHA == "" {
		return false, nil
	}

	ttl := j.ReviewedTTL
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	first, err := j.Store.MarkProcessed(ctx, store.JobKey(ev, headSHA), ttl)
	if err != nil {
		return false, fmt.Errorf("checking whether %s was reviewed: %w", headSHA, err)
	}
	return !first, nil
}

// post writes the review back to the pull request: one summary comment that is
// replaced on every run, plus the findings as one review.
func (j *ReviewJob) post(ctx context.Context, client forge.Client, ref forge.PRRef, ev *event.ReviewEvent, result *reviewer.Result, sanitized reviewer.Sanitized, headSHA, sinceSHA string, selection triaged, settings resolved) error {
	allowed, err := j.allowPost(ctx, ev)
	if err != nil {
		return err
	}
	if !allowed {
		// Refusing to post is the last line of defence against a comment
		// loop, so it is loud rather than silent.
		j.Logger.LogAttrs(ctx, slog.LevelError, "posting limit reached; not writing to the pull request",
			slog.String("ref", ref.String()),
		)
		return nil
	}

	if err := client.UpsertSummary(ctx, ref, SummaryMarker, j.summaryBody(result, sanitized, headSHA, sinceSHA, selection, settings)); err != nil {
		return fmt.Errorf("posting the summary to %s: %w", ref, err)
	}
	if len(sanitized.Findings) == 0 {
		return nil
	}

	review := forge.Review{CommitSHA: headSHA}
	for _, f := range sanitized.Findings {
		review.Comments = append(review.Comments, forge.InlineComment{
			Path:       f.Path,
			Line:       f.Line,
			EndLine:    f.EndLine,
			Body:       renderFinding(f),
			Suggestion: f.Suggestion,
		})
	}

	err = client.CreateReview(ctx, ref, review)
	if err == nil {
		return nil
	}

	// A rejected position loses the whole review, so the findings are posted
	// as text instead of being lost. This should be rare: positions are
	// checked against the diff first.
	if errors.Is(err, forge.ErrInvalidPosition) {
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "inline comments were rejected; falling back to the summary",
			slog.String("ref", ref.String()),
			slog.String("error", err.Error()),
		)
		body := j.summaryBody(result, sanitized, headSHA, sinceSHA, selection, settings) + "\n\n" + renderFindingsAsText(sanitized.Findings)
		if fallbackErr := client.UpsertSummary(ctx, ref, SummaryMarker, body); fallbackErr != nil {
			return fmt.Errorf("posting the fallback summary to %s: %w", ref, fallbackErr)
		}
		return nil
	}
	return fmt.Errorf("posting the review to %s: %w", ref, err)
}

// allowPost reports whether kibitz may still write to this pull request in the
// current hour. It is the backstop against a loop: if kibitz somehow starts
// reacting to itself, it stops after a bounded number of comments instead of
// filling the thread.
func (j *ReviewJob) allowPost(ctx context.Context, ev *event.ReviewEvent) (bool, error) {
	if j.Store == nil {
		return true, nil
	}
	limit := j.MaxPostsPerHour
	if limit <= 0 {
		limit = 10
	}

	count, err := j.Store.Incr(ctx, store.PostsKey(ev, time.Now()), 1, time.Hour)
	if err != nil {
		// A counter that cannot be read must not stop reviews; the loop is
		// unlikely and the review is the point.
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "could not check the posting limit",
			slog.String("error", err.Error()),
		)
		return true, nil
	}
	return count <= int64(limit), nil
}

// NotifyFailure implements [Notifier]. It replaces its own previous notice
// rather than adding one per attempt.
func (j *ReviewJob) NotifyFailure(ctx context.Context, ev *event.ReviewEvent, cause error) error {
	client, ok := j.Forges[ev.Source.Platform]
	if !ok || ev.PullRequest == nil {
		return nil
	}

	var b strings.Builder
	b.WriteString("レビューに失敗しました。\n\n")
	b.WriteString("```\n")
	b.WriteString(oneLine(cause.Error(), 500))
	b.WriteString("\n```\n\n")
	fmt.Fprintf(&b, "`%s review` で再実行できます。\n", j.mention())

	return client.UpsertSummary(ctx, forge.RefOf(ev), FailureMarker, b.String())
}

// acknowledgeIgnore records the request and says so, because a command that
// silently does nothing is indistinguishable from a broken one.
func (j *ReviewJob) acknowledgeIgnore(ctx context.Context, client forge.Client, ref forge.PRRef, ev *event.ReviewEvent) error {
	if err := j.ignore(ctx, ev); err != nil {
		return fmt.Errorf("recording the ignore request for %s: %w", ref, err)
	}
	j.Logger.LogAttrs(ctx, slog.LevelInfo, "asked to ignore this pull request",
		slog.String("ref", ref.String()),
	)

	body := fmt.Sprintf(
		"この PR は以降レビューしません。\n\n再開するときは `%s review` とコメントしてください。質問には引き続き答えます。\n",
		j.mention())
	return client.UpsertSummary(ctx, ref, IgnoreMarker, body)
}

func (j *ReviewJob) answer(ctx context.Context, client forge.Client, ref forge.PRRef, ev *event.ReviewEvent) error {
	if ev.Comment == nil {
		return nil
	}

	pr, err := client.PullRequest(ctx, ref)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", ref, err)
	}
	settings := j.settingsFor(ctx, client, ref)
	if !settings.AnswerEnabled {
		j.Logger.LogAttrs(ctx, slog.LevelInfo, "the repository's settings turn answers off; skipping",
			slog.String("ref", ref.String()),
		)
		return nil
	}

	if j.reportExhausted(ctx, client, ref, ev) {
		return nil
	}

	diff, err := client.Diff(ctx, ref)
	if err != nil {
		return fmt.Errorf("fetching the diff of %s: %w", ref, err)
	}
	diff, _ = filterPaths(diff, settings.PathsIgnore)

	// A reply of "why?" is answerable only with what it is replying to. That
	// is context, not a requirement: an answer without the thread is worse
	// than one with it, but better than none.
	comments, err := client.Comments(ctx, ref)
	if err != nil {
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "could not list the thread the question is in",
			slog.String("ref", ref.String()),
			slog.String("error", err.Error()),
		)
	}
	thread := threadOf(comments, ev.Comment)

	ws, err := j.prepareWorkspace(ctx, client, ref, ev, pr)
	if err != nil {
		return err
	}
	defer func() { _ = ws.Close() }()

	result, err := j.Engine.Run(ctx, reviewer.Request{
		Mode:         reviewer.ModeAnswer,
		WorkspaceDir: ws.Dir,
		Event:        ev,
		PullRequest:  pr,
		Diff:         diff,
		Question:     ev.Comment.Body,
		Thread:       thread,
		References:   j.referenceDocs(ctx, ws),
		SessionID:    j.session(ctx, ev),
		Language:     settings.Language,
		Model:        settings.Model,
		Guidelines:   settings.Guidelines,
		MCP:          settings.mcp,
		HeadSHA:      ws.HeadSHA,
	})
	if err != nil {
		return err
	}
	j.rememberSession(ctx, ev, result.SessionID)

	j.Logger.LogAttrs(ctx, slog.LevelInfo, "answered a question",
		slog.String("ref", ref.String()),
		slog.Int("thread_comments", len(thread)),
		slog.Int("input_tokens", result.Usage.Input()),
		slog.Int("output_tokens", result.Usage.Output()),
		slog.Int("tool_calls", result.Tools.Total()),
		slog.Int("docs_searched", result.Tools.Searches),
		slog.Any("docs_read", result.Tools.Documents),
	)
	j.recordTools(result.Tools)
	j.charge(ctx, ev, pass{Model: settings.Model, Usage: result.Usage})

	if err := client.ReplyToThread(ctx, ref, ev.Comment.ThreadID, result.Reply); err != nil {
		return fmt.Errorf("replying on %s: %w", ref, err)
	}
	return nil
}

// threadOf picks out the conversation a question belongs to, oldest first and
// without the question itself.
//
// A comment on the diff belongs to its thread; a comment on the conversation
// has no thread of its own, so the conversation is the thread. kibitz's own
// comments are kept either way: the finding being asked about is usually one
// of them.
func threadOf(comments []forge.Comment, question *event.Comment) []forge.Comment {
	if question == nil {
		return nil
	}

	out := make([]forge.Comment, 0, len(comments))
	for _, c := range comments {
		if c.ID == question.ID {
			continue
		}
		if question.ThreadID != "" && c.ThreadID != question.ThreadID {
			continue
		}
		out = append(out, c)
	}
	if len(out) > maxThreadComments {
		out = out[len(out)-maxThreadComments:]
	}
	return out
}

// kibitzExcluded drops kibitz's own comments from the context handed to the
// agent, so it does not treat its previous findings as someone else's review.
func kibitzExcluded(comments []forge.Comment, ev *event.ReviewEvent) []forge.Comment {
	out := make([]forge.Comment, 0, len(comments))
	for _, c := range comments {
		if strings.Contains(c.Body, SummaryMarker) || strings.Contains(c.Body, FailureMarker) {
			continue
		}
		if ev.Comment != nil && c.ID == ev.Comment.ID {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (j *ReviewJob) summaryBody(result *reviewer.Result, sanitized reviewer.Sanitized, headSHA, sinceSHA string, selection triaged, settings resolved) string {
	var b strings.Builder

	b.WriteString(strings.TrimSpace(result.Summary))
	b.WriteString("\n\n")

	if n := len(sanitized.Findings); n == 0 {
		b.WriteString("指摘はありません。\n")
	} else {
		fmt.Fprintf(&b, "指摘: %d 件\n", n)
	}
	// A review that quietly skipped half the change is worse than no review,
	// so the part that was not read is named rather than implied.
	if n := len(selection.Skipped); n > 0 {
		fmt.Fprintf(&b, "\n変更が大きいため、%d 件のファイルを選んでレビューしました (残り %d 件は未レビュー)。\n",
			len(selection.Diff.Files), n)
		if selection.Notes != "" {
			fmt.Fprintf(&b, "\n%s\n", selection.Notes)
		}
		b.WriteString("\n<details><summary>レビューしなかったファイル</summary>\n\n")
		for _, path := range selection.Skipped {
			fmt.Fprintf(&b, "- `%s`\n", path)
		}
		b.WriteString("\n</details>\n")
	}
	if sanitized.OutOfDiff > 0 {
		fmt.Fprintf(&b, "\n_%d 件の指摘は差分に含まれない行を指していたため省略しました。_\n", sanitized.OutOfDiff)
	}
	if sanitized.Excess > 0 {
		fmt.Fprintf(&b, "\n_件数の上限により %d 件を省略しました (重大なものを優先)。_\n", sanitized.Excess)
	}
	if result.RawOutput != nil && result.RawOutput.Notes != "" {
		b.WriteString("\n")
		b.WriteString(result.RawOutput.Notes)
		b.WriteString("\n")
	}

	b.WriteString("\n---\n")
	switch {
	case headSHA != "" && sinceSHA != "":
		fmt.Fprintf(&b, "レビュー対象: `%s`..`%s` (前回レビューからの差分)", shortSHA(sinceSHA), shortSHA(headSHA))
	case headSHA != "":
		fmt.Fprintf(&b, "レビュー対象: `%s`", shortSHA(headSHA))
	}
	if result.Usage.Duration > 0 {
		fmt.Fprintf(&b, " / 所要 %s", result.Usage.Duration.Round(time.Second))
	}
	b.WriteString("\n")
	// A review that needed a triage pass paid for two runs, and both are its
	// bill. The tokens add up, but the money does not: triage may have run
	// on a different model, and pricing its half at the review model's rate
	// would misreport whichever of the two was cheaper.
	j.writeUsage(&b, result.Usage.Add(selection.Usage),
		pass{Model: settings.Model, Usage: result.Usage},
		pass{Model: j.triageModel(settings.Model), Usage: selection.Usage})
	writeSettingsNotes(&b, settings)
	return b.String()
}

// writeUsage reports what the review consumed, and what that costs where
// anybody has said what it costs.
//
// It is in the comment rather than only in the metrics because the person
// deciding whether a bot is worth having is the one reading its comments, and
// "this took 40,000 tokens" is the number that decision turns on.
func (j *ReviewJob) writeUsage(b *strings.Builder, usage reviewer.Usage, passes ...pass) {
	if usage.Tokens() == 0 {
		return
	}

	fmt.Fprintf(b, "トークン: 入力 %s", thousands(usage.Input()))
	// Most of a re-review's input is usually cache, and at a tenth of the
	// price: without this the input count reads as ten times the bill.
	if cached := usage.CacheReadTokens + usage.CacheWriteTokens; cached > 0 {
		fmt.Fprintf(b, " (うちキャッシュ %s)", thousands(cached))
	}
	fmt.Fprintf(b, " / 出力 %s", thousands(usage.Output()))
	if usage.ReasoningTokens > 0 {
		fmt.Fprintf(b, " (うち推論 %s)", thousands(usage.ReasoningTokens))
	}
	if cost, ok := j.cost(passes...); ok {
		// An estimate, and said to be one: it is the configured rates applied
		// to what the agent reported, not a bill.
		fmt.Fprintf(b, " / 概算 %s", money(cost, j.currency()))
	}
	b.WriteString("\n")
}

// currency falls back to the one most model prices are quoted in.
func (j *ReviewJob) currency() string {
	if j.Currency == "" {
		return "$"
	}
	return j.Currency
}

// money renders a cost with enough digits to be worth printing. A review that
// cost a fifth of a cent should not read as "0.00".
func money(amount float64, currency string) string {
	switch {
	case amount >= 100:
		return fmt.Sprintf("%s%.0f", currency, amount)
	case amount >= 1:
		return fmt.Sprintf("%s%.2f", currency, amount)
	case amount >= 0.01:
		return fmt.Sprintf("%s%.3f", currency, amount)
	default:
		return fmt.Sprintf("%s%.4f", currency, amount)
	}
}

// thousands groups a count so that six figures can be read at a glance.
func thousands(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return s
	}

	var b strings.Builder
	for i, digit := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(digit)
	}
	return b.String()
}

func renderFinding(f reviewer.Finding) string {
	var b strings.Builder

	fmt.Fprintf(&b, "**[%s] %s**\n\n", strings.ToUpper(string(f.Severity)), f.Title)
	b.WriteString(f.Body)
	if f.Category != "" {
		fmt.Fprintf(&b, "\n\n_%s_", f.Category)
	}
	return b.String()
}

func renderFindingsAsText(findings []reviewer.Finding) string {
	var b strings.Builder

	b.WriteString("### 指摘\n\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "- **%s:%d** [%s] %s\n", f.Path, f.Line, f.Severity, f.Title)
		fmt.Fprintf(&b, "  %s\n", strings.ReplaceAll(f.Body, "\n", "\n  "))
	}
	return b.String()
}

// oneLine flattens an error for a comment body.
func oneLine(s string, limit int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > limit {
		return s[:limit] + "…"
	}
	return s
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// mention falls back to the same default the server uses, so a worker that
// was not told still prints something true for a default deployment.
func (j *ReviewJob) mention() string {
	if j.Mention == "" {
		return policy.DefaultMention
	}
	return j.Mention
}

func helpText(mention string) string {
	return strings.Join([]string{
		"### kibitz の使い方",
		"",
		fmt.Sprintf("- `%s review` — 差分をレビューします (`--focus security` などで観点を指定できます)", mention),
		fmt.Sprintf("- `%s explain <対象>` — 実装の説明を返します", mention),
		fmt.Sprintf("- `%s <質問>` — 質問に回答します", mention),
		fmt.Sprintf("- `%s ignore` — この PR は以降レビューしません (質問には答えます)", mention),
		"",
		fmt.Sprintf("コマンドは**コメントの先頭**に書いてください。`%s` が途中にある場合は質問として扱います。", mention),
		"",
		"指摘への返信で追加の質問もできます。",
	}, "\n")
}
