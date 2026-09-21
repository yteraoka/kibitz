package policy_test

import (
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
	ev := commentEvent("thanks!\n@kibitz review --focus security\n")

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
		{name: "on its own line", body: "some context\n@kibitz implement\nthanks", want: "implement"},
		{name: "case insensitive", body: "@KIBITZ Help", want: "help"},
		{name: "quoted reply", body: "> @kibitz review", want: "review"},
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
