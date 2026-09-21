package policy_test

import (
	"strings"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/policy"
)

var now = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

func engine() *policy.Engine {
	return policy.New(policy.Config{
		BotLogins:    []string{"kibitz[bot]"},
		AllowedRepos: []string{"yteraoka/*"},
		Mention:      "@kibitz",
		MaxEventAge:  5 * time.Minute,
	})
}

func prEvent(kind event.Kind) *event.ReviewEvent {
	return &event.ReviewEvent{
		SchemaVersion: event.SchemaVersion,
		ID:            "github:d1",
		OccurredAt:    now.Add(-time.Minute),
		Source:        event.Source{Platform: event.PlatformGitHub, DeliveryID: "d1"},
		Kind:          kind,
		Repository:    event.Repository{Owner: "yteraoka", Name: "kibitz", FullName: "yteraoka/kibitz"},
		PullRequest:   &event.PullRequest{Number: 42},
		Actor:         event.Actor{Login: "yteraoka"},
	}
}

func commentEvent(body string) *event.ReviewEvent {
	ev := prEvent(event.KindCommentCreated)
	ev.Comment = &event.Comment{ID: "1", Body: body, Author: event.Actor{Login: "yteraoka"}}
	return ev
}

func TestEvaluatePullRequestEvents(t *testing.T) {
	for _, kind := range []event.Kind{
		event.KindPROpened,
		event.KindPRUpdated,
		event.KindPRReadyForReview,
		event.KindPRReviewRequested,
		event.KindPRMerged,
	} {
		t.Run(string(kind), func(t *testing.T) {
			d := engine().Evaluate(prEvent(kind), now)
			if !d.Publish || d.Reason != policy.ReasonAccepted {
				t.Errorf("decision = %+v, want accepted", d)
			}
		})
	}
}

// A draft pull request still reaches the queue: whether to skip drafts depends
// on the repository's own .kibitz.yaml, which only the worker can read.
func TestEvaluateDraftIsLeftToTheWorker(t *testing.T) {
	ev := prEvent(event.KindPROpened)
	ev.PullRequest.Draft = true

	if d := engine().Evaluate(ev, now); !d.Publish {
		t.Errorf("decision = %+v, want the draft to be published", d)
	}
}

func TestEvaluateRejections(t *testing.T) {
	tests := []struct {
		name  string
		event func() *event.ReviewEvent
		want  policy.Reason
	}{
		{
			name:  "unknown kind",
			event: func() *event.ReviewEvent { ev := prEvent("pr.exploded"); return ev },
			want:  policy.ReasonUnknownKind,
		},
		{
			name: "authored by kibitz itself",
			event: func() *event.ReviewEvent {
				ev := commentEvent("@kibitz review")
				ev.Actor = event.Actor{Login: "kibitz[bot]", IsBot: true}
				return ev
			},
			want: policy.ReasonSelfAuthored,
		},
		{
			name: "repository not allowed",
			event: func() *event.ReviewEvent {
				ev := prEvent(event.KindPROpened)
				ev.Repository.FullName = "someone/else"
				return ev
			},
			want: policy.ReasonRepoNotAllowed,
		},
		{
			name: "replayed delivery",
			event: func() *event.ReviewEvent {
				ev := prEvent(event.KindPROpened)
				ev.OccurredAt = now.Add(-time.Hour)
				return ev
			},
			want: policy.ReasonStale,
		},
		{
			name:  "comment without a mention",
			event: func() *event.ReviewEvent { return commentEvent("looks good to me") },
			want:  policy.ReasonNoMention,
		},
		{
			name:  "mention of a different account",
			event: func() *event.ReviewEvent { return commentEvent("ask @kibitz-staging instead") },
			want:  policy.ReasonNoMention,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := engine().Evaluate(tc.event(), now)
			if d.Publish {
				t.Errorf("decision = %+v, want it dropped", d)
			}
			if d.Reason != tc.want {
				t.Errorf("reason = %q, want %q", d.Reason, tc.want)
			}
		})
	}
}

// A bot other than kibitz (dependabot, for instance) opens pull requests worth
// reviewing, so only kibitz's own accounts are filtered.
func TestEvaluateOtherBotsAreNotFiltered(t *testing.T) {
	ev := prEvent(event.KindPROpened)
	ev.Actor = event.Actor{Login: "dependabot[bot]", IsBot: true}

	if d := engine().Evaluate(ev, now); !d.Publish {
		t.Errorf("decision = %+v, want another bot's pull request to be published", d)
	}
}

