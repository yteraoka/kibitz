package worker

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/yteraoka/kibitz/internal/repoconfig"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/workspace"
)

// DefaultReferenceDocs are the places a repository conventionally keeps the
// decisions behind its code.
//
// Architecture decision records and nothing else, on purpose. The index they
// produce goes into every prompt, so what is listed has to earn its line: an
// ADR says "we decided not to do X, here is why", which is exactly what a
// reviewer needs and cannot infer from the diff. A repository that keeps its
// decisions elsewhere names the place itself.
var DefaultReferenceDocs = []string{
	"docs/adr/**/*.md",
	"docs/decisions/**/*.md",
	"adr/**/*.md",
}

const (
	// maxReferences caps the index. A repository with more decisions than
	// this has them, and the review can still search; what it does not get is
	// all of them listed in every prompt.
	maxReferences = 50
	// maxTitleRunes keeps one line of the index to one line.
	maxTitleRunes = 90
)

// referenceDocs indexes the repository's decision records.
//
// Paths and titles only. The bodies are what the agent reads later, through a
// tool, and only the ones it decides are relevant — putting them all in the
// prompt would crowd out the diff they are meant to inform.
//
// The index is built from the checkout, so a record the pull request adds is
// listed too. That is deliberate: a pull request that introduces an ADR is
// exactly the case worth reviewing against it. The contents are treated as
// data throughout, never as instructions (docs/security.md).
func (j *ReviewJob) referenceDocs(ctx context.Context, ws *workspace.Workspace) []reviewer.Reference {
	patterns := j.ReferenceDocs
	if patterns == nil {
		patterns = DefaultReferenceDocs
	}
	if len(patterns) == 0 || ws == nil || ws.Dir == "" {
		return nil
	}

	filter, err := repoconfig.NewPathFilter(patterns)
	if err != nil {
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "the reference document patterns are not usable",
			slog.String("error", err.Error()),
		)
		return nil
	}

	var refs []reviewer.Reference
	seen := map[string]bool{}
	// Walking only the directories the patterns name, rather than the whole
	// checkout: a monorepo's tree is not worth crossing to find three files.
	for _, root := range patternRoots(patterns) {
		base := filepath.Join(ws.Dir, filepath.FromSlash(root))
		_ = filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return nil //nolint:nilerr // a missing directory is the ordinary case
			}
			if len(refs) >= maxReferences {
				return fs.SkipAll
			}

			rel, relErr := filepath.Rel(ws.Dir, path)
			if relErr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if seen[rel] || !filter.Match(rel) {
				return nil
			}
			seen[rel] = true
			refs = append(refs, reviewer.Reference{Path: rel, Title: titleOf(path, rel)})
			return nil
		})
	}

	if len(refs) > 0 {
		j.Logger.LogAttrs(ctx, slog.LevelInfo, "indexed the repository's decision records",
			slog.Int("documents", len(refs)),
		)
	}
	return refs
}

// patternRoots reduces each pattern to the directory it is rooted in, which is
// everything before its first wildcard.
func patternRoots(patterns []string) []string {
	roots := make([]string, 0, len(patterns))
	seen := map[string]bool{}
	for _, pattern := range patterns {
		root := ""
		for _, segment := range strings.Split(strings.TrimSpace(pattern), "/") {
			if strings.ContainsAny(segment, "*?") {
				break
			}
			if segment == "" || segment == "." || segment == ".." {
				continue
			}
			root = path.Join(root, segment)
		}
		if seen[root] {
			continue
		}
		seen[root] = true
		roots = append(roots, root)
	}
	return roots
}

// titleOf reads a document's first heading, which is what makes an index line
// worth reading. A file without one is listed by its name.
func titleOf(path, rel string) string {
	data, err := os.ReadFile(path) //nolint:gosec // a path under the workspace this process created
	if err != nil {
		return filepath.Base(rel)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#") {
			continue
		}
		title := strings.TrimSpace(strings.TrimLeft(line, "#"))
		if title == "" {
			continue
		}
		if runes := []rune(title); len(runes) > maxTitleRunes {
			title = string(runes[:maxTitleRunes]) + "…"
		}
		return title
	}
	return filepath.Base(rel)
}
