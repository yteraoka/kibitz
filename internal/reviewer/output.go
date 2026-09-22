package reviewer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// OutputSchemaVersion is the version of the JSON contract the agent writes.
const OutputSchemaVersion = 1

// OutputPath is where the agent writes its findings, relative to the
// workspace. Reading a file is far more reliable than parsing prose out of the
// agent's stdout, and it keeps the contract explicit.
const OutputPath = ".kibitz/out/review.json"

// Output is the JSON document the agent produces.
type Output struct {
	SchemaVersion int             `json:"schema_version"`
	Summary       string          `json:"summary"`
	Verdict       string          `json:"verdict,omitempty"`
	Confidence    string          `json:"confidence,omitempty"`
	Comments      []OutputComment `json:"comments"`
	SkippedFiles  []string        `json:"skipped_files,omitempty"`
	Notes         string          `json:"notes,omitempty"`
}

// OutputComment is one finding as the agent wrote it.
type OutputComment struct {
	Path       string `json:"path"`
	Line       int    `json:"line"`
	EndLine    int    `json:"end_line,omitempty"`
	Severity   string `json:"severity"`
	Category   string `json:"category,omitempty"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	Suggestion string `json:"suggestion,omitempty"`
}

// ParseOutput decodes the agent's output. Errors are deliberately specific:
// they are fed back to the agent on a retry, so they have to say what was
// wrong.
func ParseOutput(data []byte) (*Output, error) {
	var out Output
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("the output is not valid JSON: %w", err)
	}
	if out.SchemaVersion != OutputSchemaVersion {
		return nil, fmt.Errorf("schema_version is %d, want %d", out.SchemaVersion, OutputSchemaVersion)
	}
	if strings.TrimSpace(out.Summary) == "" {
		return nil, fmt.Errorf("summary is empty")
	}

	for i, c := range out.Comments {
		switch {
		case strings.TrimSpace(c.Path) == "":
			return nil, fmt.Errorf("comments[%d]: path is empty", i)
		case c.Line <= 0:
			return nil, fmt.Errorf("comments[%d]: line must be positive, got %d", i, c.Line)
		case c.EndLine != 0 && c.EndLine < c.Line:
			return nil, fmt.Errorf("comments[%d]: end_line %d is before line %d", i, c.EndLine, c.Line)
		case strings.TrimSpace(c.Body) == "":
			return nil, fmt.Errorf("comments[%d]: body is empty", i)
		case !Severity(strings.ToLower(c.Severity)).Known():
			return nil, fmt.Errorf("comments[%d]: severity %q is not one of critical, high, medium, low, info", i, c.Severity)
		}
	}
	return &out, nil
}

// OutputError marks output that did not meet the contract. It is a distinct
// type because the job treats it differently from any other failure: the agent
// gets one more attempt with the validation error fed back to it, since a
// schema slip is usually fixed by being told about it.
type OutputError struct{ Err error }

func (e *OutputError) Error() string {
	return fmt.Sprintf("the agent's output did not meet the contract: %v", e.Err)
}

func (e *OutputError) Unwrap() error { return e.Err }

// Limits bound what is posted back to the pull request.
// DefaultMaxComments is the cap a caller with no opinion uses. It is not what
// a zero means: see [Sanitize].
const DefaultMaxComments = 20

type Limits struct {
	// MaxComments caps how many findings are posted. The most serious
	// survive. Zero posts none of them.
	MaxComments int
	// MinSeverity drops anything less serious.
	MinSeverity Severity
	// MaxBodyBytes truncates an over-long finding.
	MaxBodyBytes int
}

// Sanitized is the result of checking the agent's output against reality.
type Sanitized struct {
	Findings []Finding
	// OutOfDiff counts findings dropped because the line is not part of the
	// diff. A forge rejects the entire review over one such comment.
	OutOfDiff int
	// BelowSeverity counts findings dropped as too minor.
	BelowSeverity int
	// Duplicate counts findings dropped as repeats.
	Duplicate int
	// Excess counts findings dropped by the cap.
	Excess int
}

// Dropped reports how many findings did not survive.
func (s Sanitized) Dropped() int {
	return s.OutOfDiff + s.BelowSeverity + s.Duplicate + s.Excess
}

// Sanitize turns raw output into findings that can safely be posted: only
// lines the diff actually contains, only severities worth reporting, no
// repeats, and no more than the cap.
func Sanitize(out *Output, positions *Positions, limits Limits) Sanitized {
	var result Sanitized
	if out == nil {
		return result
	}
	// A cap of zero is a cap of zero: it is how a repository asks for the
	// summary without the inline comments, and reading it as "unset" would
	// turn the lowest setting there is into the highest. Callers that have no
	// cap in mind pass [DefaultMaxComments].
	//
	// MaxBodyBytes has no such reading — nobody wants a finding truncated to
	// nothing — so it keeps its fallback.
	if limits.MaxComments < 0 {
		limits.MaxComments = 0
	}
	if limits.MaxBodyBytes <= 0 {
		limits.MaxBodyBytes = 4000
	}

	seen := make(map[string]bool, len(out.Comments))
	candidates := make([]Finding, 0, len(out.Comments))

	for _, c := range out.Comments {
		severity := Severity(strings.ToLower(c.Severity))
		if limits.MinSeverity != "" && !severity.AtLeast(limits.MinSeverity) {
			result.BelowSeverity++
			continue
		}
		if !positions.Allows(c.Path, c.Line) {
			result.OutOfDiff++
			continue
		}
		if c.EndLine > c.Line && !positions.Allows(c.Path, c.EndLine) {
			// Anchor the comment to a single line rather than lose it.
			c.EndLine = 0
		}

		key := fmt.Sprintf("%s:%d:%s", c.Path, c.Line, strings.ToLower(strings.TrimSpace(c.Title)))
		if seen[key] {
			result.Duplicate++
			continue
		}
		seen[key] = true

		candidates = append(candidates, Finding{
			Path:       c.Path,
			Line:       c.Line,
			EndLine:    c.EndLine,
			Severity:   severity,
			Category:   c.Category,
			Title:      strings.TrimSpace(c.Title),
			Body:       truncate(strings.TrimSpace(c.Body), limits.MaxBodyBytes),
			Suggestion: c.Suggestion,
		})
	}

	// Keep the most serious findings when the cap bites, rather than whichever
	// ones the agent happened to write first.
	sort.SliceStable(candidates, func(i, j int) bool {
		return severityRank[candidates[i].Severity] < severityRank[candidates[j].Severity]
	})
	if len(candidates) > limits.MaxComments {
		result.Excess = len(candidates) - limits.MaxComments
		candidates = candidates[:limits.MaxComments]
	}

	result.Findings = candidates
	return result
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const notice = "\n\n…(truncated)"
	if limit <= len(notice) {
		return s[:limit]
	}
	return s[:limit-len(notice)] + notice
}
