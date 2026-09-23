// Package textdiff computes a unified diff between two versions of a file.
//
// It exists because one of the three platforms kibitz supports does not
// produce one. GitHub and GitLab both return a patch with their list of
// changed files; Azure DevOps returns the list only, with no patch, no hunks
// and no line counts anywhere in its REST API. kibitz needs the patch twice
// over — the prompt carries it, and [reviewer.NewPositions] checks every
// finding against it — so on that platform the patch has to be made here,
// from the two blobs.
//
// The output is the same shape GitHub sends: hunks starting at "@@", with no
// "---" / "+++" file header, because that is what the rest of kibitz already
// parses.
package textdiff

import (
	"bytes"
	"strings"
)

// Defaults.
const (
	// DefaultContext is the number of unchanged lines kept around a change.
	// Three is what git, GitHub and GitLab all produce, and the reviewer's
	// position map counts context lines as commentable, so a different number
	// here would quietly change which findings are accepted.
	DefaultContext = 3

	// DefaultMaxLines caps the size of a file that is worth diffing. Beyond
	// it the file is reported as changed with no patch, exactly as a platform
	// reports a file it declined to diff.
	DefaultMaxLines = 20000

	// DefaultMaxEdits caps the search. The algorithm below costs O(ND) in
	// time and O(D²) in memory, so a file whose every line differs would cost
	// more than it is worth; past this many edits the whole file is reported
	// as replaced, which is both true and cheap.
	DefaultMaxEdits = 1500

	// sniffBytes is how much of a file is examined for the NUL byte that
	// marks it binary. Git looks at the first 8000 bytes; matching it means
	// kibitz and git agree on what is text.
	sniffBytes = 8000
)

// Options tunes a diff. The zero value is the default in every field.
type Options struct {
	// Context is the number of unchanged lines around each change.
	Context int
	// MaxLines caps the file size that will be diffed.
	MaxLines int
	// MaxEdits caps the edit script search.
	MaxEdits int
}

func (o Options) context() int {
	if o.Context > 0 {
		return o.Context
	}
	return DefaultContext
}

func (o Options) maxLines() int {
	if o.MaxLines > 0 {
		return o.MaxLines
	}
	return DefaultMaxLines
}

func (o Options) maxEdits() int {
	if o.MaxEdits > 0 {
		return o.MaxEdits
	}
	return DefaultMaxEdits
}

// Result is the diff of one file.
type Result struct {
	// Patch is the unified diff, or empty when there is nothing to show:
	// an unchanged file, a binary one, or one too large to diff.
	Patch string
	// Additions and Deletions count changed lines. They are filled in even
	// when Patch is empty and the counts are known.
	Additions int
	Deletions int
	// Binary reports that at least one side is not text.
	Binary bool
	// TooLarge reports that the file was past MaxLines and was not diffed.
	TooLarge bool
	// Approximate reports that the edit script was capped: the patch is a
	// whole-file replacement rather than the smallest set of changes. It is
	// still a correct diff, and still applies.
	Approximate bool
}

// Unified diffs two versions of a file.
//
// Either side may be nil, which is how an added or a deleted file is diffed.
func Unified(before, after []byte, opts Options) Result {
	if bytes.Equal(before, after) {
		return Result{}
	}
	if isBinary(before) || isBinary(after) {
		return Result{Binary: true}
	}

	old, cur := splitLines(before), splitLines(after)
	if len(old) > opts.maxLines() || len(cur) > opts.maxLines() {
		return Result{TooLarge: true}
	}

	// A change is almost always a handful of lines in the middle of a file
	// that is otherwise identical. Trimming what matches at both ends first
	// is what keeps the search below proportional to the change rather than
	// to the file.
	prefix := commonPrefix(old, cur)
	suffix := commonSuffix(old[prefix:], cur[prefix:])
	midOld, midCur := old[prefix:len(old)-suffix], cur[prefix:len(cur)-suffix]

	script, exact := diffLines(midOld, midCur, opts.maxEdits())

	hunks := group(compact(rebase(script, prefix, old, cur, suffix), old, cur), opts.context())
	res := Result{Approximate: !exact}
	var sb strings.Builder
	for _, h := range hunks {
		h.render(&sb, old, cur)
		res.Additions += h.additions
		res.Deletions += h.deletions
	}
	res.Patch = sb.String()
	return res
}

// op is one step of an edit script.
type op uint8

