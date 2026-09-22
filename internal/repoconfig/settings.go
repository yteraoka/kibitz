package repoconfig

import (
	"strings"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/reviewer"
)

// Settings is what one job actually runs with: the deployment's defaults with
// the repository's file laid over them.
//
// It is a resolved value rather than a pile of optionals, so that the code
// doing the work never has to ask which layer a setting came from.
type Settings struct {
	ReviewEnabled bool
	AnswerEnabled bool
	SkipDraft     bool
	// Triggers are the event kinds worth reviewing. Nil means every kind, and
	// is not the same as an empty set, which means none.
	Triggers map[event.Kind]bool
	// PathsIgnore hides files from the agent. It may be nil.
	PathsIgnore *PathFilter
	// Focus narrows what the review looks for. It reaches the prompt.
	Focus      []string
	Language   string
	Limits     reviewer.Limits
	Model      string
	Guidelines string
	// MCP names the external tool servers this repository asked for. They are
	// resolved against the deployment's catalog by the caller, which is where
	// the definitions and the credentials live.
	MCP []string
}

// Reviews reports whether an event of this kind should be reviewed.
func (s Settings) Reviews(kind event.Kind) bool {
	if !s.ReviewEnabled {
		return false
	}
	// A command says what it wants, and a repository that lists no triggers
	// has not asked for fewer of them.
	if s.Triggers == nil || kind == event.KindCommand {
		return true
	}
	return s.Triggers[kind]
}

// Apply lays the repository's file over base and returns the result. A nil
// config returns base unchanged, which is what a repository without a file
// gets.
//
// Every field works the same way: what the file did not say, the deployment
// still decides. That is the whole contract, and it is why the scalars in
// [Config] are pointers.
func (c *Config) Apply(base Settings) (Settings, error) {
	if c == nil {
		return base, nil
	}
	out := base

	if guidelines := strings.TrimSpace(c.Guidelines); guidelines != "" {
		// Appended rather than replaced: the deployment's rules are the ones
		// nobody with commit access should be able to drop.
		out.Guidelines = join(base.Guidelines, guidelines)
	}
	if c.Answer != nil && c.Answer.Enabled != nil {
		out.AnswerEnabled = *c.Answer.Enabled
	}
	if c.MCP != nil {
		// Replaced rather than added to: a repository that lists its servers
		// has said which ones it wants, and an empty list turns them off.
		out.MCP = trimAll(c.MCP.Allow)
	}
	if c.Review == nil {
		return out, nil
	}
	r := c.Review

	if r.Enabled != nil {
		out.ReviewEnabled = *r.Enabled
	}
	if r.SkipDraft != nil {
		out.SkipDraft = *r.SkipDraft
	}
	if len(r.Triggers) > 0 {
		out.Triggers = make(map[event.Kind]bool, len(r.Triggers))
		for _, name := range r.Triggers {
			if kind, ok := triggerNames[name]; ok {
				out.Triggers[kind] = true
			}
		}
	}
	if language := strings.TrimSpace(r.Language); language != "" {
		out.Language = language
	}
	if model := strings.TrimSpace(r.Model); model != "" {
		out.Model = model
	}
	if severity := strings.TrimSpace(r.MinSeverity); severity != "" {
		out.Limits.MinSeverity = reviewer.Severity(severity)
	}
	if r.MaxComments != nil && *r.MaxComments < out.Limits.MaxComments {
		// Only downwards, with no exception for zero. The cap keeps one pull
		// request from being buried in comments and that is the deployment's
		// call; a repository asking for zero is asking for the summary alone,
		// which is downwards.
		//
		// The base has to be a real cap for this to hold, which is why the
		// caller resolves an unset one before getting here.
		out.Limits.MaxComments = *r.MaxComments
	}
	if len(r.Focus) > 0 {
		out.Focus = trimAll(r.Focus)
	}
	if len(r.PathsIgnore) > 0 {
		// Copied rather than appended in place: Patterns returns the filter's
		// own slice, and appending to it could write into the deployment's
		// patterns instead of adding to a copy of them.
		patterns := append([]string(nil), base.PathsIgnore.Patterns()...)
		filter, err := NewPathFilter(append(patterns, r.PathsIgnore...))
		if err != nil {
			return base, err
		}
		out.PathsIgnore = filter
	}
	return out, nil
}

func join(base, extra string) string {
	if strings.TrimSpace(base) == "" {
		return extra
	}
	return strings.TrimRight(base, "\n") + "\n" + extra
}

func trimAll(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
