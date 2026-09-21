// Package policy decides whether an event is worth acting on. The server runs
// it before publishing, so that traffic kibitz will never review does not
// reach the queue at all.
//
// The rules here are the ones that can be decided without reading the
// repository's own configuration; everything that depends on .kibitz.yaml is
// decided later, in the worker. See docs/event-schema.md.
package policy

import (
	"strings"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
)

// Reason explains a decision. It is logged and exported as a metric label, so
// "why did kibitz ignore my pull request" has an answer.
type Reason string

// Decision reasons.
const (
	ReasonAccepted       Reason = "accepted"
	ReasonUnknownKind    Reason = "unknown_kind"
	ReasonSelfAuthored   Reason = "self_authored"
	ReasonRepoNotAllowed Reason = "repo_not_allowed"
	ReasonStale          Reason = "stale"
	ReasonNoMention      Reason = "no_mention"
	ReasonNoKeyword      Reason = "no_keyword"
)

// Config holds the server-side trigger rules.
type Config struct {
	// BotLogins are the accounts kibitz posts as.
	BotLogins []string
	// AllowedRepos are wildcard patterns matched against "owner/name".
	AllowedRepos []string
	// Mention is the handle that addresses kibitz in a comment.
	Mention string
	// Keywords gate pull request events: when set, a pull request is only
	// reviewed if its title or description contains one of them. It is how a
	// repository opts in per pull request rather than having every push
	// reviewed, and it keeps the queue empty the rest of the time.
	//
	// Comments are unaffected: addressing kibitz is already an opt-in, and a
	// command always goes through.
	Keywords []string
	// MaxEventAge rejects deliveries older than this as replays. Zero disables
	// the check.
	MaxEventAge time.Duration
}

// Engine evaluates events against a [Config].
type Engine struct {
	botLogins    map[string]bool
	allowedRepos []string
	mention      string
	keywords     []string
	maxEventAge  time.Duration
}

// New builds an engine.
func New(cfg Config) *Engine {
	bots := make(map[string]bool, len(cfg.BotLogins))
	for _, login := range cfg.BotLogins {
		if login = strings.TrimSpace(login); login != "" {
			bots[strings.ToLower(login)] = true
		}
	}
	keywords := make([]string, 0, len(cfg.Keywords))
	for _, k := range cfg.Keywords {
		if k = strings.TrimSpace(k); k != "" {
			keywords = append(keywords, strings.ToLower(k))
		}
	}

	return &Engine{
		botLogins:    bots,
		allowedRepos: cfg.AllowedRepos,
		mention:      cfg.Mention,
		keywords:     keywords,
		maxEventAge:  cfg.MaxEventAge,
	}
}

// Decision is the outcome of evaluating one event.
type Decision struct {
	Publish bool
	Reason  Reason
}

// Evaluate decides whether ev should be published.
//
// It may modify ev: when a comment names a command, the command is attached
// and the kind is promoted to [event.KindCommand], so the worker does not have
// to parse comment bodies again.
func (e *Engine) Evaluate(ev *event.ReviewEvent, now time.Time) Decision {
	if ev == nil || !ev.Kind.Known() {
		return Decision{Reason: ReasonUnknownKind}
	}

	// Never react to our own comments: that is how a bot ends up talking to
	// itself until someone notices the bill.
	if e.isSelf(ev.Actor) {
		return Decision{Reason: ReasonSelfAuthored}
	}

	if !MatchAny(e.allowedRepos, ev.Repository.FullName) {
		return Decision{Reason: ReasonRepoNotAllowed}
	}

	if e.maxEventAge > 0 && !ev.OccurredAt.IsZero() && now.Sub(ev.OccurredAt) > e.maxEventAge {
		return Decision{Reason: ReasonStale}
	}

	if ev.Kind == event.KindCommentCreated {
		if ev.Comment == nil || !Mentions(ev.Comment.Body, e.mention) {
			return Decision{Reason: ReasonNoMention}
		}
		if cmd := ParseCommand(ev.Comment.Body, e.mention); cmd != nil {
			ev.Command = cmd
			ev.Kind = event.KindCommand
		}
		return Decision{Publish: true, Reason: ReasonAccepted}
	}

	// Pull request events are the ones that arrive whether or not anybody
	// wants a review, so they are the ones a keyword gates. An explicit
	// command has already said what it wants and is never gated.
	if ev.Kind != event.KindCommand && !e.wanted(ev) {
		return Decision{Reason: ReasonNoKeyword}
	}

	return Decision{Publish: true, Reason: ReasonAccepted}
}

// wanted reports whether a pull request asked to be reviewed. With no keywords
// configured every pull request is wanted, which is the behaviour of a review
// bot that reviews everything.
func (e *Engine) wanted(ev *event.ReviewEvent) bool {
	if len(e.keywords) == 0 {
		return true
	}
	if ev.PullRequest == nil {
		return false
	}

	text := ev.PullRequest.Title + "\n" + ev.PullRequest.Description

	// The mention counts as a keyword: writing "@kibitz review this" in the
	// description is the obvious way to ask, and having to learn a second
	// vocabulary for it would be surprising.
	if Mentions(text, e.mention) {
		return true
	}

	// The keywords are lowercased in New, and punctuation like "[review]" is
	// matched anywhere rather than as a word, so a title can carry it as a tag.
	haystack := strings.ToLower(text)
	for _, keyword := range e.keywords {
		if strings.Contains(haystack, keyword) {
			return true
		}
	}
	return false
}

// isSelf reports whether the actor is kibitz itself.
func (e *Engine) isSelf(a event.Actor) bool {
	if a.Login == "" {
		return false
	}
	return e.botLogins[strings.ToLower(a.Login)]
}
