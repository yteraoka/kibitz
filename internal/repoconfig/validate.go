package repoconfig

import (
	"fmt"
	"sort"
	"strings"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/reviewer"
)

// triggerNames maps the names a repository writes to the event kinds kibitz
// uses. The file's vocabulary is deliberately not the wire format: "pr_opened"
// is what somebody would guess, and event kinds are free to change shape.
var triggerNames = map[string]event.Kind{
	"pr_opened":           event.KindPROpened,
	"pr_updated":          event.KindPRUpdated,
	"pr_ready_for_review": event.KindPRReadyForReview,
	"pr_review_requested": event.KindPRReviewRequested,
	"command":             event.KindCommand,
}

// validate rejects a file kibitz cannot act on. A misspelled severity is
// refused rather than rounded to the nearest one: a repository that asked for
// "Critical" and got every "info" finding posted has been ignored, not helped.
func (c *Config) validate() error {
	if c.Review == nil {
		return nil
	}
	r := c.Review

	if r.MinSeverity != "" && !reviewer.Severity(r.MinSeverity).Known() {
		return fmt.Errorf("review.min_severity: %q is not one of %s",
			r.MinSeverity, strings.Join(severityNames(), ", "))
	}
	if r.MaxComments != nil && *r.MaxComments < 0 {
		return fmt.Errorf("review.max_comments must not be negative")
	}
	for _, name := range r.Triggers {
		if _, ok := triggerNames[name]; !ok {
			return fmt.Errorf("review.triggers: %q is not one of %s",
				name, strings.Join(triggerNamesSorted(), ", "))
		}
	}
	for _, pattern := range r.PathsIgnore {
		if err := validPattern(pattern); err != nil {
			return fmt.Errorf("review.paths_ignore: %w", err)
		}
	}
	return nil
}

func severityNames() []string {
	names := make([]string, 0, 5)
	for _, s := range []reviewer.Severity{
		reviewer.SeverityCritical, reviewer.SeverityHigh, reviewer.SeverityMedium,
		reviewer.SeverityLow, reviewer.SeverityInfo,
	} {
		names = append(names, string(s))
	}
	return names
}

func triggerNamesSorted() []string {
	names := make([]string, 0, len(triggerNames))
	for name := range triggerNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
