package textdiff

import (
	"fmt"
	"strings"
)

// rebase turns a script for the searched span into one for the whole file,
// putting back the matching lines that were trimmed off both ends. They are
// what the context around the first and last change is drawn from, so they
// have to be in the script even though no search ever looked at them.
func rebase(script []step, prefix int, old, cur []string, suffix int) []step {
	out := make([]step, 0, len(script)+prefix+suffix)
	for i := 0; i < prefix; i++ {
		out = append(out, step{kind: opEqual, old: i, cur: i})
	}
	for _, s := range script {
		s.old += prefix
		s.cur += prefix
		out = append(out, s)
	}
	for i := 0; i < suffix; i++ {
		out = append(out, step{
			kind: opEqual,
			old:  len(old) - suffix + i,
			cur:  len(cur) - suffix + i,
		})
	}
	return out
}

// hunk is one "@@" block: a run of changes with context around it.
type hunk struct {
	steps     []step
	additions int
	deletions int
}

// group cuts an edit script into hunks.
//
// A hunk covers one run of changes plus context lines on each side. Two runs
// closer together than twice the context are kept in one hunk rather than
// split into two that would overlap, which is what git does and what makes
// the line numbers in the headers add up.
func group(script []step, context int) []hunk {
	var hunks []hunk
	i := 0
	for i < len(script) {
		// Find the next change.
		start := i
		for start < len(script) && script[start].kind == opEqual {
			start++
		}
		if start == len(script) {
			return hunks
		}

		// Back up over the leading context.
		from := start - context
		if from < 0 {
			from = 0
		}

		// Extend past every change that is close enough to share this hunk.
		end := start
		for {
			for end < len(script) && script[end].kind != opEqual {
				end++
			}
			// A run of equal lines longer than twice the context separates
			// two hunks; anything shorter is cheaper to keep as context.
			run := end
			for run < len(script) && script[run].kind == opEqual {
				run++
			}
			if run-end > 2*context || run == len(script) {
				break
			}
			end = run
		}

		to := end + context
		if to > len(script) {
			to = len(script)
		}

		h := hunk{steps: script[from:to]}
		for _, s := range h.steps {
			switch s.kind {
			case opInsert:
				h.additions++
			case opDelete:
				h.deletions++
			case opEqual:
			}
		}
		hunks = append(hunks, h)
		i = to
	}
	return hunks
}

// render writes one hunk in unified format.
func (h hunk) render(sb *strings.Builder, old, cur []string) {
	if len(h.steps) == 0 {
		return
	}

	oldStart, oldCount := span(h.steps, opDelete)
	curStart, curCount := span(h.steps, opInsert)

	// Counts are lines, starts are 1-based line numbers. A side that
	// contributes nothing — a file that was created, or emptied — is written
	// as starting at the line before the change, which is how git renders a
	// zero-length range.
	fmt.Fprintf(sb, "@@ -%s +%s @@\n", rng(oldStart, oldCount), rng(curStart, curCount))

	for _, s := range h.steps {
		switch s.kind {
		case opEqual:
			write(sb, " ", old[s.old])
		case opDelete:
			write(sb, "-", old[s.old])
		case opInsert:
			write(sb, "+", cur[s.cur])
		}
	}
}

// write emits one line of a hunk.
//
// A line that does not end in a newline is the last line of a file that has
// no final newline, and git marks that explicitly: without the marker, a
// patch that adds the missing newline is indistinguishable from one that
// changes nothing.
func write(sb *strings.Builder, prefix, line string) {
	sb.WriteString(prefix)
	if strings.HasSuffix(line, "\n") {
		sb.WriteString(line)
		return
	}
	sb.WriteString(line)
	sb.WriteString("\n\\ No newline at end of file\n")
}

// span returns the 1-based start and the length of one side of a hunk. kind
// selects the side: the old side counts equal and delete lines, the new side
// equal and insert.
func span(steps []step, changed op) (start, count int) {
	first := -1
	for _, s := range steps {
		if s.kind != opEqual && s.kind != changed {
			continue
		}
		index := s.old
		if changed == opInsert {
			index = s.cur
		}
		if first < 0 {
			first = index
		}
		count++
	}
	if first < 0 {
		// This side has no lines in the hunk at all: the file was created,
		// or every line of it was removed. git points at the line before.
		return position(steps, changed), 0
	}
	return first + 1, count
}

// position answers where a side with no lines in the hunk sits, which is
// after the last line of it that exists before the hunk.
func position(steps []step, changed op) int {
	for _, s := range steps {
		if changed == opInsert {
			return s.cur
		}
		return s.old
	}
	return 0
}

func rng(start, count int) string {
	if count == 1 {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}

// splitLines cuts content into lines, keeping the newline on each.
//
// Keeping the terminator is what makes a last line with no newline compare
// unequal to the same text with one, so a patch that only adds the final
// newline still comes out as a change rather than as nothing.
func splitLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	s := string(content)
	var out []string
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			return out
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
		if s == "" {
			return out
		}
	}
}

func commonPrefix(a, b []string) int {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

func commonSuffix(a, b []string) int {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[len(a)-1-i] == b[len(b)-1-i] {
		i++
	}
	return i
}

// isBinary reports whether content is not text, by the same rule git uses: a
// NUL byte near the start.
func isBinary(content []byte) bool {
	head := content
	if len(head) > sniffBytes {
		head = head[:sniffBytes]
	}
	for _, b := range head {
		if b == 0 {
			return true
		}
	}
	return false
}