func TestEvaluatePromotesCommands(t *testing.T) {
	ev := commentEvent("@kibitz review --focus security\nよろしくお願いします\n")

	d := engine().Evaluate(ev, now)
	if !d.Publish {
		t.Fatalf("decision = %+v, want accepted", d)
	}
	if ev.Kind != event.KindCommand {
		t.Errorf("kind = %s, want %s", ev.Kind, event.KindCommand)
	}
	if ev.Command == nil || ev.Command.Name != policy.CommandReview {
		t.Fatalf("command = %+v, want review", ev.Command)
	}
	if got, want := ev.Command.Args, []string{"--focus", "security"}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("args = %v, want %v", got, want)
	}
}

// A plain mention is a question, not a command: it stays a comment and the
// worker answers it.
func TestEvaluateMentionWithoutCommand(t *testing.T) {
	ev := commentEvent("@kibitz なぜこの実装だと競合するのですか?")

	d := engine().Evaluate(ev, now)
	if !d.Publish {
		t.Fatalf("decision = %+v, want accepted", d)
	}
	if ev.Kind != event.KindCommentCreated {
		t.Errorf("kind = %s, want it left as a comment", ev.Kind)
	}
	if ev.Command != nil {
		t.Errorf("command = %+v, want none", ev.Command)
	}
}

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
		args int
	}{
		{name: "bare command", body: "@kibitz review", want: "review"},
		{name: "with arguments", body: "@kibitz review --focus security", want: "review", args: 2},
		{name: "further down the comment", body: "some context\n@kibitz implement\nthanks", want: ""},
		{name: "case insensitive", body: "@KIBITZ Help", want: "help"},
		{name: "quoted reply", body: "> @kibitz review", want: ""},
		{name: "quoted reply with a follow-up", body: "> @kibitz review\n\n了解です", want: ""},
		{name: "unknown verb", body: "@kibitz deploy to production", want: ""},
		{name: "mention only", body: "@kibitz", want: ""},
		{name: "mid sentence", body: "I asked @kibitz review this earlier", want: ""},
		{name: "no mention", body: "review please", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := policy.ParseCommand(tc.body, "@kibitz")
			if tc.want == "" {
				if cmd != nil {
					t.Fatalf("ParseCommand = %+v, want nil", cmd)
				}
				return
			}
			if cmd == nil {
				t.Fatalf("ParseCommand = nil, want %q", tc.want)
			}
			if cmd.Name != tc.want {
				t.Errorf("name = %q, want %q", cmd.Name, tc.want)
			}
			if len(cmd.Args) != tc.args {
				t.Errorf("args = %v, want %d of them", cmd.Args, tc.args)
			}
		})
	}
}

