package repoconfig_test

import (
	"testing"

	"github.com/yteraoka/kibitz/internal/repoconfig"
)

func TestPathFilter(t *testing.T) {
	filter, err := repoconfig.NewPathFilter([]string{
		"**/*.md",
		"**/testdata/**",
		"go.sum",
		"vendor/**",
		"internal/*/generated.go",
		"note?.txt",
	})
	if err != nil {
		t.Fatalf("NewPathFilter: %v", err)
	}

	for _, path := range []string{
		// "**/" also means "here": a pattern meant to catch a file anywhere
		// has to catch it at the root too.
		"README.md",
		"docs/worker.md",
		"testdata/webhooks/github/pr.json",
		"internal/webhook/testdata/a/b.json",
		"go.sum",
		"vendor/github.com/x/y.go",
		"internal/forge/generated.go",
		"note1.txt",
	} {
		if !filter.Match(path) {
			t.Errorf("%q was not excluded", path)
		}
	}

	for _, path := range []string{
		"queue.go",
		"go.mod",
		// "*" stops at a separator, so one star is not two.
		"internal/forge/github/generated.go",
		// A pattern is matched against the whole path, not a substring of it.
		"docs/markdown.go",
		"cmd/vendor.go",
		"note10.txt",
	} {
		if filter.Match(path) {
			t.Errorf("%q was excluded", path)
		}
	}
}

// "a/**/b" has to match "a/b": the middle is any number of segments,
// including none.
func TestPathFilterDoubleStarMatchesNothingInBetween(t *testing.T) {
	filter, err := repoconfig.NewPathFilter([]string{"internal/**/testdata/**"})
	if err != nil {
		t.Fatalf("NewPathFilter: %v", err)
	}
	for _, path := range []string{
		"internal/testdata/x.json",
		"internal/a/testdata/x.json",
		"internal/a/b/testdata/c/x.json",
	} {
		if !filter.Match(path) {
			t.Errorf("%q was not excluded", path)
		}
	}
	if filter.Match("internal/testdata.go") {
		t.Error("internal/testdata.go was excluded")
	}
}

// A pattern list nobody wrote must not quietly exclude everything.
func TestEmptyPathFilterMatchesNothing(t *testing.T) {
	filter, err := repoconfig.NewPathFilter(nil)
	if err != nil {
		t.Fatalf("NewPathFilter: %v", err)
	}
	if filter.Match("queue.go") {
		t.Error("an empty filter excluded a file")
	}
	var nilFilter *repoconfig.PathFilter
	if nilFilter.Match("queue.go") {
		t.Error("a nil filter excluded a file")
	}
}

// Glob metacharacters in a literal must not become regexp metacharacters.
func TestPathFilterQuotesTheRest(t *testing.T) {
	filter, err := repoconfig.NewPathFilter([]string{"a.b.go", "x+y.go"})
	if err != nil {
		t.Fatalf("NewPathFilter: %v", err)
	}
	if !filter.Match("a.b.go") || !filter.Match("x+y.go") {
		t.Error("a literal pattern did not match itself")
	}
	if filter.Match("axbxgo") {
		t.Error("a dot in the pattern matched any character")
	}
	if filter.Match("xyy.go") {
		t.Error("a plus in the pattern was read as a repeat")
	}
}

// "vendor" and "vendor/" are what everybody writes for a directory, and both
// used to match nothing at all — silently, which is the worst way for an
// exclusion to fail.
func TestPathFilterExcludesDirectories(t *testing.T) {
	filter, err := repoconfig.NewPathFilter([]string{"vendor/", "node_modules", "docs/generated/"})
	if err != nil {
		t.Fatalf("NewPathFilter: %v", err)
	}

	for _, path := range []string{
		"vendor/github.com/x/y.go",
		"vendor",
		"node_modules/a/b.js",
		"docs/generated/api.md",
	} {
		if !filter.Match(path) {
			t.Errorf("%q was not excluded", path)
		}
	}
	for _, path := range []string{
		"cmd/vendor.go",
		"vendored.go",
		"docs/worker.md",
		"my_node_modules/a.js",
	} {
		if filter.Match(path) {
			t.Errorf("%q was excluded", path)
		}
	}
}

// The patterns a repository is shown are the ones it wrote, separator and all.
func TestPathFilterKeepsThePatternsAsWritten(t *testing.T) {
	filter, err := repoconfig.NewPathFilter([]string{"vendor/", " go.sum ", ""})
	if err != nil {
		t.Fatalf("NewPathFilter: %v", err)
	}
	got := filter.Patterns()
	if len(got) != 2 || got[0] != "vendor/" || got[1] != "go.sum" {
		t.Errorf("Patterns = %q", got)
	}
}
