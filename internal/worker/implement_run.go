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
	"github.com/yteraoka/kibitz/internal/repoconfig"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/sandbox"
	"github.com/yteraoka/kibitz/internal/telemetry"
	"github.com/yteraoka/kibitz/internal/workspace"
)

// What implement mode does once the gate in implement.go has let it through.
//
// The order of what follows is the whole design. Every step that can say no
// comes before the one that costs something, and the two steps that decide
// whether a pull request exists -- what was edited, and whether it builds --
// both come before anything is pushed. A change that fails either of them
// leaves nothing behind but a comment saying so.

// Defaults for implement mode.
const (
	// DefaultBranchPrefix starts the branch names implement mode pushes. The
	// slash matters: it puts everything kibitz creates in one namespace, which
	// is what a branch protection rule or a person cleaning up can select on.
	DefaultBranchPrefix = "kibitz/"
	// DefaultCommitName and DefaultCommitEmail attribute the commits. The
	// noreply address is deliberate: it is not a mailbox, and a commit author
	// that looked like a person's would be a lie about who wrote the code.
	DefaultCommitName  = "kibitz"
	DefaultCommitEmail = "kibitz@users.noreply.github.com"
)

// implement writes the change an issue asks for, verifies it where kibitz holds
// no credentials, and opens a draft pull request if it passed.
func (j *ReviewJob) implement(ctx context.Context, client forge.Client, ref forge.IssueRef, ev *event.ReviewEvent, settings resolved) error {
	ctx, span := telemetry.Tracer().Start(ctx, "issue.implement")
	defer span.End()

	writer, ok := j.Forges[ev.Source.Platform].(forge.Writer)
	if !ok {
		// Not a failure and not worth retrying: this build cannot open a pull
		// request on this platform, and running the agent first would spend a
		// model call to produce a change with nowhere to go.
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "this platform's client cannot open pull requests",
			slog.String("platform", string(ev.Source.Platform)),
			slog.String("ref", ref.String()),
		)
		return j.sayOnIssue(ctx, ev, ref,
			"このプラットフォームではまだプルリクエストを作成できません。")
	}
	if j.Sandbox == nil {
		// Fail closed. A change nobody built must not become a pull request,
		// and a deployment that enabled the mode without configuring where it
		// verifies has a configuration problem rather than a broken issue.
		j.Logger.LogAttrs(ctx, slog.LevelError, "implement mode is enabled but has nowhere to verify",
			slog.String("ref", ref.String()),
		)
		return j.sayOnIssue(ctx, ev, ref,
			"実装した変更を検証する環境が設定されていないため、実行できません。\n"+
				"運用者に `KIBITZ_SANDBOX_LOCATION` の設定を依頼してください。")
	}

	commands, err := sandbox.ParseCommands(settings.Implement.CommandsAllow)
	if err != nil {
		// The repository's own setting is wrong, and the person who can fix it
		// is reading the issue.
		return j.sayOnIssue(ctx, ev, ref, "`"+repoconfig.Path+"` の `implement.commands_allow` が使えません: "+err.Error())
	}

	allow, err := settings.Implement.AllowFilter()
	if err != nil {
		return j.sayOnIssue(ctx, ev, ref, "`"+repoconfig.Path+"` の `implement.paths_allow` が使えません: "+err.Error())
	}

	branch := j.branchName(settings.Implement.BranchPrefix, ref.Number)
	repo := ref.Repository()

	// Asked before anything is spent. A redelivered message, or a second
	// instruction on the same issue, finds the pull request the first one
	// opened rather than paying for the change twice.
	if existing, err := writer.FindPullRequest(ctx, repo, branch); err != nil {
		return fmt.Errorf("looking for an existing pull request on %s: %w", branch, err)
	} else if existing != nil {
		j.Logger.LogAttrs(ctx, slog.LevelInfo, "this issue already has a pull request",
			slog.String("ref", ref.String()),
			slog.String("branch", branch),
			slog.Int("pull_request", existing.Number),
		)
		return j.sayOnIssue(ctx, ev, ref, fmt.Sprintf(
			"この Issue のブランチ `%s` には、すでにプルリクエストがあります: %s\n\n"+
				"書き直す場合は、そのプルリクエストとブランチを閉じてから、もう一度指示してください。",
			branch, existing.URL))
	}

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

	// The branch is checked on the remote rather than only through the pull
	// request search: a branch with no pull request, left behind by an attempt
	// that failed after pushing, is still not something to overwrite.
	if exists, err := ws.BranchExists(ctx, branch); err != nil {
		return fmt.Errorf("checking whether %s exists: %w", branch, err)
	} else if exists {
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "the branch already exists; not overwriting it",
			slog.String("ref", ref.String()),
			slog.String("branch", branch),
		)
		return j.sayOnIssue(ctx, ev, ref, fmt.Sprintf(
			"ブランチ `%s` がすでに存在します。**上書きはしません。**\n\n"+
				"不要であれば削除してから、もう一度指示してください。", branch))
	}

	result, err := j.Engine.Run(ctx, reviewer.Request{
		Mode:           reviewer.ModeImplement,
		WorkspaceDir:   ws.Dir,
		Event:          ev,
		Issue:          ev.Issue,
		References:     j.referenceDocs(ctx, ws),
		Language:       settings.Language,
		Model:          settings.Model,
		Guidelines:     settings.Guidelines,
		MCP:            settings.mcp,
		HeadSHA:        ws.HeadSHA,
		EditablePaths:  settings.Implement.PathsAllow,
		VerifyCommands: settings.Implement.CommandsAllow,
	})
	if err != nil {
		return err
	}

	p := pass{Model: settings.Model, Usage: result.Usage}
	j.Logger.LogAttrs(ctx, slog.LevelInfo, "implement mode finished writing",
		slog.String("repository", ev.Repository.FullName),
		slog.String("ref", ref.String()),
		slog.String("model", settings.Model),
		slog.String("head", ws.HeadSHA),
		slog.Int("input_tokens", result.Usage.Input()),
		slog.Int("output_tokens", result.Usage.Output()),
		slog.Int("tool_calls", result.Tools.Total()),
		slog.Attr{Key: "cost", Value: costValue(j.cost(p))},
	)
	j.recordTools(result.Tools)
	// Charged here rather than after the pull request: the model call has
	// happened, and a verification that fails does not make it free.
	j.charge(ctx, ev, p)

	// kibitz's own prompt lives in the checkout, and it is not a change the
	// agent made. Only untracked files go, so a repository that tracks
	// something under that directory keeps it.
	if err := ws.Discard(ctx, reviewer.ScratchDir); err != nil {
		return fmt.Errorf("removing kibitz's own files from the checkout: %w", err)
	}

	changed, err := ws.Changes(ctx)
	if err != nil {
		return fmt.Errorf("finding out what changed: %w", err)
	}
	if len(changed) == 0 {
		j.Logger.LogAttrs(ctx, slog.LevelInfo, "implement mode changed nothing",
			slog.String("ref", ref.String()),
		)
		if j.Metrics != nil {
			j.Metrics.ImplementRefused.WithLabelValues(refusedNoChanges).Inc()
		}
		return j.sayOnIssue(ctx, ev, ref, j.noChangesComment(result, ws.HeadSHA, settings, p))
	}

	if outside := notEditable(allow, changed); len(outside) > 0 {
		// Everything is discarded, not just the paths that were out of bounds.
		// A change that had to write outside what the repository allows is not
		// a change minus those files: what is left would be the half of an
		// edit whose other half was refused.
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "implement mode wrote outside the allowed paths",
			slog.String("ref", ref.String()),
			slog.Any("paths", outside),
			slog.Int("changed", len(changed)),
		)
		if j.Metrics != nil {
			j.Metrics.ImplementRefused.WithLabelValues(refusedPathNotAllowed).Inc()
		}
		return j.sayOnIssue(ctx, ev, ref, j.outsidePathsComment(outside, changed, result, settings, p))
	}

	verified, err := j.verify(ctx, ws.Dir, commands)
	if err != nil {
		// kibitz's own machinery failed -- the tree could not be uploaded, the
		// job could not be started. Worth retrying, so the message goes back.
		return err
	}
	if !verified.Passed() {
		j.Logger.LogAttrs(ctx, slog.LevelInfo, "the change did not pass verification; not opening a pull request",
			slog.String("ref", ref.String()),
			slog.Int("steps", len(verified.Steps)),
			slog.String("failure", verified.Failure),
		)
		if j.Metrics != nil {
			j.Metrics.ImplementRefused.WithLabelValues(refusedVerifyFailed).Inc()
		}
		return j.sayOnIssue(ctx, ev, ref, j.verifyFailedComment(verified, changed, result, settings, p))
	}

	commit, err := ws.Record(ctx, workspace.Commit{
		Message: j.commitMessage(ev, ref),
		Name:    j.commitName(),
		Email:   j.commitEmail(),
	})
	if err != nil {
		if errors.Is(err, workspace.ErrNoChanges) {
			// Only reachable if something removed the changes between the two
			// steps, which is worth a line rather than a pull request.
			return j.sayOnIssue(ctx, ev, ref, "変更が残っていなかったため、プルリクエストは作りませんでした。")
		}
		return fmt.Errorf("committing the change for %s: %w", ref, err)
	}
	if err := ws.Push(ctx, branch); err != nil {
		return fmt.Errorf("pushing %s: %w", branch, err)
	}

	created, err := writer.CreatePullRequest(ctx, repo, forge.NewPullRequest{
		Head:  branch,
		Base:  ev.Repository.DefaultBranch,
		Title: j.pullRequestTitle(ev, ref),
		Body:  j.pullRequestBody(result, ref, verified, changed, settings, p),
		Draft: true,
	})
	if err != nil {
		return fmt.Errorf("opening a pull request for %s: %w", ref, err)
	}

	j.Logger.LogAttrs(ctx, slog.LevelInfo, "implement mode opened a pull request",
		slog.String("ref", ref.String()),
		slog.String("branch", branch),
		slog.String("commit", shortSHA(commit)),
		slog.Int("pull_request", created.Number),
		slog.Bool("draft", created.Draft),
		slog.Int("files", len(changed)),
	)
	if j.Metrics != nil {
		j.Metrics.ImplementOpened.Inc()
	}
	return j.sayOnIssue(ctx, ev, ref, j.openedComment(created, changed, verified))
}

