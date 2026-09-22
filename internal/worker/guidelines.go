package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/yteraoka/kibitz/internal/forge"
)

// DefaultGuidelineFiles are the convention files kibitz looks for.
//
// AGENTS.md is the file the ecosystem settled on for telling a coding agent
// what a repository expects, and .kibitz/guidelines.md is for a repository
// that wants to say something to kibitz alone. CONTRIBUTING.md is deliberately
// not here: it is usually written for a person opening their first pull
// request — how to sign the CLA, how to word a commit — and paying for it on
// every review buys nothing.
var DefaultGuidelineFiles = []string{"AGENTS.md", ".kibitz/guidelines.md"}

// maxGuidelineBytes caps one file. A repository whose conventions do not fit
// in this has written a manual, and the part past it would crowd out the diff.
const maxGuidelineBytes = 24 << 10

// repoGuidelines reads the repository's own conventions.
//
// From the default branch, never from the pull request. These go into the
// agent's instructions rather than being quoted as data, and instructions are
// exactly what somebody without commit access must not be able to write. It is
// the same rule as .kibitz.yaml, for the same reason, and it is why kibitz
// turns off opencode's own search for AGENTS.md in the checkout.
//
// Nothing here fails a job: a file that cannot be read is left out, and the
// review runs with what it has.
func (j *ReviewJob) repoGuidelines(ctx context.Context, client forge.Client, ref forge.PRRef) (string, []string) {
	paths := j.GuidelineFiles
	if paths == nil {
		paths = DefaultGuidelineFiles
	}

	var b strings.Builder
	var found, notes []string
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}

		data, err := client.ReadFile(ctx, ref, path)
		switch {
		case errors.Is(err, forge.ErrFileNotFound), errors.Is(err, forge.ErrNotSupported):
			continue
		case err != nil:
			j.Logger.LogAttrs(ctx, slog.LevelWarn, "could not read a guideline file",
				slog.String("ref", ref.String()),
				slog.String("path", path),
				slog.String("error", err.Error()),
			)
			notes = append(notes, fmt.Sprintf("`%s` を読めませんでした", path))
			continue
		}

		text := strings.TrimSpace(string(data))
		if text == "" {
			continue
		}
		truncated := false
		if len(text) > maxGuidelineBytes {
			text = text[:maxGuidelineBytes]
			truncated = true
		}

		// Named, so that a rule the review cites can be traced back to the
		// file somebody has to edit to change it.
		fmt.Fprintf(&b, "<!-- %s (デフォルトブランチ) -->\n%s\n", path, text)
		if truncated {
			fmt.Fprintf(&b, "\n_(`%s` はここで打ち切りました)_\n", path)
			notes = append(notes, fmt.Sprintf("`%s` が長いため途中までしか読んでいません", path))
		}
		b.WriteString("\n")
		found = append(found, path)
	}

	if len(found) > 0 {
		j.Logger.LogAttrs(ctx, slog.LevelInfo, "read the repository's conventions",
			slog.String("ref", ref.String()),
			slog.Any("files", found),
		)
	}
	return strings.TrimSpace(b.String()), notes
}

// joinGuidelines stacks one block of rules on another. The earlier one stays
// first: a repository adds to what the deployment said rather than replacing
// it, which is the only reason reading these from the default branch is safe.
func joinGuidelines(first, second string) string {
	first, second = strings.TrimSpace(first), strings.TrimSpace(second)
	switch {
	case first == "":
		return second
	case second == "":
		return first
	default:
		return first + "\n\n" + second
	}
}