const (
	opEqual op = iota
	opDelete
	opInsert
)

// step is one line of the edit script, in output order.
type step struct {
	kind op
	// old indexes the old lines and cur the new ones. Within diffLines they
	// count from the start of the searched span; rebase turns them into
	// positions in the whole file before anything else reads them.
	old int
	cur int
}

// diffLines produces the edit script between two line slices.
//
// It is Myers' greedy algorithm: walk diagonals of the edit graph, one edit
// distance at a time, and keep the furthest point reached on each. The first
// distance that reaches the far corner is the shortest edit script, and the
// recorded trace is walked backwards to recover it.
//
// The second return reports whether the script is the shortest one. When the
// search passes maxEdits it stops and the caller gets a whole-file
// replacement instead, which is a correct diff and a bounded amount of work.
func diffLines(old, cur []string, maxEdits int) (script []step, exact bool) {
	n, m := len(old), len(cur)
	switch {
	case n == 0 && m == 0:
		return nil, true
	case n == 0:
		return insertAll(cur), true
	case m == 0:
		return deleteAll(old), true
	}

	limit := n + m
	if limit > maxEdits {
		limit = maxEdits
	}

	// v holds the furthest x reached on each diagonal k, offset so that k=0
	// sits in the middle. trace keeps one trimmed copy per edit distance:
	// at distance d only the diagonals -d..d are reachable, so 2d+1 entries
	// are enough and the whole trace costs O(D²) rather than O(D·(N+M)).
	offset := limit + 1
	v := make([]int, 2*offset+1)
	trace := make([][]int, 0, limit+1)

	for d := 0; d <= limit; d++ {
		trace = append(trace, trim(v, offset, d))
		for k := -d; k <= d; k += 2 {
			var x int
			switch {
			case k == -d:
				x = v[offset+k+1]
			case k != d && v[offset+k-1] < v[offset+k+1]:
				x = v[offset+k+1]
			default:
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && old[x] == cur[y] {
				x, y = x+1, y+1
			}
			v[offset+k] = x
			if x >= n && y >= m {
				return backtrack(trace, old, cur, d), true
			}
		}
	}

	// Past the cap. Replacing the whole span is the honest fallback: it says
	// what changed without claiming to know the smallest way to say it.
	return append(deleteAll(old), insertAll(cur)...), false
}

// trim copies the reachable part of v at edit distance d.
func trim(v []int, offset, d int) []int {
	lo, hi := offset-d, offset+d+1
	if lo < 0 {
		lo = 0
	}
	if hi > len(v) {
		hi = len(v)
	}
	out := make([]int, hi-lo)
	copy(out, v[lo:hi])
	return out
}

// at reads the diagonal k out of a trimmed trace entry recorded at distance d.
func at(entry []int, d, k int) int {
	i := k + d
	if i < 0 || i >= len(entry) {
		return 0
	}
	return entry[i]
}

// backtrack walks the trace from the far corner to the origin and reverses
// the result, turning the recorded search into an edit script in file order.
func backtrack(trace [][]int, old, cur []string, d int) []step {
	var reversed []step
	x, y := len(old), len(cur)

	for ; d > 0; d-- {
		entry := trace[d]
		k := x - y

		var prevK int
		if k == -d || (k != d && at(entry, d, k-1) < at(entry, d, k+1)) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := at(entry, d, prevK)
		prevY := prevX - prevK

		for x > prevX && y > prevY {
			x, y = x-1, y-1
			reversed = append(reversed, step{kind: opEqual, old: x, cur: y})
		}
		if prevK == k+1 {
			y--
			reversed = append(reversed, step{kind: opInsert, old: x, cur: y})
		} else {
			x--
			reversed = append(reversed, step{kind: opDelete, old: x, cur: y})
		}
		x, y = prevX, prevY
	}
	for x > 0 && y > 0 {
		x, y = x-1, y-1
		reversed = append(reversed, step{kind: opEqual, old: x, cur: y})
	}

	script := make([]step, len(reversed))
	for i, s := range reversed {
		script[len(reversed)-1-i] = s
	}
	return script
}

func insertAll(cur []string) []step {
	out := make([]step, len(cur))
	for i := range cur {
		out[i] = step{kind: opInsert, cur: i}
	}
	return out
}

func deleteAll(old []string) []step {
	out := make([]step, len(old))
	for i := range old {
		out[i] = step{kind: opDelete, old: i}
	}
	return out
}
