// Package reviewer turns a pull request into findings. The agent engine is
// behind an interface so that OpenCode can be replaced without touching the
// job around it (see docs/agent-engine.md).
package reviewer

import (
	"context"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
)

// Mode selects what the agent is asked to do.
type Mode string

// Modes.
const (
	// ModeReview reviews a diff and produces findings.
	ModeReview Mode = "review"
	// ModeAnswer answers a question asked in a comment thread.
	ModeAnswer Mode = "answer"
)

// Severity ranks a finding. Anything below the configured threshold is left
// out, because a review that reports everything reports nothing.
type Severity string

// Severities, most serious first.
const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
	SeverityInfo     Severity = "info"
)

var severityRank = map[Severity]int{
	SeverityCritical: 0,
	SeverityHigh:     1,
	SeverityMedium:   2,
	SeverityLow:      3,
	SeverityInfo:     4,
}

// Known reports whether s is a severity kibitz understands.
func (s Severity) Known() bool {
	_, ok := severityRank[s]
	return ok
}

// AtLeast reports whether s is at least as serious as threshold.
func (s Severity) AtLeast(threshold Severity) bool {
	rank, ok := severityRank[s]
	if !ok {
		return false
	}
	limit, ok := severityRank[threshold]
	if !ok {
		return true
	}
	return rank <= limit
}

// Finding is one thing the agent wants to say about a line of the diff.
type Finding struct {
	Path     string
	Line     int
	EndLine  int
	Severity Severity
	Category string
	Title    string
	Body     string
	// Suggestion is a replacement for the lines the finding covers.
	Suggestion string
}

// Request is one unit of work for the engine.
type Request struct {
	Mode Mode
	// WorkspaceDir is the checkout the agent reads. It is disposable.
	WorkspaceDir string
	Event        *event.ReviewEvent
	PullRequest  *event.PullRequest
	Diff         *forge.Diff
	// ExistingComments are the comments already on the pull request, so the
	// agent does not repeat a point someone has made.
	ExistingComments []forge.Comment
	// Question is the comment being answered in [ModeAnswer].
	Question string
	// Thread is the conversation the question belongs to, oldest first, with
	// the question itself left out. It includes kibitz's own writing: a reply
	// of "why?" under a finding is answerable only if the finding is there
	// too.
	Thread []forge.Comment
	// SessionID continues an earlier conversation about this pull request.
	SessionID string
	// Language is the language findings are written in.
	Language string
	Model    string
	// Guidelines are the repository's own review rules.
	Guidelines string
	// HeadSHA is the commit that was actually checked out.
	HeadSHA string
	// SinceSHA is the commit kibitz reviewed last time, when [Request.Diff]
	// holds only what has changed since. Empty means Diff is the whole pull
	// request.
	SinceSHA string
	// Feedback is what went wrong on the previous attempt. A schema slip is
	// usually fixed by telling the agent about it, so one retry carries the
	// validation error back into the prompt.
	Feedback string
}

// Usage reports what a run cost.
type Usage struct {
	InputTokens  int
	OutputTokens int
	Duration     time.Duration
}

// Result is what the engine produced.
type Result struct {
	Summary  string
	Findings []Finding
	// Reply is the answer in [ModeAnswer].
	Reply string
	// SessionID identifies the conversation, so a follow-up question can
	// continue it.
	SessionID string
	Usage     Usage
	// Dropped counts findings removed while validating the output, which is
	// worth knowing when a review looks thinner than expected.
	Dropped int
	// RawOutput is the agent's document as written, kept for the fields the
	// posted review does not carry: skipped files, notes and its own verdict.
	RawOutput *Output
}

// Engine runs the agent.
type Engine interface {
	Run(ctx context.Context, req Request) (*Result, error)
}
