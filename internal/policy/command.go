package policy

import (
	"strings"

	"github.com/yteraoka/kibitz/internal/event"
)

// DefaultMention is how a comment addresses kibitz unless configured
// otherwise. It is shared so that the server's rules and the worker's help
// text cannot drift apart.
//
// It is deliberately not "@kibitz". GitHub reads "@name" as a mention of
// whoever owns that account — kibitz itself cannot be mentioned, since a
// GitHub App has no account to mention — so the "@" form sends mail to a
// stranger every time somebody asks for a review. A slash has no meaning to
// the forge and every meaning here. See docs/security.md.
const DefaultMention = "/kibitz"

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

// issueCommands are the commands that mean something on an issue. The rest
// need a diff, a thread, or a pull request to stay out of.
var issueCommands = map[string]bool{
	CommandImplement: true,
	CommandPlan:      true,
	CommandHelp:      true,
}

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
// its own rather than merely appear inside a longer word, and code is not
// speech: a mention inside a fenced block or an inline span is somebody
// writing about kibitz, not to it.
func Mentions(body, mention string) bool {
	if mention == "" {
		return false
	}
	lowerBody, lowerMention := strings.ToLower(StripCode(body)), strings.ToLower(mention)

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

	// Only the comment's first line of content can carry it, and only as its
	// first word. The two are walked together so that a line which was all
	// code counts as that first line and comes up empty, rather than the
	// command being found on the line below it.
	lines := strings.Split(body, "\n")
	stripped := strings.Split(StripCode(body), "\n")

	first := ""
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if i < len(stripped) {
			first = stripped[i]
		}
		break
	}

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
