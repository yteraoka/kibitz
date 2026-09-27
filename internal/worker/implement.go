package worker

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/policy"
	"github.com/yteraoka/kibitz/internal/repoconfig"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/telemetry"
	"github.com/yteraoka/kibitz/internal/workspace"
)

// Implement mode is the one that writes code, and it is the one place where
// text somebody else wrote turns into a commit. Everything in this file is
// about the conditions under which that is allowed to happen; what it then
// does is elsewhere.
//
// The gate is deliberately made of separate checks that each say no on their
// own, rather than one expression. A reader has to be able to see which
// condition failed, and an operator reading the log has to be told which one
// without having to reproduce it.

// deniedPaths are never editable, whatever the repository's settings say.
//
// These are the files that decide how the next run behaves: the CI
// definitions, kibitz's own settings, and the dependency manifests. A mode
// that could edit them could grant itself anything it currently lacks —
// including, in the case of .kibitz.yaml, the right to edit them.
//
// They are not configurable for that reason. A repository-side override of
// "do not edit the repository-side settings" is not a setting, it is a hole.
var deniedPaths = []string{
	// CI, on all three platforms.
	".github/workflows/**",
	".github/actions/**",
	".gitlab-ci.yml",
	"**/.gitlab-ci.yml",
	"azure-pipelines.yml",
	"**/azure-pipelines.yml",
	".azuredevops/**",
	".circleci/**",
	"Jenkinsfile",
	// kibitz's own settings, and the conventions the prompt is built from.
	".kibitz.yaml",
	".kibitz/**",
	"AGENTS.md",
	// Dependency manifests and lock files. Adding a dependency is running
	// somebody else's code on the next build, which is a decision for a
	// person (docs/worker.md).
	"go.mod",
	"go.sum",
	"package.json",
	"package-lock.json",
	"pnpm-lock.yaml",
	"yarn.lock",
	"requirements.txt",
	"pyproject.toml",
	"poetry.lock",
	"uv.lock",
	"Cargo.toml",
	"Cargo.lock",
	"Gemfile",
	"Gemfile.lock",
	"composer.json",
	"composer.lock",
	"pom.xml",
	"build.gradle",
	"build.gradle.kts",
	// Anything that is itself a credential, wherever it is.
	"**/*.pem",
	"**/*.key",
	".env",
	"**/.env",
}

// denied is the compiled form of the list above. It is built once: the
// patterns are constants, and a matcher rebuilt per file would be the same
// answer at a worse price.
//
// The matching is repoconfig's, not a second implementation of it. Two glob
// matchers in one program is two sets of edge cases, and this is the one
// where getting an edge case wrong means a file that should never be written
// gets written.
// The patterns are lowercased, and so is every path checked against them.
// A path is matched as the repository spells it everywhere else, but not
// here: on a case-insensitive checkout ".github/Workflows/ci.yml" is the
// workflow file, and a deny list that only knew the lowercase spelling would
// be one capital letter away from useless.
var denied = mustFilter(lowerAll(deniedPaths))

func lowerAll(patterns []string) []string {
	out := make([]string, len(patterns))
	for i, pattern := range patterns {
		out[i] = strings.ToLower(pattern)
	}
	return out
}

func mustFilter(patterns []string) *repoconfig.PathFilter {
	filter, err := repoconfig.NewPathFilter(patterns)
	if err != nil {
		// The patterns are compiled into the binary, so this cannot depend on
		// input. It failing means this file is wrong.
		panic("worker: the denied path list does not compile: " + err.Error())
	}
	return filter
}

// Denied reports whether a path may never be edited, whatever the repository
// allows.
func Denied(filePath string) bool {
	clean, ok := cleanRepoPath(filePath)
	if !ok {
		return true
	}
	return denied.Match(strings.ToLower(clean))
}

// cleanRepoPath normalizes a path and reports whether it names something
// inside the repository at all. Anything that climbs out, or that names the
// repository itself, is not a file this mode may write.
func cleanRepoPath(filePath string) (string, bool) {
	clean := strings.TrimPrefix(path.Clean(strings.TrimSpace(filePath)), "/")
	switch {
	case clean == "" || clean == "." || clean == "..":
		return "", false
	case strings.HasPrefix(clean, "../"):
		return "", false
	}
	return clean, true
}

