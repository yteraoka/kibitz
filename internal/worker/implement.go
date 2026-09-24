package worker

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/policy"
	"github.com/yteraoka/kibitz/internal/repoconfig"
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

	return nil
}

// issueCommand handles an instruction given on an issue.
//
// Today every path through it ends in a refusal: the gate is here and the
// work is not. That is deliberate — the conditions under which this mode may
// write code are worth having settled, and tested, before there is anything
// that writes.
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

		// Everything the gate can check has passed. What comes next -- the
		// branch, the agent, the sandbox, the draft pull request -- is not
		// built yet, and saying so is better than silence on an issue
		// somebody is waiting on.
		j.Logger.LogAttrs(ctx, slog.LevelInfo, "implement mode was allowed but is not implemented yet",
			slog.String("ref", ref.String()),
			slog.String("actor", ev.Actor.Login),
			slog.String("command", name),
		)
		return j.sayOnIssue(ctx, ev, ref,
			"実装モードの実行条件は満たしていますが、**この kibitz のビルドではまだ実装処理が入っていません**。\n\n"+
				"許可の判定 (運用側の有効化・リポジトリ側の有効化・指示者・編集可能パス) までは通っています。\n")

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
| `+"`%s plan`"+` | 同上の計画だけを書く (未実装) |
| `+"`%s help`"+` | これ |

実装モードを使うには、デフォルトブランチの `+"`%s`"+` に次が要ります。

`+"```yaml"+`
implement:
  enabled: true
  allowed_actors: [alice, bob]   # 指示してよい人
  paths_allow: ["internal/**"]   # 編集してよいパス
  commands_allow: ["go test ./..."]
`+"```"+`

CI 設定・`+"`%s`"+`・依存定義ファイルは、設定に関わらず編集できません。
`, mention, mention, mention, repoconfig.Path, repoconfig.Path)
}
