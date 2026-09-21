package policy

import (
	"strings"

	"github.com/yteraoka/kibitz/internal/event"
)

// DefaultMention is how a comment addresses kibitz unless configured
// otherwise. It is shared so that the server's rules and the worker's help
// text cannot drift apart.
const DefaultMention = "@kibitz"

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

// ParseCommand looks for an explicit command addressed to kibitz. The comment
// has to *open* with it: the first thing in the body is the mention, and the
// word after it is the command.
//
// Anywhere else it is not an instruction. The same words appear when someone
// quotes an earlier comment, explains how to use the bot, or asks about a
// command, and running a review because a colleague wrote down how to ask for
// one is the kind of surprise that gets a bot turned off. Addressing kibitz
// further down the comment still reaches it — as a question, which costs a
// reply rather than an action.
//
// It returns nil when the comment opens with the mention but names no
// command: that is a question too.
func ParseCommand(body, mention string) *event.Command {
	if mention == "" {
		return nil
	}

	// Only the first line can carry it, and only as its first word.
	first, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
	fields := strings.Fields(first)
	if len(fields) < 2 || !strings.EqualFold(fields[0], mention) {
		return nil
	}

	name := strings.ToLower(fields[1])
	if !knownCommands[name] {
		return nil
	}
	cmd := &event.Command{Name: name}
	if len(fields) > 2 {
		cmd.Args = fields[2:]
	}
	return cmd
}
