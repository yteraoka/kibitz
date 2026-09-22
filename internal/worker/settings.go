package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/repoconfig"
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
}

// defaults are the settings this deployment was started with. They are what a
// repository without a .kibitz.yaml gets, and the floor everything else is
// laid over.
func (j *ReviewJob) defaults() repoconfig.Settings {
	return repoconfig.Settings{
		ReviewEnabled: true,
		AnswerEnabled: true,
		SkipDraft:     j.SkipDraft,
		Language:      j.Language,
		Limits:        j.Limits,
		Model:         j.Model,
		Guidelines:    j.Guidelines,
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

	data, err := client.ReadFile(ctx, ref, repoconfig.Path)
	switch {
	case errors.Is(err, forge.ErrFileNotFound):
		return resolved{Settings: base}
	case errors.Is(err, forge.ErrNotSupported):
		// A platform kibitz cannot read files from is not a misconfigured
		// repository, and saying so on every pull request would be noise.
		return resolved{Settings: base}
	case err != nil:
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "could not read the repository settings",
			slog.String("ref", ref.String()),
			slog.String("path", repoconfig.Path),
			slog.String("error", err.Error()),
		)
		return resolved{Settings: base, notes: []string{
			repoconfig.Path + " を読めなかったため、既定の設定でレビューしました",
		}}
	}

	cfg, notes, err := repoconfig.Parse(data)
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

	if len(notes) > 0 {
		j.Logger.LogAttrs(ctx, slog.LevelInfo, "the repository settings have keys kibitz ignored",
			slog.String("ref", ref.String()),
			slog.Any("notes", notes),
		)
	}
	return resolved{Settings: settings, notes: notes}
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