// Editable reports whether implement mode may write to a path.
//
// Both halves have to agree: the repository has to have named it, and it must
// not be one of the paths nothing may touch. The allow filter is passed in
// already compiled, because it is the same for every file in one job and
// compiling it per file would be the only expensive thing here.
func Editable(allow *repoconfig.PathFilter, filePath string) bool {
	clean, ok := cleanRepoPath(filePath)
	if !ok {
		return false
	}
	if denied.Match(strings.ToLower(clean)) {
		return false
	}
	// The allow list is matched as written: a repository naming its own
	// directories knows how it spells them, and being lenient here would
	// widen what may be written rather than narrow it.
	return allow.Match(clean)
}

// implementCommands are the instructions this gate answers for. Anything
// else reaching it is a routing mistake, not a refusal worth reporting.
var implementCommands = map[string]bool{
	policy.CommandImplement: true,
	policy.CommandPlan:      true,
}

// refusal is why implement mode did not run. It carries a reason for the log
// and a sentence for the issue, because the person who asked is the one who
// has to fix it and the log is not somewhere they can see.
type refusal struct {
	Reason string
	Say    string
}

// Refusal reasons, which are what the log and the metrics count.
const (
	refusedDisabledHere    = "implement_disabled_deployment"
	refusedDisabledRepo    = "implement_disabled_repository"
	refusedActorNotAllowed = "implement_actor_not_allowed"
	refusedNoPaths         = "implement_no_paths_allowed"
	refusedNotAnIssue      = "implement_not_an_issue"
	refusedNotACommand     = "implement_not_a_command"
	refusedNoCommands      = "implement_no_commands_allowed"
	// The three below are counted after the agent has run, which is why they
	// are counted apart: the model call has been paid for and there is still no
	// pull request. Together they are what tells an operator whether the mode
	// is being refused by its configuration or by its own output.
	refusedNoChanges      = "implement_no_changes"
	refusedPathNotAllowed = "implement_path_not_allowed"
	refusedVerifyFailed   = "implement_verification_failed"
)

// allowImplement decides whether an instruction may run, and says why not
// when it may not.
//
// The order matters for what gets said rather than for the answer: the most
// basic condition is reported first, so somebody who has not turned the mode
// on is told that rather than being told their name is not on a list they
// have not written yet.
func (j *ReviewJob) allowImplement(ev *event.ReviewEvent, settings repoconfig.ImplementSettings) *refusal {
	if !j.ImplementEnabled {
		return &refusal{
			Reason: refusedDisabledHere,
			Say:    "この kibitz では実装モードが有効になっていません。運用者に `KIBITZ_IMPLEMENT_ENABLED` の設定を依頼してください。",
		}
	}
	if !settings.Enabled {
		return &refusal{
			Reason: refusedDisabledRepo,
			Say: "このリポジトリでは実装モードが有効になっていません。デフォルトブランチの `" +
				repoconfig.Path + "` に `implement.enabled: true` を書いてください。",
		}
	}

	// An issue is where this mode is asked for. A pull request already has a
	// branch and a diff; changing it is a different operation with different
	// consequences, and it is not this one.
	if ev.Issue == nil {
		return &refusal{
			Reason: refusedNotAnIssue,
			Say:    "実装モードは Issue 上でのみ使えます。",
		}
	}
	// Both commands come through here, and both are gated the same way: a
	// plan reads the code and costs a model call, which is enough to want the
	// same answer about who may ask for it.
	if ev.Command == nil || !implementCommands[ev.Command.Name] {
		return &refusal{Reason: refusedNotACommand, Say: ""}
	}

	// The instruction is what is checked, not the issue. Whoever wrote the
	// issue and whoever asked for it to be built may be different people, and
	// the issue's text is data either way (ADR-0010).
	if !settings.Allows(ev.Actor.Login) {
		return &refusal{
			Reason: refusedActorNotAllowed,
			Say: fmt.Sprintf(
				"`%s` は `%s` の `implement.allowed_actors` に含まれていないため、この指示では動きません。",
				ev.Actor.Login, repoconfig.Path),
		}
	}

	// Without a path list there is nothing it may write, so running the agent
	// would burn a model call to produce a change that could not be applied.
	if len(settings.PathsAllow) == 0 {
		return &refusal{
			Reason: refusedNoPaths,
			Say: "編集してよいパスが 1 つも宣言されていません。`" + repoconfig.Path +
				"` の `implement.paths_allow` に、変更を許すパスを書いてください。",
		}
	}

	// Only writing needs a way of being checked. A plan is prose, and nothing
	// verifies prose.
	//
	// It is required rather than defaulted because a change kibitz wrote and
	// nothing built must not become a pull request, and there is no command
	// kibitz could guess: "go test ./..." is wrong in a repository that is not
	// Go, and a default that silently verified nothing would be worse than
	// refusing to run.
	if ev.Command.Name == policy.CommandImplement && len(settings.CommandsAllow) == 0 {
		return &refusal{
			Reason: refusedNoCommands,
			Say: "検証に使うコマンドが宣言されていません。`" + repoconfig.Path +
				"` の `implement.commands_allow` に、ビルドとテストのコマンドを書いてください。\n\n" +
				"書いた変更がそれらを通らなければ、プルリクエストは作りません。",
		}
	}

	return nil
}

