package reviewer

import (
	"strconv"
	"strings"

	"github.com/yteraoka/kibitz/internal/forge"
)

// Positions records which lines of which files a comment may be anchored to.
// Forges reject a review comment on a line that is not part of the diff, and
// they reject the whole review, not just the offending comment, so this is
// checked before anything is posted.
type Positions struct {
	byFile map[string]map[int]bool
}

// NewPositions reads the commentable lines out of a diff.
func NewPositions(diff *forge.Diff) *Positions {
	p := &Positions{byFile: make(map[string]map[int]bool)}
	if diff == nil {
		return p
	}
	for _, f := range diff.Files {
		if f.Patch == "" {
			// Binary files and files the platform declined to diff have no
			// commentable lines.
			continue
		}
		p.byFile[f.Path] = commentableLines(f.Patch)
	}
	return p
}

// Allows reports whether a comment on path at line would be accepted.
func (p *Positions) Allows(path string, line int) bool {
	lines, ok := p.byFile[path]
	if !ok {
		return false
	}
	return lines[line]
}

// Files reports how many files carry commentable lines.
func (p *Positions) Files() int { return len(p.byFile) }

// commentableLines walks a unified diff and collects the line numbers on the
// new side: added lines and the context around them. Removed lines are not
// commentable on the new side, which is the side kibitz reviews.
func commentableLines(patch string) map[int]bool {
	lines := make(map[int]bool)
	current := 0

	for _, raw := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(raw, "@@"):
			if start, ok := parseHunkStart(raw); ok {
				current = start
			}
		case current == 0:
			// Anything before the first hunk header is not positioned.
			continue
		case strings.HasPrefix(raw, "+"):
			lines[current] = true
			current++
		case strings.HasPrefix(raw, "-"):
			// Only the old side advances.
		case strings.HasPrefix(raw, "\\"):
			// "\ No newline at end of file" belongs to the previous line.
		default:
			// A context line, which is commentable too.
			lines[current] = true
			current++
		}
	}
	return lines
}

// parseHunkStart pulls the new-side start line out of "@@ -a,b +c,d @@".
func parseHunkStart(header string) (int, bool) {
	_, rest, ok := strings.Cut(header, "+")
	if !ok {
		return 0, false
	}
	rest, _, _ = strings.Cut(rest, " ")
	start, _, _ := strings.Cut(rest, ",")

	n, err := strconv.Atoi(strings.TrimSpace(start))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
