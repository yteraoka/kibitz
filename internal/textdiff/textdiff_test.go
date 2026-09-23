package textdiff

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnUnchangedFileHasNoPatch(t *testing.T) {
	got := Unified([]byte("a\nb\n"), []byte("a\nb\n"), Options{})
	if got.Patch != "" || got.Additions != 0 || got.Deletions != 0 {
		t.Fatalf("an unchanged file produced %+v", got)
	}
}

func TestPatchesLookLikeTheOnesTheOtherPlatformsSend(t *testing.T) {
	// No "---" or "+++" header: GitHub's patch field starts at the first
	// hunk, and internal/reviewer parses that shape.
	tests := []struct {
		name          string
		before, after string
		want          string
		adds, dels    int
	}{
		{
			name:   "a line changes in the middle",
			before: "a\nb\nc\nd\ne\nf\ng\n",
			after:  "a\nb\nc\nX\ne\nf\ng\n",
			want:   "@@ -1,7 +1,7 @@\n a\n b\n c\n-d\n+X\n e\n f\n g\n",
			adds:   1, dels: 1,
		},
		{
			name:   "a file is created",
			before: "",
			after:  "a\nb\n",
			want:   "@@ -0,0 +1,2 @@\n+a\n+b\n",
			adds:   2,
		},
		{
			name:   "a file is emptied",
			before: "a\nb\n",
			after:  "",
			want:   "@@ -1,2 +0,0 @@\n-a\n-b\n",
			dels:   2,
		},
		{
			name:   "the last line gains a newline it did not have",
			before: "a\nb",
			after:  "a\nb\n",
			want:   "@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+b\n",
			adds:   1, dels: 1,
		},
		{
			name:   "the last line loses its newline",
			before: "a\nb\n",
			after:  "a\nb",
			want:   "@@ -1,2 +1,2 @@\n a\n-b\n+b\n\\ No newline at end of file\n",
			adds:   1, dels: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Unified([]byte(tc.before), []byte(tc.after), Options{})
			if got.Patch != tc.want {
				t.Errorf("patch:\n%s\nwant:\n%s", got.Patch, tc.want)
			}
			if got.Additions != tc.adds || got.Deletions != tc.dels {
				t.Errorf("counted +%d -%d, want +%d -%d", got.Additions, got.Deletions, tc.adds, tc.dels)
			}
		})
	}
}

func TestChangesFarApartBecomeSeparateHunks(t *testing.T) {
	before := lines("a", 30)
	after := strings.Replace(before, "a3\n", "X\n", 1)
	after = strings.Replace(after, "a25\n", "Y\n", 1)

	got := Unified([]byte(before), []byte(after), Options{})
	if n := strings.Count(got.Patch, "@@ -"); n != 2 {
		t.Fatalf("got %d hunks, want 2:\n%s", n, got.Patch)
	}
}

func TestChangesCloseTogetherShareOneHunk(t *testing.T) {
	// Two changes four lines apart: splitting them would produce hunks whose
	// context overlaps, which is not what a diff looks like.
	before := lines("a", 30)
	after := strings.Replace(before, "a10\n", "X\n", 1)
	after = strings.Replace(after, "a14\n", "Y\n", 1)

	got := Unified([]byte(before), []byte(after), Options{})
	if n := strings.Count(got.Patch, "@@ -"); n != 1 {
		t.Fatalf("got %d hunks, want 1:\n%s", n, got.Patch)
	}
}

func TestAChangeIsReportedWhereItHappened(t *testing.T) {
	// Appending to a file that ends in a repeated line is the case a raw
	// shortest-edit-script gets wrong: without compaction the change is
	// reported against the first of the repeated lines instead of after the
	// last one.
	before := "x\n}\n\n}\n"
	after := "x\n}\n\nnew\n\n}\n"

	got := Unified([]byte(before), []byte(after), Options{})
	for _, line := range strings.Split(got.Patch, "\n") {
		if strings.HasPrefix(line, "-") {
			t.Errorf("a pure addition reported a removal:\n%s", got.Patch)
			break
		}
	}
	if got.Deletions != 0 || got.Additions != 2 {
		t.Errorf("counted +%d -%d, want +2 -0:\n%s", got.Additions, got.Deletions, got.Patch)
	}
}

func TestBinaryFilesAreNotDiffed(t *testing.T) {
	got := Unified([]byte("a\x00b"), []byte("a\x00c"), Options{})
	if !got.Binary || got.Patch != "" {
		t.Fatalf("a binary file produced %+v", got)
	}
}

