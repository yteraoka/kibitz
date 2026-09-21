package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/policy"
	"github.com/yteraoka/kibitz/internal/reviewer"
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
	HelpMarker    = "<!-- kibitz:help -->"
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
		// Session cleanup lands with the state store in Phase 3.
		return nil

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
		return j.review(ctx, client, ref, ev)
	case policy.CommandAnswer, policy.CommandExplain:
		return j.answer(ctx, client, ref, ev)
	case policy.CommandHelp:
		return client.UpsertSummary(ctx, ref, HelpMarker, helpText(j.mention()))
	case policy.CommandIgnore, policy.CommandImplement, policy.CommandPlan:
		// Ignore needs the state store (Phase 3); implement is Phase 8.
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
	if pr.Draft && j.SkipDraft && ev.Kind != event.KindCommand {
		j.Logger.LogAttrs(ctx, slog.LevelInfo, "pull request is a draft; skipping", slog.String("ref", ref.String()))
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
	if len(diff.Files) == 0 {
		j.Logger.LogAttrs(ctx, slog.LevelInfo, "nothing to review", slog.String("ref", ref.String()))
		return nil
	}

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

	req := reviewer.Request{
		Mode:             reviewer.ModeReview,
		WorkspaceDir:     ws.Dir,
		Event:            ev,
		PullRequest:      pr,
		Diff:             diff,
		ExistingComments: kibitzExcluded(comments, ev),
		Language:         j.Language,
		Model:            j.Model,
		Guidelines:       j.Guidelines,
		HeadSHA:          ws.HeadSHA,
	}

	agentCtx, agentSpan := telemetry.Tracer().Start(ctx, "review.agent",
		trace.WithAttributes(attribute.String("kibitz.model", j.Model)))
	result, err := j.runWithRetry(agentCtx, req)
	if err != nil {
		agentSpan.RecordError(err)
		agentSpan.SetStatus(codes.Error, err.Error())
		agentSpan.End()
		return err
	}
	agentSpan.SetAttributes(
		attribute.Int("kibitz.input_tokens", result.Usage.InputTokens),
		attribute.Int("kibitz.output_tokens", result.Usage.OutputTokens),
	)
	agentSpan.End()

	sanitized := reviewer.Sanitize(result.RawOutput, reviewer.NewPositions(diff), j.Limits)
	j.Logger.LogAttrs(ctx, slog.LevelInfo, "review produced findings",
		slog.String("ref", ref.String()),
		slog.String("head", ws.HeadSHA),
		slog.Int("findings", len(sanitized.Findings)),
		slog.Int("dropped", sanitized.Dropped()),
		slog.Int("out_of_diff", sanitized.OutOfDiff),
		slog.Int("input_tokens", result.Usage.InputTokens),
		slog.Int("output_tokens", result.Usage.OutputTokens),
		slog.Duration("agent_duration", result.Usage.Duration),
	)

	j.record(ev, result, sanitized)

	ctx, postSpan := telemetry.Tracer().Start(ctx, "review.post",
		trace.WithAttributes(telemetry.AttrFindings.Int(len(sanitized.Findings))))
	defer postSpan.End()
	return j.post(ctx, client, ref, ev, result, sanitized, ws.HeadSHA)
}

// record reports what the run produced, which is how cost and noise are
// tracked over time.
func (j *ReviewJob) record(ev *event.ReviewEvent, result *reviewer.Result, sanitized reviewer.Sanitized) {
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
	if model := j.Model; model != "" {
		j.Metrics.AgentTokens.WithLabelValues(model, "input").Add(float64(result.Usage.InputTokens))
		j.Metrics.AgentTokens.WithLabelValues(model, "output").Add(float64(result.Usage.OutputTokens))
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
func (j *ReviewJob) post(ctx context.Context, client forge.Client, ref forge.PRRef, ev *event.ReviewEvent, result *reviewer.Result, sanitized reviewer.Sanitized, headSHA string) error {
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

	if err := client.UpsertSummary(ctx, ref, SummaryMarker, j.summaryBody(result, sanitized, headSHA)); err != nil {
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
		body := j.summaryBody(result, sanitized, headSHA) + "\n\n" + renderFindingsAsText(sanitized.Findings)
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

func (j *ReviewJob) answer(ctx context.Context, client forge.Client, ref forge.PRRef, ev *event.ReviewEvent) error {
	if ev.Comment == nil {
		return nil
	}

	pr, err := client.PullRequest(ctx, ref)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", ref, err)
	}
	diff, err := client.Diff(ctx, ref)
	if err != nil {
		return fmt.Errorf("fetching the diff of %s: %w", ref, err)
	}

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
		Language:     j.Language,
		Model:        j.Model,
		Guidelines:   j.Guidelines,
		HeadSHA:      ws.HeadSHA,
	})
	if err != nil {
		return err
	}

	if err := client.ReplyToThread(ctx, ref, ev.Comment.ThreadID, result.Reply); err != nil {
		return fmt.Errorf("replying on %s: %w", ref, err)
	}
	return nil
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

func (j *ReviewJob) summaryBody(result *reviewer.Result, sanitized reviewer.Sanitized, headSHA string) string {
	var b strings.Builder

	b.WriteString(strings.TrimSpace(result.Summary))
	b.WriteString("\n\n")

	if n := len(sanitized.Findings); n == 0 {
		b.WriteString("指摘はありません。\n")
	} else {
		fmt.Fprintf(&b, "指摘: %d 件\n", n)
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
	if headSHA != "" {
		fmt.Fprintf(&b, "レビュー対象: `%s`", shortSHA(headSHA))
	}
	if result.Usage.Duration > 0 {
		fmt.Fprintf(&b, " / 所要 %s", result.Usage.Duration.Round(time.Second))
	}
	b.WriteString("\n")
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
		"",
		fmt.Sprintf("コマンドは**コメントの先頭**に書いてください。`%s` が途中にある場合は質問として扱います。", mention),
		"",
		"指摘への返信で追加の質問もできます。",
	}, "\n")
}