func TestMentions(t *testing.T) {
	tests := []struct {
		body string
		want bool
	}{
		{"@kibitz review", true},
		{"hey @kibitz, thoughts?", true},
		{"(@kibitz)", true},
		{"@kibitz-staging review", false},
		{"@kibitz[bot] said", false},
		{"mail@kibitz.example", false},
		{"nothing here", false},
	}

	for _, tc := range tests {
		if got := policy.Mentions(tc.body, "@kibitz"); got != tc.want {
			t.Errorf("Mentions(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"*", "yteraoka/kibitz", true},
		{"yteraoka/*", "yteraoka/kibitz", true},
		{"yteraoka/*", "someone/kibitz", false},
		{"yteraoka/kibitz", "yteraoka/kibitz", true},
		{"*/kibitz", "yteraoka/kibitz", true},
		{"yteraoka/kibit?", "yteraoka/kibitz", true},
		{"yteraoka/kibit?", "yteraoka/kibitzz", false},
		{"*a*a*a*a*a*b", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false},
		{"", "", true},
		{"", "x", false},
	}

	for _, tc := range tests {
		if got := policy.Match(tc.pattern, tc.name); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestMatchAnyEmptyListMatchesNothing(t *testing.T) {
	if policy.MatchAny(nil, "yteraoka/kibitz") {
		t.Error("MatchAny(nil, ...) = true, want false")
	}
}

// keywordEngine gates pull requests on an opt-in keyword, which is how a busy
// repository keeps the queue (and the worker) idle unless a review was asked
// for.
func keywordEngine() *policy.Engine {
	return policy.New(policy.Config{
		BotLogins:    []string{"kibitz[bot]"},
		AllowedRepos: []string{"yteraoka/*"},
		Mention:      "@kibitz",
		Keywords:     []string{"/review", "[review]"},
		MaxEventAge:  5 * time.Minute,
	})
}

func TestEvaluateKeywordGate(t *testing.T) {
	tests := []struct {
		name        string
		title       string
		description string
		want        bool
	}{
		{name: "keyword in the title", title: "[review] add the retry loop", want: true},
		{name: "keyword in the description", description: "見てほしいです\n/review\n", want: true},
		{name: "different case", title: "[REVIEW] add the retry loop", want: true},
		{name: "mention in the description", description: "@kibitz お願いします", want: true},
		{name: "no keyword", title: "add the retry loop", description: "内部だけの変更です", want: false},
		{name: "keyword of another bot", title: "/reviewers please", want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := prEvent(event.KindPROpened)
			ev.PullRequest.Title = tc.title
			ev.PullRequest.Description = tc.description

			d := keywordEngine().Evaluate(ev, now)
			if d.Publish != tc.want {
				t.Errorf("decision = %+v, want publish = %t", d, tc.want)
			}
			if !tc.want && d.Reason != policy.ReasonNoKeyword {
				t.Errorf("reason = %q, want %q", d.Reason, policy.ReasonNoKeyword)
			}
		})
	}
}

// Comments are their own opt-in: addressing kibitz is the request, so a
// keyword in the pull request is not also required.
func TestEvaluateKeywordDoesNotGateComments(t *testing.T) {
	ev := commentEvent("@kibitz レビューをお願いします")
	ev.PullRequest.Title = "add the retry loop"

	if d := keywordEngine().Evaluate(ev, now); !d.Publish {
		t.Errorf("decision = %+v, want the mention to be published", d)
	}

	cmd := commentEvent("@kibitz review")
	cmd.PullRequest.Title = "add the retry loop"

	if d := keywordEngine().Evaluate(cmd, now); !d.Publish {
		t.Errorf("decision = %+v, want the command to be published", d)
	}
	if cmd.Kind != event.KindCommand {
		t.Fatalf("kind = %s, want %s", cmd.Kind, event.KindCommand)
	}
}

// A command that arrives already parsed (a replayed message, or a platform
// that carries commands natively) is an explicit request too.
func TestEvaluateKeywordDoesNotGateCommands(t *testing.T) {
	ev := prEvent(event.KindCommand)
	ev.PullRequest.Title = "add the retry loop"
	ev.Command = &event.Command{Name: policy.CommandReview}

	if d := keywordEngine().Evaluate(ev, now); !d.Publish {
		t.Errorf("decision = %+v, want the command to be published", d)
	}
}

// Without keywords kibitz reviews every pull request, which is the behaviour
// the gate opts out of.
func TestEvaluateWithoutKeywordsEverythingIsWanted(t *testing.T) {
	ev := prEvent(event.KindPROpened)
	ev.PullRequest.Title = "add the retry loop"

	if d := engine().Evaluate(ev, now); !d.Publish {
		t.Errorf("decision = %+v, want every pull request published", d)
	}
}

// The mention is kibitz's own marker, not something GitHub interprets: a
// GitHub App cannot be @-mentioned at all, while "@name" does notify whoever
// owns that account. So a deployment is free to address kibitz with a token
// that is not a username, and everything has to keep working when it does.
func TestMentionNeedNotLookLikeAnAccount(t *testing.T) {
	e := policy.New(policy.Config{
		AllowedRepos: []string{"*"},
		Mention:      "/kibitz",
	})

	t.Run("a command", func(t *testing.T) {
		ev := commentEvent("/kibitz review --focus security")

		if d := e.Evaluate(ev, now); !d.Publish {
			t.Fatalf("decision = %+v, want accepted", d)
		}
		if ev.Kind != event.KindCommand {
			t.Errorf("kind = %s, want %s", ev.Kind, event.KindCommand)
		}
		if ev.Command == nil || ev.Command.Name != policy.CommandReview {
			t.Fatalf("command = %+v, want review", ev.Command)
		}
		if len(ev.Command.Args) != 2 {
			t.Errorf("args = %v, want the two arguments", ev.Command.Args)
		}
	})

	t.Run("a question", func(t *testing.T) {
		ev := commentEvent("/kibitz なぜこの実装だと競合するのですか?")

		if d := e.Evaluate(ev, now); !d.Publish {
			t.Errorf("decision = %+v, want accepted", d)
		}
	})

	// A path is not an address.
	t.Run("a path that contains the token", func(t *testing.T) {
		ev := commentEvent("internal/kibitz/foo.go を見てください")

		if d := e.Evaluate(ev, now); d.Publish {
			t.Errorf("decision = %+v, want it dropped", d)
		}
	})
}

// A command is something a comment opens with. The same words appear when
// somebody quotes an earlier comment or writes down how to ask for a review,
// and neither should run one.
func TestEvaluateCommandsMustOpenTheComment(t *testing.T) {
	bodies := []string{
		"> @kibitz review",
		"ありがとうございます。\n@kibitz review",
	}

	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			ev := commentEvent(body)

			d := engine().Evaluate(ev, now)
			if !d.Publish {
				t.Fatalf("decision = %+v, want the mention still published", d)
			}
			// Still a mention, so kibitz answers rather than acts.
			if ev.Kind != event.KindCommentCreated {
				t.Errorf("kind = %s, want it left as a comment", ev.Kind)
			}
			if ev.Command != nil {
				t.Errorf("command = %+v, want none", ev.Command)
			}
		})
	}
}

