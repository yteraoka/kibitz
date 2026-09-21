package policy

// Match reports whether name matches pattern, where "*" stands for any run of
// characters and "?" for exactly one. It is the same wildcard syntax used for
// repository allow lists in the configuration.
//
// The implementation walks the pattern with a backtracking pointer instead of
// recursing, so a pattern full of stars cannot blow the stack or take
// exponential time on hostile input.
func Match(pattern, name string) bool {
	var (
		p, n           int
		starP, starN   = -1, 0
		patLen, namLen = len(pattern), len(name)
	)

	for n < namLen {
		switch {
		case p < patLen && (pattern[p] == '?' || pattern[p] == name[n]):
			p++
			n++
		case p < patLen && pattern[p] == '*':
			starP, starN = p, n
			p++
		case starP >= 0:
			// Backtrack: let the last star swallow one more character.
			starN++
			p, n = starP+1, starN
		default:
			return false
		}
	}

	for p < patLen && pattern[p] == '*' {
		p++
	}
	return p == patLen
}

// MatchAny reports whether name matches any of the patterns. An empty list
// matches nothing.
func MatchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if Match(p, name) {
			return true
		}
	}
	return false
}
