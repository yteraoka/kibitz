package policy

import (
	"strings"

	"github.com/yteraoka/kibitz/internal/event"
)

// Command names kibitz understands in a comment.
const (
	CommandReview    = "review"
	CommandExplain   = "explain"
	CommandAnswer    = "answer"
	CommandIgnore    = "ignore"
	CommandHelp      = "help"
	CommandImplement = "implement"
	CommandPlan      = "plan"
)

var knownCommands = map[string]bool{
	CommandReview:    true,
	CommandExplain:   true,
	CommandAnswer:    true,
	CommandIgnore:    true,
	CommandHelp:      true,
	CommandImplement: true,
	CommandPlan:      true,
}

// Mentions reports whether body addresses kibitz. The mention has to stand on
// its own rather than merely appear inside a longer word, so that a quoted
// address in prose does not summon the bot.
func Mentions(body, mention string) bool {
	if mention == "" {
		return false
	}
	lowerBody, lowerMention := strings.ToLower(body), strings.ToLower(mention)

	for i := 0; ; {
		idx := strings.Index(lowerBody[i:], lowerMention)
		if idx < 0 {
			return false
		}
		start := i + idx
		end := start + len(lowerMention)
		if isStandaloneMention(lowerBody, start, end) {
			return true
		}
		i = end
	}
}

func isStandaloneMention(body string, start, end int) bool {
	if start > 0 && !isBoundary(body[start-1]) {
		return false
	}
	if end < len(body) && !isBoundary(body[end]) {
		return false
	}
	return true
}

func isBoundary(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return false
	case c == '_', c == '-', c == '/', c == '[':
		// "@kibitz-staging" and "@kibitz[bot]" are different accounts.
		return false
	default:
		return true
	}
}

// ParseCommand looks for an explicit command addressed to kibitz. It reads one
// line at a time, because a mention buried in a paragraph is a conversation,
// not an instruction.
//
// It returns nil when the comment mentions kibitz without naming a command:
// that is a question, handled by the answer path.
func ParseCommand(body, mention string) *event.Command {
	if mention == "" {
		return nil
	}
	lowerMention := strings.ToLower(mention)

	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), ">"))
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 || strings.ToLower(fields[0]) != lowerMention {
			continue
		}

		name := strings.ToLower(fields[1])
		if !knownCommands[name] {
			continue
		}
		cmd := &event.Command{Name: name}
		if len(fields) > 2 {
			cmd.Args = fields[2:]
		}
		return cmd
	}
	return nil
}