// verify runs the change's own build and tests where kibitz's credentials are
// not (ADR-0019).
func (j *ReviewJob) verify(ctx context.Context, dir string, commands [][]string) (*sandbox.Result, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "issue.verify")
	defer span.End()

	started := time.Now()
	result, err := j.Sandbox.Verify(ctx, dir, &sandbox.Request{
		Commands: commands,
		Timeout:  j.VerifyTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("verifying the change: %w", err)
	}

	j.Logger.LogAttrs(ctx, slog.LevelInfo, "verification finished",
		slog.Bool("passed", result.Passed()),
		slog.Int("steps", len(result.Steps)),
		slog.String("failure", result.Failure),
		slog.Duration("duration", time.Since(started)),
	)
	if j.Metrics != nil {
		j.Metrics.ImplementVerified.WithLabelValues(strconv.FormatBool(result.Passed())).Inc()
	}
	return result, nil
}

// notEditable returns the changed paths the repository does not allow, in the
// order they came.
func notEditable(allow *repoconfig.PathFilter, changed []string) []string {
	var outside []string
	for _, path := range changed {
		if !Editable(allow, path) {
			outside = append(outside, path)
		}
	}
	return outside
}

// branchName is where the change is pushed. It is derived from the issue alone,
// so a second instruction on one issue finds the first one's branch instead of
// opening a second pull request for the same thing.
func (j *ReviewJob) branchName(prefix string, number int) string {
	if prefix = strings.TrimSpace(prefix); prefix == "" {
		prefix = j.BranchPrefix
	}
	if prefix = strings.TrimSpace(prefix); prefix == "" {
		prefix = DefaultBranchPrefix
	}
	return prefix + "issue-" + strconv.Itoa(number)
}

