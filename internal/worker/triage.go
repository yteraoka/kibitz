package worker

import (
	"context"
	"log/slog"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/telemetry"
	"github.com/yteraoka/kibitz/internal/workspace"
)

// triaged is what a triage pass decided: the diff to review, and what was set
// aside to get there.
type triaged struct {
	Diff    *forge.Diff
	Skipped []string
	// Notes is the agent's own account of the selection, shown to the author.
	Notes string
}

// triage narrows a change that is too large to review in one go.
//
// Reviewing a hundred thousand lines in one prompt does not work and is not
// cheap to fail at, so a first pass reads only the list of changed files and
// says which are worth the reviewer's attention. It sees names and sizes, not
// contents: a pass that read everything would spend exactly what it exists to
// save.
//
// Returning the diff unchanged is always a valid outcome. Reviewing too much
// wastes tokens; reviewing too little loses findings, so every failure here
// falls back to the whole thing.
func (j *ReviewJob) triage(ctx context.Context, ws *workspace.Workspace, ev *event.ReviewEvent, pr *event.PullRequest, diff *forge.Diff) triaged {
	if j.MaxDiffLines <= 0 || diff == nil {
		return triaged{Diff: diff}
	}
	if diff.Lines() <= j.MaxDiffLines && !diff.Truncated {
		return triaged{Diff: diff}
	}

	ctx, span := telemetry.Tracer().Start(ctx, "review.triage")
	defer span.End()

	j.Logger.LogAttrs(ctx, slog.LevelInfo, "the change is too large to review in one pass; triaging",
		slog.Int("lines", diff.Lines()),
		slog.Int("limit", j.MaxDiffLines),
		slog.Int("files", len(diff.Files)),
	)

	model := j.TriageModel
	if model == "" {
		model = j.Model
	}

	result, err := j.Engine.Run(ctx, reviewer.Request{
		Mode:         reviewer.ModeTriage,
		WorkspaceDir: ws.Dir,
		Event:        ev,
		PullRequest:  pr,
		Diff:         diff,
		Language:     j.Language,
		Model:        model,
		HeadSHA:      ws.HeadSHA,
	})
	if err != nil || result == nil || result.Triage == nil {
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "triage failed; reviewing the whole change",
			slog.String("error", errorText(err)),
		)
		return triaged{Diff: diff}
	}

	selected, skipped := selectFiles(diff, result.Triage.Paths)
	if len(selected.Files) == 0 {
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "triage selected nothing that is in the diff; reviewing the whole change")
		return triaged{Diff: diff}
	}

	j.Logger.LogAttrs(ctx, slog.LevelInfo, "triage selected the files to review",
		slog.Int("selected", len(selected.Files)),
		slog.Int("skipped", len(skipped)),
		slog.Int("lines", selected.Lines()),
		slog.Int("input_tokens", result.Usage.InputTokens),
		slog.Int("output_tokens", result.Usage.OutputTokens),
	)
	return triaged{Diff: selected, Skipped: skipped, Notes: result.Triage.Notes}
}

// selectFiles keeps the files the triage pass asked for, in the order the
// diff already has them, and reports the rest.
//
// A path the agent invented is ignored rather than trusted: the selection
// decides what to read, and nothing outside the diff was ever readable.
func selectFiles(diff *forge.Diff, paths []string) (*forge.Diff, []string) {
	wanted := make(map[string]bool, len(paths))
	for _, p := range paths {
		wanted[p] = true
	}

	selected := &forge.Diff{Truncated: diff.Truncated}
	var skipped []string
	for _, f := range diff.Files {
		if wanted[f.Path] || (f.PreviousPath != "" && wanted[f.PreviousPath]) {
			selected.Files = append(selected.Files, f)
			continue
		}
		skipped = append(skipped, f.Path)
	}
	return selected, skipped
}

func errorText(err error) string {
	if err == nil {
		return "the agent produced no selection"
	}
	return err.Error()
}