// issueCommand handles an instruction given on an issue.
//
// The gate above decides whether anything happens at all; what happens once it
// has is in implement_run.go. Nothing reaches that which has not passed every
// check in [ReviewJob.allowImplement].
func (j *ReviewJob) issueCommand(ctx context.Context, client forge.Client, ev *event.ReviewEvent) error {
	ref, ok := forge.IssueRefOf(ev)
	if !ok {
		// Validate should have caught this at the webhook. Reaching here
		// means the two disagree, which is worth a line rather than a panic.
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "an issue command arrived without an issue",
			slog.String("event_id", ev.ID),
		)
		return nil
	}

	name := ""
	if ev.Command != nil {
		name = ev.Command.Name
	}

	switch name {
	case policy.CommandHelp:
		return j.sayOnIssue(ctx, ev, ref, issueHelpText(j.mention()))

	case policy.CommandImplement, policy.CommandPlan:
		// The repository's own settings, read from its default branch. The
		// issue cannot influence them, which is the point (ADR-0011).
		settings := j.settingsFor(ctx, client, ref.Repository())

		if refused := j.allowImplement(ev, settings.Implement); refused != nil {
			return j.refuse(ctx, ev, ref, refused)
		}

		if name == policy.CommandPlan {
			return j.plan(ctx, client, ref, ev, settings)
		}

		return j.implement(ctx, client, ref, ev, settings)

	default:
		return nil
	}
}

// refuse says why, and counts it.
func (j *ReviewJob) refuse(ctx context.Context, ev *event.ReviewEvent, ref forge.IssueRef, r *refusal) error {
	j.Logger.LogAttrs(ctx, slog.LevelInfo, "refusing an implement instruction",
		slog.String("ref", ref.String()),
		slog.String("actor", ev.Actor.Login),
		slog.String("reason", r.Reason),
	)
	if j.Metrics != nil {
		j.Metrics.ImplementRefused.WithLabelValues(r.Reason).Inc()
	}
	if r.Say == "" {
		// Nothing useful to tell anybody: the comment was not addressed to
		// this mode in the first place.
		return nil
	}
	return j.sayOnIssue(ctx, ev, ref, r.Say)
}

// sayOnIssue posts kibitz's one comment on an issue, replacing whatever it
// said last time.
func (j *ReviewJob) sayOnIssue(ctx context.Context, ev *event.ReviewEvent, ref forge.IssueRef, body string) error {
	client, ok := j.Forges[ev.Source.Platform].(forge.IssueClient)
	if !ok {
		// The platform's client cannot write to issues yet. Not a failure of
		// this job, and not something a retry improves.
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "this platform's client cannot comment on issues",
			slog.String("platform", string(ev.Source.Platform)),
			slog.String("ref", ref.String()),
		)
		return nil
	}

	allowed, err := j.allowPost(ctx, ev)
	if err != nil {
		return err
	}
	if !allowed {
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "hit the posting limit on this issue; staying quiet",
			slog.String("ref", ref.String()),
		)
		return nil
	}

	return client.UpsertIssueComment(ctx, ref, ImplementMarker, body)
}

// issueHelpText is the help for what kibitz does on an issue, which is a
// shorter list than what it does on a pull request.
func issueHelpText(mention string) string {
	return fmt.Sprintf(`Issue 上で使えるコマンド:

| コマンド | 内容 |
| --- | --- |
| `+"`%s implement`"+` | Issue の内容からコードを書き、draft PR を作る (既定は無効) |
| `+"`%s plan`"+` | 同上の計画だけを書く (コードは変更しない) |
| `+"`%s help`"+` | これ |

実装モードを使うには、デフォルトブランチの `+"`%s`"+` に次が要ります。

`+"```yaml"+`
implement:
  enabled: true
  allowed_actors: [alice, bob]       # 指示してよい人
  paths_allow: ["internal/**"]       # 編集してよいパス
  commands_allow: ["go test ./..."]  # 検証に使うコマンド
`+"```"+`

CI 設定・`+"`%s`"+`・依存定義ファイルは、設定に関わらず編集できません。

`+"`commands_allow`"+` が通らなかった変更は、プルリクエストになりません。
`, mention, mention, mention, repoconfig.Path, repoconfig.Path)
}