func (j *ReviewJob) commitName() string {
	if name := strings.TrimSpace(j.CommitName); name != "" {
		return name
	}
	return DefaultCommitName
}

func (j *ReviewJob) commitEmail() string {
	if email := strings.TrimSpace(j.CommitEmail); email != "" {
		return email
	}
	return DefaultCommitEmail
}

// commitMessage says what the commit is and where it came from.
//
// The issue's own title is not used. It is written by a third party, it ends up
// in the repository's history where nobody re-reads it as data, and a subject
// line is not worth that: the issue number is a link to the text itself.
func (j *ReviewJob) commitMessage(ev *event.ReviewEvent, ref forge.IssueRef) string {
	var b strings.Builder
	fmt.Fprintf(&b, "feat: implement issue #%d\n\n", ref.Number)
	b.WriteString("Written by kibitz from the issue, and not read by a person yet.\n")
	b.WriteString("The change passed the repository's own build and tests before this\n")
	b.WriteString("commit was made; nothing else about it has been checked.\n")
	if ev.Actor.Login != "" {
		fmt.Fprintf(&b, "\nRequested-by: %s\n", ev.Actor.Login)
	}
	return b.String()
}

func (j *ReviewJob) pullRequestTitle(ev *event.ReviewEvent, ref forge.IssueRef) string {
	// The issue's title is quoted here rather than used as the subject: this is
	// a title people read, and where it came from should be visible in it.
	title := ""
	if ev.Issue != nil {
		title = strings.TrimSpace(ev.Issue.Title)
	}
	if title == "" {
		return fmt.Sprintf("[kibitz] Issue #%d の実装", ref.Number)
	}
	title = truncate(title, 60)
	return fmt.Sprintf("[kibitz] #%d %s", ref.Number, title)
}
