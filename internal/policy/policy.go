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
	// ReasonNoCommand drops a comment on an issue that did not ask for
	// anything. An issue has no diff to review and no thread to answer
	// about, so a mention on its own is not work.
	ReasonNoCommand Reason = "no_command"
	// ReasonCommandNotForIssue drops a command that only means something on
	// a pull request. "review" on an issue is a typo, not an instruction.
	ReasonCommandNotForIssue Reason = "command_not_for_issue"
	// ReasonActorNotAllowed drops a comment addressed to kibitz by somebody
	// the operator has not allowed to ask. It is the one refusal worth telling
	// the author about, which is why [Decision.Refused] marks it.
	ReasonActorNotAllowed Reason = "actor_not_allowed"
)

// Config holds the server-side trigger rules.
type Config struct {
	// BotLogins are the accounts kibitz posts as.
	BotLogins []string
	// AllowedRepos are wildcard patterns matched against "owner/name".
	AllowedRepos []string
	// AppID is kibitz's own forge app id. Where the payload says which app
	// performed an action, this is what recognizes kibitz's own writing —
	// exactly, and without anyone having to write down what the account is
	// called. The id is not a secret; it is in the app's settings URL.
	AppID string
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
	// AllowedCommenters are who may ask kibitz for something in a comment:
	// logins, matched without regard to case, with "*" and "?" as in
	// AllowedRepos. "github:alice" names alice on GitHub only; an entry with
	// no platform names that login on every platform. Empty lets anybody ask.
	//
	// It gates comments and nothing else. A pull request being opened or
	// pushed to is not somebody asking.
	AllowedCommenters []string
}

// Engine evaluates events against a [Config].
type Engine struct {
	appID        string
	botLogins    map[string]bool
	allowedRepos []string
	mention      string
	keywords     []string
	maxEventAge  time.Duration
	commenters   []commenter
}

// commenter is one entry of the allowed commenters, split into the platform
// it is limited to (empty for every platform) and the login pattern.
type commenter struct {
	platform event.Platform
	login    string
}

// platforms are the prefixes an allowed commenter may be limited to.
var platforms = map[event.Platform]bool{
	event.PlatformGitHub:      true,
	event.PlatformGitLab:      true,
	event.PlatformAzureDevOps: true,
}

func parseCommenters(list []string) []commenter {
	out := make([]commenter, 0, len(list))
	for _, entry := range list {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		c := commenter{login: entry}
		if platform, login, ok := strings.Cut(entry, ":"); ok && platforms[event.Platform(platform)] {
			c = commenter{platform: event.Platform(platform), login: login}
		}
		out = append(out, c)
	}
	return out
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
		appID:        strings.TrimSpace(cfg.AppID),
		botLogins:    bots,
		allowedRepos: cfg.AllowedRepos,
		mention:      cfg.Mention,
		keywords:     keywords,
		maxEventAge:  cfg.MaxEventAge,
		commenters:   parseCommenters(cfg.AllowedCommenters),
	}
}

// Decision is the outcome of evaluating one event.
type Decision struct {
	Publish bool
	Reason  Reason
}

// Refused reports a comment that asked kibitz for something and was turned
// down because of who wrote it. Unlike every other reason to skip an event,
// the author is waiting for an answer, so this one is worth telling them.
func (d Decision) Refused() bool { return d.Reason == ReasonActorNotAllowed }

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
	// The comment carries its own author, and on GitHub it is the only place
	// the app that wrote it is named.
	if ev.Comment != nil && e.isSelf(ev.Comment.Author) {
		return Decision{Reason: ReasonSelfAuthored}
	}

	if !MatchAny(e.allowedRepos, ev.Repository.FullName) {
		return Decision{Reason: ReasonRepoNotAllowed}
	}

	if e.maxEventAge > 0 && !ev.OccurredAt.IsZero() && now.Sub(ev.OccurredAt) > e.maxEventAge {
		return Decision{Reason: ReasonStale}
	}

	// A comment on an issue is only ever acted on when it asks for something
	// explicitly, and only for the handful of things that mean anything
	// without a diff. Everything else about an issue is somebody else's
	// conversation (ADR-0012).
	if ev.Kind == event.KindIssueComment {
		if ev.Comment == nil || !Mentions(ev.Comment.Body, e.mention) {
			return Decision{Reason: ReasonNoMention}
		}
		cmd := ParseCommand(ev.Comment.Body, e.mention)
		if cmd == nil {
			return Decision{Reason: ReasonNoCommand}
		}
		if !issueCommands[cmd.Name] {
			return Decision{Reason: ReasonCommandNotForIssue}
		}
		if !e.mayAsk(ev) {
			return Decision{Reason: ReasonActorNotAllowed}
		}
		ev.Command = cmd
		ev.Kind = event.KindIssueCommand
		return Decision{Publish: true, Reason: ReasonAccepted}
	}

	if ev.Kind == event.KindCommentCreated {
		if ev.Comment == nil || !Mentions(ev.Comment.Body, e.mention) {
			return Decision{Reason: ReasonNoMention}
		}
		// Checked only once the comment has addressed kibitz: a comment that
		// did not ask for anything is not refused, it is simply not ours.
		if !e.mayAsk(ev) {
			return Decision{Reason: ReasonActorNotAllowed}
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

	// The mention counts as a keyword: writing "/kibitz review this" in the
	// description is the obvious way to ask, and having to learn a second
	// vocabulary for it would be surprising.
	if Mentions(text, e.mention) {
		return true
	}

	// The keywords are lowercased in New, and punctuation like "[review]" is
	// matched anywhere rather than as a word, so a title can carry it as a tag.
	// Code is excluded for the same reason it is in a comment: a description
	// that shows how to ask for a review is not asking for one.
	haystack := strings.ToLower(StripCode(text))
	for _, keyword := range e.keywords {
		if strings.Contains(haystack, keyword) {
			return true
		}
	}
	return false
}

// mayAsk reports whether the comment's author may ask kibitz for something.
//
// It is the comment's own author that is checked, not the delivery's actor:
// the comment is the request, and on every platform kibitz supports the two
// are the same person, but only the comment says so for certain.
func (e *Engine) mayAsk(ev *event.ReviewEvent) bool {
	if len(e.commenters) == 0 {
		return true
	}
	author := ev.Actor
	if ev.Comment != nil && ev.Comment.Author.Login != "" {
		author = ev.Comment.Author
	}
	login := strings.ToLower(strings.TrimSpace(author.Login))
	if login == "" {
		// Nobody to check against the list is nobody on it.
		return false
	}
	for _, c := range e.commenters {
		if c.platform != "" && c.platform != ev.Source.Platform {
			continue
		}
		if Match(c.login, login) {
			return true
		}
	}
	return false
}

// isSelf reports whether the actor is kibitz itself.
//
// The app id is the reliable half: it survives renaming the app and needs no
// configuration. The login list is the other half, for the events that do not
// name an app — GitHub reports one on an issue comment but not on a review
// comment.
func (e *Engine) isSelf(a event.Actor) bool {
	if e.appID != "" && a.AppID == e.appID {
		return true
	}
	if a.Login == "" {
		return false
	}
	return e.botLogins[strings.ToLower(a.Login)]
}