func TestAFileTooLargeToDiffIsReportedRatherThanDiffed(t *testing.T) {
	before := lines("a", 50)
	after := lines("b", 50)

	got := Unified([]byte(before), []byte(after), Options{MaxLines: 10})
	if !got.TooLarge || got.Patch != "" {
		t.Fatalf("an oversized file produced %+v", got)
	}
}

func TestPastTheEditCapTheWholeFileIsReplaced(t *testing.T) {
	// The search is bounded, and what it falls back to still has to be a
	// correct patch — not a wrong one, and not none.
	before := lines("a", 200)
	after := lines("b", 200)

	got := Unified([]byte(before), []byte(after), Options{MaxEdits: 4})
	if !got.Approximate {
		t.Fatal("a capped search did not say so")
	}
	if got.Additions != 200 || got.Deletions != 200 {
		t.Fatalf("counted +%d -%d, want +200 -200", got.Additions, got.Deletions)
	}
	requireApplies(t, before, after, got.Patch)
}

// TestEveryPatchApplies is the test that matters. A diff that reads well but
// does not describe the change is worse than no diff at all, and the only
// authority on whether it does is git: apply the patch to the old file and
// see whether the new one comes back.
func TestEveryPatchApplies(t *testing.T) {
	requireGit(t)

	rng := rand.New(rand.NewSource(1))
	checked := 0
	for i := 0; i < 400; i++ {
		before := randomFile(rng, rng.Intn(40), rng.Intn(4) != 0)
		after := mutate(rng, before)
		if before == after {
			continue
		}
		checked++

		got := Unified([]byte(before), []byte(after), Options{})
		if got.Patch == "" {
			t.Fatalf("case %d produced no patch for a changed file:\nbefore %q\nafter %q", i, before, after)
		}
		if !applies(t, before, after, got.Patch) {
			t.Fatalf("case %d does not apply:\nbefore %q\nafter %q\npatch:\n%s", i, before, after, got.Patch)
		}
	}
	if checked < 200 {
		t.Fatalf("only %d cases actually differed; the generator is not exercising anything", checked)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed, and it is the authority this test checks against")
	}
}

func requireApplies(t *testing.T, before, after, patch string) {
	t.Helper()
	requireGit(t)
	if !applies(t, before, after, patch) {
		t.Fatalf("the patch does not apply:\n%s", patch)
	}
}

// applies hands git the old file and the patch, and reports whether what
// comes out is the new file.
func applies(t *testing.T, before, after, patch string) bool {
	t.Helper()

	dir := t.TempDir()
	target := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(target, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	patchFile := filepath.Join(dir, "p.diff")
	if err := os.WriteFile(patchFile, []byte("--- a/f.txt\n+++ b/f.txt\n"+patch), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("git", "apply", "p.diff")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("git apply: %v: %s", err, out)
		return false
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	return string(got) == after
}

func lines(prefix string, n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "%s%d\n", prefix, i)
	}
	return sb.String()
}

// randomFile builds a file out of a small vocabulary, so that repeated lines
// are common: that is what makes the alignment ambiguous and the compaction
// worth testing.
func randomFile(rng *rand.Rand, count int, trailingNewline bool) string {
	var sb strings.Builder
	for i := 0; i < count; i++ {
		fmt.Fprintf(&sb, "line %d %s", rng.Intn(12), strings.Repeat("x", rng.Intn(4)))
		if i < count-1 || trailingNewline {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func mutate(rng *rand.Rand, src string) string {
	split := strings.SplitAfter(src, "\n")
	if len(split) > 0 && split[len(split)-1] == "" {
		split = split[:len(split)-1]
	}

	for e := rng.Intn(6); e >= 0; e-- {
		if len(split) == 0 {
			split = append(split, "new\n")
			continue
		}
		i := rng.Intn(len(split))
		switch rng.Intn(3) {
		case 0:
			split = append(split[:i], split[i+1:]...)
		case 1:
			added := fmt.Sprintf("added %d\n", rng.Intn(99))
			split = append(split[:i], append([]string{added}, split[i:]...)...)
		default:
			newline := ""
			if strings.HasSuffix(split[i], "\n") {
				newline = "\n"
			}
			split[i] = fmt.Sprintf("changed %d", rng.Intn(99)) + newline
		}
	}
	return strings.Join(split, "")
}
