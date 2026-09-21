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
	"github.com/yteraoka/kibitz/internal/workspace"
)

// SummaryMarker identifies kibitz's own summary comment, so that reviewing a
// pull request again replaces it instead of adding another one.
const SummaryMarker = "<!-- kibitz:summary -->"

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
	// SkipDraft leaves draft pull requests alone until they are marked ready.
	SkipDraft bool
}

// Handle implements [Handler].
func (j *ReviewJob) Handle(ctx context.Context, ev *event.ReviewEvent) error {
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
		return client.UpsertSummary(ctx, ref, "<!-- kibitz:help -->", helpText())
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

	diff, err := client.Diff(ctx, ref)
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

	result, err := j.runWithRetry(ctx, req)
	if err != nil {
		return err
	}

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

	return j.post(ctx, client, ref, result, sanitized, ws.HeadSHA)
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

// post writes the review back to the pull request: one summary comment that is
// replaced on every run, plus the findings as one review.
func (j *ReviewJob) post(ctx context.Context, client forge.Client, ref forge.PRRef, result *reviewer.Result, sanitized reviewer.Sanitized, headSHA string) error {
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

	err := client.CreateReview(ctx, ref, review)
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
		if strings.Contains(c.Body, SummaryMarker) {
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

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func helpText() string {
	return strings.Join([]string{
		"### kibitz の使い方",
		"",
		"- `@kibitz review` — 差分をレビューします (`--focus security` などで観点を指定できます)",
		"- `@kibitz explain <対象>` — 実装の説明を返します",
		"- `@kibitz <質問>` — 質問に回答します",
		"",
		"指摘への返信で追加の質問もできます。",
	}, "\n")
}
