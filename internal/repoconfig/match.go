package repoconfig

import (
	"fmt"
	"regexp"
	"strings"
)

// PathFilter decides whether a file is shown to the agent.
//
// The patterns are the ones people already write in .gitignore and in CI
// configuration, because that is what they will type:
//
//	"*"  matches any run of characters within one path segment
//	"**" matches any number of segments
//	"?"  matches one character, but never a separator
//
// A pattern that starts with "**/" also matches at the root, so "**/*.md"
// covers README.md as well as docs/README.md. Anything else is a literal,
// matched against the whole path.
//
// A pattern also covers everything under what it names, so "vendor" and
// "vendor/" both exclude vendor/github.com/x/y.go. Without that, the two
// spellings everybody reaches for first would match nothing at all and say
// nothing about it.
type PathFilter struct {
	patterns []string
	re       *regexp.Regexp
}

// NewPathFilter compiles the patterns. An empty list filters nothing.
func NewPathFilter(patterns []string) (*PathFilter, error) {
	cleaned := make([]string, 0, len(patterns))
	parts := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if err := validPattern(pattern); err != nil {
			return nil, err
		}
		cleaned = append(cleaned, pattern)
		// "vendor/" names a directory the same way "vendor" does; the
		// trailing separator is not part of any path it has to match.
		parts = append(parts, translate(strings.TrimSuffix(pattern, "/")))
	}
	if len(parts) == 0 {
		return nil, nil
	}

	// One alternation rather than a pattern per file: the patterns come from
	// the repository and the file list from the pull request, and both can be
	// long. Go's regexp is linear in the input, so a hostile pattern costs
	// compile time and nothing else.
	// The "(?:/.*)?" is what makes a pattern cover a directory's contents:
	// it is the one place the whole alternation needs it, and applying it
	// there is the same as applying it to every branch.
	re, err := regexp.Compile("^(?:" + strings.Join(parts, "|") + ")(?:/.*)?$")
	if err != nil {
		return nil, fmt.Errorf("patterns %v cannot be compiled: %w", cleaned, err)
	}
	return &PathFilter{patterns: cleaned, re: re}, nil
}

// Match reports whether path is covered by any pattern. A nil filter matches
// nothing, which is what "no patterns configured" should do.
func (f *PathFilter) Match(path string) bool {
	if f == nil || f.re == nil {
		return false
	}
	return f.re.MatchString(strings.TrimPrefix(path, "./"))
}

// Patterns returns the patterns as written, for logs and for the summary.
func (f *PathFilter) Patterns() []string {
	if f == nil {
		return nil
	}
	return f.patterns
}

// validPattern rejects what cannot be a path pattern. It is separate from
// [NewPathFilter] so that a file can be refused when it is read rather than
// when the first diff happens to be filtered.
func validPattern(pattern string) error {
	if strings.TrimSpace(pattern) == "" {
		return fmt.Errorf("an empty pattern matches nothing useful")
	}
	if strings.HasPrefix(pattern, "/") {
		return fmt.Errorf("%q: paths are relative to the repository root, so they do not start with /", pattern)
	}
	if strings.TrimSuffix(pattern, "/") == "" {
		return fmt.Errorf("%q: a pattern of nothing but separators matches nothing useful", pattern)
	}
	return nil
}

// translate turns one glob into a regular expression fragment.
func translate(pattern string) string {
	var b strings.Builder

	// "**/x" is also "x": a pattern meant to catch a file anywhere should
	// catch it at the root too, which is what .gitignore does and what
	// everybody expects.
	if rest, ok := strings.CutPrefix(pattern, "**/"); ok {
		b.WriteString("(?:.*/)?")
		pattern = rest
	}

	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				// "a/**/b" must also match "a/b", so the separator after a
				// "**" is swallowed with it.
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 2
					continue
				}
				b.WriteString(".*")
				i++
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}
