// Package repoconfig reads `.kibitz.yaml`, the settings a repository keeps
// next to its own code.
//
// Two rules shape everything here.
//
// Only the keys named in [Config] have any effect. The file is written by
// whoever can commit to the repository, which is not the same set of people
// who deploy kibitz, so it reaches the settings kibitz chose to expose and no
// others. Anything else is reported and ignored rather than applied.
//
// And the file is read from the default branch, never from the pull request
// under review, so that opening a pull request cannot change how that pull
// request is reviewed. That is the caller's job — see [forge.Client.ReadFile]
// — but it is the reason this package never takes a ref.
//
// See docs/configuration.md for the file itself and docs/security.md for why
// it is read where it is.
package repoconfig

import (
	"fmt"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

// Path is where the file lives in a repository.
const Path = ".kibitz.yaml"

// Version is the schema this build understands. A file that claims a later
// one is refused rather than half-applied: a setting that silently did
// nothing is worse than one that said so.
const Version = 1

// Config is `.kibitz.yaml` as written.
//
// The scalars are pointers so that "the repository said false" can be told
// apart from "the repository said nothing", which is the whole job of a file
// that overrides a deployment-wide default.
type Config struct {
	Version    int     `yaml:"version"`
	Review     *Review `yaml:"review"`
	Answer     *Answer `yaml:"answer"`
	Guidelines string  `yaml:"guidelines"`
	MCP        *MCP    `yaml:"mcp"`
}

// MCP turns on the external tool servers a deployment has configured.
type MCP struct {
	// Allow names the servers to enable. A name the deployment does not
	// offer is reported rather than applied: asking for a server nobody
	// configured should not look the same as asking for none.
	Allow []string `yaml:"allow"`
}

// Review holds the review settings.
type Review struct {
	Enabled *bool `yaml:"enabled"`
	// Triggers lists the event kinds worth reviewing. Empty means all of them.
	Triggers []string `yaml:"triggers"`
	// SkipDraft leaves a draft pull request alone until it is marked ready.
	SkipDraft *bool `yaml:"skip_draft"`
	// PathsIgnore are glob patterns whose files are not shown to the agent.
	PathsIgnore []string `yaml:"paths_ignore"`
	// Focus narrows what the review looks for.
	Focus       []string `yaml:"focus"`
	Language    string   `yaml:"language"`
	MinSeverity string   `yaml:"min_severity"`
	MaxComments *int     `yaml:"max_comments"`
	Model       string   `yaml:"model"`
}

// Answer holds the settings for answering questions.
type Answer struct {
	Enabled *bool `yaml:"enabled"`
}

// Parse reads the file. It returns what it understood, the complaints worth
// telling somebody about, and an error only when the file cannot be applied
// at all.
//
// The complaints are separate from the error on purpose. A key this build
// does not know is not a reason to throw away a file that is otherwise fine —
// a repository shared between two kibitz deployments would then work for
// neither — but it is a reason to say so, because the alternative is a
// setting that appears to have been accepted and was not.
func Parse(data []byte) (*Config, []string, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return &Config{}, nil, nil
	}

	// Decoding twice is how both halves of that promise are kept: once into
	// the known shape, which is the whitelist, and once into a bare map to
	// see what was in the file that the first pass dropped.
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, nil, fmt.Errorf("%s is not valid YAML: %w", Path, err)
	}

	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		// A document that decoded into Config but not into a map is a YAML
		// scalar or list at the top level.
		return nil, nil, fmt.Errorf("%s must be a mapping", Path)
	}

	if cfg.Version > Version {
		return nil, nil, fmt.Errorf("%s asks for version %d; this kibitz understands %d",
			Path, cfg.Version, Version)
	}

	notes := unknownKeys(raw)
	sort.Strings(notes)
	if err := cfg.validate(); err != nil {
		return nil, notes, err
	}
	return &cfg, notes, nil
}

// known lists every key that has an effect, and reserved the ones that are
// planned but do nothing yet. Reserved keys are named so that writing one
// gets an answer rather than silence; see docs/roadmap.md for which phase
// each belongs to.
var (
	known = map[string][]string{
		"version":    nil,
		"guidelines": nil,
		"review": {
			"enabled", "triggers", "skip_draft", "paths_ignore",
			"focus", "language", "min_severity", "max_comments", "model",
		},
		"answer": {"enabled"},
		"mcp":    {"allow"},
	}
	reserved = map[string]string{
		"budget":               "Phase 9",
		"implement":            "Phase 8",
		"review.allow_verdict": "未実装",
		"answer.mention":       "未実装",
	}
)

func unknownKeys(raw map[string]any) []string {
	var notes []string
	for key, value := range raw {
		children, ok := known[key]
		if !ok {
			notes = append(notes, describe(key))
			continue
		}
		nested, ok := value.(map[string]any)
		if !ok {
			continue
		}
		for child := range nested {
			if !contains(children, child) {
				notes = append(notes, describe(key+"."+child))
			}
		}
	}
	return notes
}

func describe(key string) string {
	if phase, ok := reserved[key]; ok {
		return fmt.Sprintf("%q はまだ効きません (%s)", key, phase)
	}
	return fmt.Sprintf("%q は kibitz の設定ではありません", key)
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
