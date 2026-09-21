package policy

import "strings"

// StripCode blanks out the parts of a Markdown comment that quote code rather
// than say something: fenced blocks and inline spans.
//
// Explaining how to ask for a review is not asking for one, and on a forge the
// way people explain it is to wrap it in backticks. Without this, writing
// "use `@kibitz review`" in a pull request summons the bot, which is both
// surprising and the fastest way to have it turned off.
//
// The result has exactly as many lines as the input, and removed spans leave a
// space behind, so that positions still line up and a mention next to a span
// is still bounded by whitespace.
func StripCode(body string) string {
	lines := strings.Split(body, "\n")
	out := make([]string, len(lines))

	fence := ""
	for i, line := range lines {
		// A fence inside a quoted reply is still a fence.
		bare := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), ">"))

		switch {
		case fence != "":
			if strings.HasPrefix(bare, fence) {
				fence = ""
			}
			out[i] = ""
		case openingFence(bare) != "":
			fence = openingFence(bare)
			out[i] = ""
		default:
			out[i] = stripSpans(line)
		}
	}
	return strings.Join(out, "\n")
}

// openingFence reports the fence a line opens, or "".
func openingFence(line string) string {
	switch {
	case strings.HasPrefix(line, "```"):
		return "```"
	case strings.HasPrefix(line, "~~~"):
		return "~~~"
	default:
		return ""
	}
}

// stripSpans replaces `inline code` with a space. A run of backticks is closed
// by a run of the same length, which is how a span containing a backtick is
// written; an unclosed run is not a span at all and stays as it is.
func stripSpans(line string) string {
	if !strings.Contains(line, "`") {
		return line
	}

	var out strings.Builder
	out.Grow(len(line))

	for i := 0; i < len(line); {
		if line[i] != '`' {
			out.WriteByte(line[i])
			i++
			continue
		}

		run := backtickRun(line[i:])
		closing := indexBacktickRun(line[i+run:], run)
		if closing < 0 {
			out.WriteString(line[i : i+run])
			i += run
			continue
		}
		out.WriteByte(' ')
		i += run + closing + run
	}
	return out.String()
}

// backtickRun counts the backticks s starts with.
func backtickRun(s string) int {
	n := 0
	for n < len(s) && s[n] == '`' {
		n++
	}
	return n
}

// indexBacktickRun finds a run of exactly n backticks, which is what closes a
// span opened by n of them.
func indexBacktickRun(s string, n int) int {
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		run := backtickRun(s[i:])
		if run == n {
			return i
		}
		i += run
	}
	return -1
}
