package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/policy"
	"github.com/yteraoka/kibitz/internal/repoconfig"
	"github.com/yteraoka/kibitz/internal/reviewer"
)

// resolved is the settings one job runs with, together with whatever should
// be said about the file they came from.
type resolved struct {
	repoconfig.Settings
	// notes are complaints about the repository's own file — a key that does
	// not exist, a file that could not be read. They go in the summary rather
	// than only in the log, because the person who can fix the file is the
	// one reading the pull request, not the one reading the worker's log.
	notes []string
	// ignored are the files paths_ignore kept out of this review. A review
	// that silently skipped half a pull request reads as a review that found
	// nothing wrong with it.
	ignored []string
	// mcp are the external tool servers this run may use, already resolved
	// against what the deployment offers.
	mcp []string
	// engine is the engine a review command asked for, named engineName.
	// Nil means the deployment's own, [ReviewJob.Engine].
	engine     reviewer.Engine
	engineName string
}

// defaults are the settings this deployment was started with. They are what a
// repository without a .kibitz.yaml gets, and the floor everything else is
// laid over.
func (j *ReviewJob) defaults() repoconfig.Settings {
	limits := j.Limits
	// Resolved here rather than left to the sanitizer, because a repository
	// may only lower this and "lower than nothing" has no answer.
	if limits.MaxComments <= 0 {
		limits.MaxComments = reviewer.DefaultMaxComments
	}

	return repoconfig.Settings{
		ReviewEnabled: true,
		AnswerEnabled: true,
		// The ceiling, and only the ceiling. Enabled stays false here on
		// purpose: a repository with no settings file never reaches Apply, so
		// whatever these defaults hold is what it ends up with. Written into
		// Enabled, the operator's switch silently meant "this repository
		// asked for it", and the refusal then blamed the actor list on a
		// repository that had never written one.
		Implement:  repoconfig.ImplementSettings{DeploymentAllows: j.ImplementEnabled},
		SkipDraft:  j.SkipDraft,
		Language:   j.Language,
		Limits:     limits,
		Model:      j.Model,
		Guidelines: j.Guidelines,
	}
}

// settingsFor reads the repository's own file and lays it over the defaults.
//
// Nothing here fails a job. A repository that cannot be read, or whose file is
// wrong, gets the deployment's settings and a note saying so: refusing to
// review over a typo in a settings file would be a worse outcome than
// reviewing with the settings everyone else uses.
func (j *ReviewJob) settingsFor(ctx context.Context, client forge.Client, ref forge.PRRef) resolved {
	base := j.defaults()

	// The repository's own conventions, from its default branch. They sit
	// between the deployment's rules and .kibitz.yaml's, so a repository adds
	// to what the operator said and .kibitz.yaml has the last word.
	conventions, notes := j.repoGuidelines(ctx, client, ref)
	base.Guidelines = joinGuidelines(base.Guidelines, conventions)

	data, err := client.ReadFile(ctx, ref, repoconfig.Path)
	switch {
	case errors.Is(err, forge.ErrFileNotFound):
		return resolved{Settings: base, notes: notes}
	case errors.Is(err, forge.ErrNotSupported):
		// A platform kibitz cannot read files from is not a misconfigured
		// repository, and saying so on every pull request would be noise.
		return resolved{Settings: base, notes: notes}
	case err != nil:
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "could not read the repository settings",
			slog.String("ref", ref.String()),
			slog.String("path", repoconfig.Path),
			slog.String("error", err.Error()),
		)
		return resolved{Settings: base, notes: append(notes,
			repoconfig.Path+" を読めなかったため、既定の設定でレビューしました")}
	}

	cfg, parseNotes, err := repoconfig.Parse(data)
	notes = append(notes, parseNotes...)
	if err != nil {
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "the repository settings are not usable",
			slog.String("ref", ref.String()),
			slog.String("error", err.Error()),
		)
		return resolved{Settings: base, notes: append(notes, err.Error())}
	}

	settings, err := cfg.Apply(base)
	if err != nil {
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "the repository settings could not be applied",
			slog.String("ref", ref.String()),
			slog.String("error", err.Error()),
		)
		return resolved{Settings: base, notes: append(notes, err.Error())}
	}

	if len(parseNotes) > 0 {
		// Only the keys: a guideline file that could not be read was already
		// logged where it was read, with its own message.
		j.Logger.LogAttrs(ctx, slog.LevelInfo, "the repository settings have keys kibitz ignored",
			slog.String("ref", ref.String()),
			slog.Any("notes", parseNotes),
		)
	}

	enabled, refused := j.MCP.Resolve(settings.MCP)
	if len(refused) > 0 {
		// Named and not delivered. The repository asked for something it did
		// not get, so it is told, in the log and on the pull request.
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "the repository asked for MCP servers this deployment does not offer",
			slog.String("ref", ref.String()),
			slog.Any("refused", refused),
			slog.Any("offered", j.MCP.Names()),
		)
		notes = append(notes, fmt.Sprintf("`mcp.allow` の %s は、この kibitz では有効にできません",
			strings.Join(backquoted(refused), "、")))
	}
	if len(enabled) > 0 {
		j.Logger.LogAttrs(ctx, slog.LevelInfo, "MCP servers enabled for this review",
			slog.String("ref", ref.String()),
			slog.Any("servers", enabled),
		)
	}

	return resolved{Settings: settings, notes: notes, mcp: enabled}
}

