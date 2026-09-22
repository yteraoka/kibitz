package repoconfig_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/repoconfig"
	"github.com/yteraoka/kibitz/internal/reviewer"
)

func base() repoconfig.Settings {
	return repoconfig.Settings{
		ReviewEnabled: true,
		AnswerEnabled: true,
		SkipDraft:     true,
		Language:      "日本語",
		Limits:        reviewer.Limits{MaxComments: 20, MinSeverity: reviewer.SeverityLow},
		Model:         "deployment/model",
		Guidelines:    "- 運用側のルール",
	}
}

func parse(t *testing.T, text string) *repoconfig.Config {
	t.Helper()
	cfg, notes, err := repoconfig.Parse([]byte(text))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(notes) > 0 {
		t.Fatalf("unexpected notes: %v", notes)
	}
	return cfg
}

func apply(t *testing.T, text string) repoconfig.Settings {
	t.Helper()
	settings, err := parse(t, text).Apply(base())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return settings
}

// A repository that says nothing gets the deployment's settings. This is the
// case for nearly every repository, so it is the one that must not surprise.
func TestNoFileChangesNothing(t *testing.T) {
	var cfg *repoconfig.Config
	settings, err := cfg.Apply(base())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !reflect.DeepEqual(settings, base()) {
		t.Errorf("settings = %+v, want the defaults unchanged", settings)
	}

	empty := apply(t, "\n# 何も書いていない\n")
	if !reflect.DeepEqual(empty, base()) {
		t.Errorf("an empty file changed the settings: %+v", empty)
	}
}

func TestApplyOverridesWhatItStates(t *testing.T) {
	settings := apply(t, `
version: 1
review:
  language: English
  min_severity: high
  max_comments: 5
  model: repo/model
  skip_draft: false
`)

	if settings.Language != "English" {
		t.Errorf("Language = %q", settings.Language)
	}
	if settings.Limits.MinSeverity != reviewer.SeverityHigh {
		t.Errorf("MinSeverity = %q", settings.Limits.MinSeverity)
	}
	if settings.Limits.MaxComments != 5 {
		t.Errorf("MaxComments = %d", settings.Limits.MaxComments)
	}
	if settings.Model != "repo/model" {
		t.Errorf("Model = %q", settings.Model)
	}
	// A bool the repository set to false is not a bool the repository left
	// out, which is the entire reason those fields are pointers.
	if settings.SkipDraft {
		t.Error("skip_draft: false did not take effect")
	}
	// Nothing it did not mention moved.
	if !settings.ReviewEnabled || !settings.AnswerEnabled {
		t.Errorf("settings it did not mention changed: %+v", settings)
	}
}

// The comment cap exists to keep one pull request from being buried, so a
// repository may ask for fewer comments and not for more.
func TestMaxCommentsOnlyGoesDown(t *testing.T) {
	if got := apply(t, "review:\n  max_comments: 50\n").Limits.MaxComments; got != 20 {
		t.Errorf("MaxComments = %d, want the deployment's 20", got)
	}
	if got := apply(t, "review:\n  max_comments: 3\n").Limits.MaxComments; got != 3 {
		t.Errorf("MaxComments = %d, want 3", got)
	}
}

// The deployment's guidelines are the ones nobody with commit access should be
// able to drop, so a repository adds to them.
func TestGuidelinesAreAppended(t *testing.T) {
	got := apply(t, "guidelines: |\n  - リポジトリのルール\n").Guidelines
	if !strings.Contains(got, "運用側のルール") || !strings.Contains(got, "リポジトリのルール") {
		t.Errorf("Guidelines = %q, want both", got)
	}
}

func TestTriggersLimitWhatIsReviewed(t *testing.T) {
	settings := apply(t, "review:\n  triggers: [pr_opened, command]\n")

	if !settings.Reviews(event.KindPROpened) {
		t.Error("pr_opened is not reviewed")
	}
	if settings.Reviews(event.KindPRUpdated) {
		t.Error("pr_updated is reviewed although it was not listed")
	}
	// A command says what it wants; a trigger list is about the events
	// nobody asked for.
	if !settings.Reviews(event.KindCommand) {
		t.Error("a command was refused")
	}
	// Listing nothing is not asking for nothing.
	if !base().Reviews(event.KindPRUpdated) {
		t.Error("the default refuses an event")
	}
}

func TestDisablingReview(t *testing.T) {
	settings := apply(t, "review:\n  enabled: false\nanswer:\n  enabled: false\n")
	if settings.Reviews(event.KindPROpened) || settings.Reviews(event.KindCommand) {
		t.Error("review: enabled false still reviews")
	}
	if settings.AnswerEnabled {
		t.Error("answer: enabled false still answers")
	}
}

// A setting nobody implemented, and a typo, both get an answer rather than
// silence.
func TestUnknownAndReservedKeysAreReported(t *testing.T) {
	_, notes, err := repoconfig.Parse([]byte("review:\n  languag: ja\nbudget:\n  monthly_tokens: 1\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "review.languag") {
		t.Errorf("the typo was not reported: %v", notes)
	}
	if !strings.Contains(joined, "budget") || !strings.Contains(joined, "Phase 9") {
		t.Errorf("the reserved key was not reported as such: %v", notes)
	}
}

func TestParseRejectsWhatCannotBeApplied(t *testing.T) {
	for name, text := range map[string]string{
		"a severity nobody defined": "review:\n  min_severity: urgent\n",
		"a trigger nobody defined":  "review:\n  triggers: [pr_reopened]\n",
		"a negative cap":            "review:\n  max_comments: -1\n",
		"an absolute path":          "review:\n  paths_ignore: [\"/etc/passwd\"]\n",
		"a later schema":            "version: 99\n",
		"not YAML at all":           "review:\n\tlanguage: ja\n",
	} {
		if _, _, err := repoconfig.Parse([]byte(text)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