// Code is not speech. Writing down how to ask for a review is the most
// ordinary thing to do in a pull request, and it must not ask for one.
func TestEvaluateIgnoresCode(t *testing.T) {
	bodies := []string{
		"使い方: `@kibitz review` とコメントしてください",
		"```\n@kibitz review\n```",
		"```sh\n@kibitz review --focus security\n```\nこう書きます",
		"> 使い方:\n> ```\n> @kibitz review\n> ```",
		"``@kibitz review``",
	}

	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			ev := commentEvent(body)

			d := engine().Evaluate(ev, now)
			if d.Publish {
				t.Errorf("decision = %+v, want it dropped", d)
			}
			if d.Reason != policy.ReasonNoMention {
				t.Errorf("reason = %q, want %q", d.Reason, policy.ReasonNoMention)
			}
		})
	}
}

// A comment whose first line is code has no command on its first line, and
// the line below it is not the head of the comment.
func TestParseCommandDoesNotPromoteTheLineBelowCode(t *testing.T) {
	body := "`前置き`\n@kibitz review"

	if cmd := policy.ParseCommand(body, "@kibitz"); cmd != nil {
		t.Errorf("ParseCommand = %+v, want nil", cmd)
	}
}

// An unmatched backtick is a backtick, not the start of a span that swallows
// the rest of the comment.
func TestStripCodeKeepsUnmatchedBackticks(t *testing.T) {
	body := "`@kibitz review"

	if !policy.Mentions(body, "@kibitz") {
		t.Errorf("Mentions(%q) = false, want the mention to survive", body)
	}
}

func TestStripCode(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "nothing to strip", body: "plain text", want: "plain text"},
		{name: "inline span", body: "use `code` here", want: "use   here"},
		{name: "double backticks", body: "a ``b `c` d`` e", want: "a   e"},
		{name: "unmatched", body: "a ` b", want: "a ` b"},
		{name: "fence", body: "before\n```\ninside\n```\nafter", want: "before\n\n\n\nafter"},
		{name: "tilde fence", body: "~~~\ninside\n~~~", want: "\n\n"},
		{name: "quoted fence", body: "> ```\n> inside\n> ```", want: "\n\n"},
		{name: "unclosed fence runs to the end", body: "```\ninside\nmore", want: "\n\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := policy.StripCode(tc.body); got != tc.want {
				t.Errorf("StripCode(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// Every line stays a line, because the head-of-comment rule is decided by
// position.
func TestStripCodePreservesLineCount(t *testing.T) {
	body := "one\n```\ntwo\n```\n`three`\nfour"

	if got, want := len(strings.Split(policy.StripCode(body), "\n")), len(strings.Split(body, "\n")); got != want {
		t.Errorf("stripped body has %d lines, want %d", got, want)
	}
}