func backquoted(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, "`"+name+"`")
	}
	return out
}

// filterPaths drops the files a repository asked kibitz to leave alone.
//
// It is applied to the diff itself rather than only to the prompt, so that the
// findings are checked against the same set of files the agent was shown. An
// agent that read an ignored file anyway cannot comment on it.
func filterPaths(diff *forge.Diff, filter *repoconfig.PathFilter) (*forge.Diff, []string) {
	if diff == nil || filter == nil {
		return diff, nil
	}

	out := &forge.Diff{Truncated: diff.Truncated}
	var ignored []string
	for _, file := range diff.Files {
		if filter.Match(file.Path) {
			ignored = append(ignored, file.Path)
			continue
		}
		out.Files = append(out.Files, file)
	}
	if len(ignored) == 0 {
		return diff, nil
	}
	return out, ignored
}

// focusOf is what this run was asked to concentrate on: the repository's
// standing setting, or the arguments of the command that asked for a review,
// which apply to that run only.
//
//	/kibitz review --focus security --focus performance
//	/kibitz review --focus=security,performance
func focusOf(ev *event.ReviewEvent, settings repoconfig.Settings) []string {
	if ev == nil || ev.Command == nil {
		return settings.Focus
	}
	if focus := parseFocus(ev.Command.Args); len(focus) > 0 {
		return focus
	}
	return settings.Focus
}

// commandModel is the model a review runs on: the one the command asked for,
// if this deployment lets a comment choose it, and settingsModel otherwise.
//
//	/kibitz review --model google-vertex/gemini-3.8-flash
//	/kibitz review --model=google-vertex/gemini-3.8-flash
//
// A model that is not allowed does not stop the review. It runs on the usual
// model, and the summary says why the request was not honoured -- the same
// treatment a repository's settings get when they ask for too much.
func (j *ReviewJob) commandModel(ev *event.ReviewEvent, settingsModel string, notes []string) (string, []string) {
	if ev == nil || ev.Command == nil || ev.Command.Name != policy.CommandReview {
		return settingsModel, notes
	}
	asked := parseFlag(ev.Command.Args, "--model")
	if asked == "" || asked == settingsModel {
		return settingsModel, notes
	}

	if len(j.CommandModels) == 0 || !policy.MatchAny(j.CommandModels, asked) {
		j.Logger.LogAttrs(context.Background(), slog.LevelWarn, "the review command asked for a model this deployment does not allow",
			slog.String("event_id", ev.ID),
			slog.String("asked", asked),
			slog.Any("allowed", j.CommandModels),
		)
		note := fmt.Sprintf("指定されたモデル `%s` は使えないため、`%s` でレビューしました", asked, settingsModel)
		if len(j.CommandModels) == 0 {
			note += " (この kibitz ではコメントでモデルを指定できません)"
		} else {
			note += fmt.Sprintf(" (使えるのは %s)", strings.Join(backquoted(j.CommandModels), "、"))
		}
		return settingsModel, append(notes, note)
	}
	return asked, append(notes, fmt.Sprintf("コマンドの指定により `%s` でレビューしました", asked))
}