// plan writes what implementing an issue would take, and says so on the issue.
//
// It writes no code and creates no branch, so it needs none of what implement
// mode still lacks: the agent runs under the same read-only permissions that
// answer a question, on a checkout of the default branch.
func (j *ReviewJob) plan(ctx context.Context, client forge.Client, ref forge.IssueRef, ev *event.ReviewEvent, settings resolved) error {
	ctx, span := telemetry.Tracer().Start(ctx, "issue.plan")
	defer span.End()

	ws, err := j.prepareIssueWorkspace(ctx, client, ref, ev)
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

	result, err := j.Engine.Run(ctx, reviewer.Request{
		Mode:         reviewer.ModePlan,
		WorkspaceDir: ws.Dir,
		Event:        ev,
		Issue:        ev.Issue,
		References:   j.referenceDocs(ctx, ws),
		Language:     settings.Language,
		Model:        settings.Model,
		Guidelines:   settings.Guidelines,
		MCP:          settings.mcp,
		HeadSHA:      ws.HeadSHA,
	})
	if err != nil {
		return err
	}

	pass := pass{Model: settings.Model, Usage: result.Usage}
	j.Logger.LogAttrs(ctx, slog.LevelInfo, "planned an issue",
		slog.String("repository", ev.Repository.FullName),
		slog.String("model", settings.Model),
		slog.String("ref", ref.String()),
		slog.String("head", ws.HeadSHA),
		slog.Int("input_tokens", result.Usage.Input()),
		slog.Int("output_tokens", result.Usage.Output()),
		slog.Int("total_tokens", result.Usage.Tokens()),
		slog.Int("tool_calls", result.Tools.Total()),
		slog.Attr{Key: "cost", Value: costValue(j.cost(pass))},
	)
	j.recordTools(result.Tools)
	j.charge(ctx, ev, pass)

	return j.sayOnIssue(ctx, ev, ref, j.planComment(result, ws.HeadSHA, settings, pass))
}

// planComment wraps the plan in what a reader needs to judge it: that a model
// wrote it, which commit it was written against, and what it cost.
func (j *ReviewJob) planComment(result *reviewer.Result, headSHA string, settings resolved, p pass) string {
	var b strings.Builder

	b.WriteString("## 実装の計画\n\n")
	b.WriteString("**これは AI が書いた計画で、コードはまだ何も変更していません。**\n")
	b.WriteString("実装するかどうか、この形でよいかは人が判断してください。\n\n")
	b.WriteString(strings.TrimSpace(result.Reply))
	b.WriteString("\n\n---\n")
	if headSHA != "" {
		fmt.Fprintf(&b, "対象: `%s` (デフォルトブランチ)", shortSHA(headSHA))
	}
	if result.Usage.Duration > 0 {
		fmt.Fprintf(&b, " / 所要 %s", result.Usage.Duration.Round(time.Second))
	}
	b.WriteString("\n")
	j.writeUsage(&b, result.Usage, p)
	writeSettingsNotes(&b, settings)
	return b.String()
}

// prepareIssueWorkspace checks out the default branch.
//
// An issue has no branch of its own, and the default branch is the one a
// change would be based on. Nothing from the issue chooses what is checked
// out: a ref named in the issue's text would be somebody else's code running
// under kibitz's credentials.
func (j *ReviewJob) prepareIssueWorkspace(ctx context.Context, client forge.Client, ref forge.IssueRef, ev *event.ReviewEvent) (*workspace.Workspace, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "issue.workspace")
	defer span.End()

	cred, err := client.CloneAuth(ctx, ref.Repository())
	if err != nil {
		return nil, fmt.Errorf("getting clone credentials for %s: %w", ref, err)
	}
	if ev.Repository.CloneURL == "" {
		return nil, fmt.Errorf("no clone url for %s", ref)
	}

	branch := ev.Repository.DefaultBranch
	if branch == "" {
		// Not worth guessing: a wrong branch reads as an empty repository,
		// and a plan written against nothing is worse than no plan.
		return nil, Permanent(fmt.Errorf("%s: the event does not say what the default branch is", ref))
	}

	ws, err := workspace.Prepare(ctx, j.Workspace, workspace.Spec{
		CloneURL:   ev.Repository.CloneURL,
		HeadRef:    "refs/heads/" + branch,
		BaseBranch: branch,
		Credential: cred,
	}, j.Logger)
	if err != nil {
		return nil, fmt.Errorf("preparing a workspace for %s: %w", ref, err)
	}
	return ws, nil
}