// commandEngine is the engine a review runs on: the one the command asked
// for, if this deployment lets a comment choose it and it can run model, and
// nil -- the deployment's own -- otherwise.
//
//	/kibitz review --engine pi
//	/kibitz review --engine=adk --model google-vertex/gemini-3.8-flash
//
// As with the model, a request that cannot be honoured does not stop the
// review: it runs on the usual engine, and the summary says why.
func (j *ReviewJob) commandEngine(ev *event.ReviewEvent, model string, notes []string) (reviewer.Engine, string, []string) {
	if ev == nil || ev.Command == nil || ev.Command.Name != policy.CommandReview {
		return nil, "", notes
	}
	asked := parseFlag(ev.Command.Args, "--engine")
	if asked == "" || asked == j.EngineName {
		return nil, "", notes
	}

	refuse := func(reason, logMsg string, attrs ...slog.Attr) (reviewer.Engine, string, []string) {
		attrs = append(attrs, slog.String("event_id", ev.ID), slog.String("asked", asked))
		j.Logger.LogAttrs(context.Background(), slog.LevelWarn, logMsg, attrs...)
		note := fmt.Sprintf("指定されたエンジン `%s` は使えないため、%s でレビューしました (%s)", asked, j.engineLabel(), reason)
		return nil, "", append(notes, note)
	}

	engine, ok := j.CommandEngines[asked]
	if !ok {
		allowed := j.commandEngineNames()
		reason := "この kibitz ではコメントでエンジンを指定できません"
		if len(allowed) > 0 {
			reason = fmt.Sprintf("使えるのは %s", strings.Join(backquoted(allowed), "、"))
		}
		return refuse(reason, "the review command asked for an engine this deployment does not allow",
			slog.Any("allowed", allowed))
	}
	// The model was chosen for the usual engine, so the one asked for may
	// not run it: adk runs Gemini only. Triage is not checked: it falls back
	// to the usual engine on its own (see [ReviewJob.triageEngine]).
	if checker, ok := engine.(reviewer.ModelChecker); ok {
		if err := checker.CanRun(model); err != nil {
			return refuse(fmt.Sprintf("モデル `%s` を実行できません", model),
				"the engine the review command asked for cannot run the model",
				slog.String("model", model), slog.String("error", err.Error()))
		}
	}
	return engine, asked, append(notes, fmt.Sprintf("コマンドの指定によりエンジン `%s` でレビューしました", asked))
}

// engineLabel names the deployment's own engine for the summary.
func (j *ReviewJob) engineLabel() string {
	if j.EngineName == "" {
		return "既定のエンジン"
	}
	return "`" + j.EngineName + "`"
}

// commandEngineNames lists the engines a review command may ask for, sorted
// so that the help and the summary read the same every time.
func (j *ReviewJob) commandEngineNames() []string {
	return slices.Sorted(maps.Keys(j.CommandEngines))
}

// parseFlag returns the value of the last flag (as "--flag value" or
// "--flag=value") in args, or "".
func parseFlag(args []string, flag string) string {
	value := ""
	for i := 0; i < len(args); i++ {
		if rest, ok := strings.CutPrefix(args[i], flag+"="); ok {
			value = strings.TrimSpace(rest)
		} else if args[i] == flag && i+1 < len(args) {
			i++
			value = strings.TrimSpace(args[i])
		}
	}
	return value
}

func parseFocus(args []string) []string {
	var focus []string
	for i := 0; i < len(args); i++ {
		value := ""
		if rest, ok := strings.CutPrefix(args[i], "--focus="); ok {
			value = rest
		} else if args[i] == "--focus" && i+1 < len(args) {
			i++
			value = args[i]
		}
		for _, item := range strings.Split(value, ",") {
			if item = strings.TrimSpace(item); item != "" {
				focus = append(focus, item)
			}
		}
	}
	return focus
}

// writeSettingsNotes adds what the repository's own settings did to this
// review, and what kibitz could not do with them.
func writeSettingsNotes(b *strings.Builder, settings resolved) {
	if n := len(settings.ignored); n > 0 {
		fmt.Fprintf(b, "除外: %d 件 (`%s` の `review.paths_ignore`)\n", n, repoconfig.Path)
	}
	for _, note := range settings.notes {
		fmt.Fprintf(b, "\n_%s_\n", note)
	}
}
