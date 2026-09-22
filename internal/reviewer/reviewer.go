// Package reviewer turns a pull request into findings. The agent engine is
// behind an interface so that OpenCode can be replaced without touching the
// job around it (see docs/agent-engine.md).
package reviewer

import (
	"context"
	"strings"
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
	// ModeTriage picks which files of a very large change are worth
	// reviewing. It reads the list of changed files, not their contents.
	ModeTriage Mode = "triage"
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
	// Focus narrows what the review looks for. It comes from the repository's
	// settings or from the command that asked for the review, and it adds an
	// emphasis rather than replacing the standard criteria: a review asked to
	// focus on security is still expected to report the crash it walked past.
	Focus []string
	// FullDiff is the whole pull request when Diff holds only the part the
	// prompt carries — after triage, or after narrowing to what is new. The
	// agent's tools serve it, so a file the prompt left out is still
	// reachable when something makes it worth a look.
	FullDiff *forge.Diff
	// References index the repository's decision records: path and title, not
	// the bodies. They tell the agent what exists to consult; the bodies are
	// fetched through a tool, and only the ones it decides are relevant.
	References []Reference
	// MCP names the external tool servers this run may use. They are names
	// only: what each one is, and the credential it needs, belongs to the
	// deployment and never travels with a request.
	MCP []string
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

// The tools that serve [Reference] bodies, as kibitz-mcp registers them.
//
// The prompt names them as a hint, not as an address: an engine is free to
// namespace a tool server's tools however it likes, and hard-coding the
// qualified name would make the instruction wrong the moment it did. The
// model matches these against the tools it was actually given.
const (
	SearchDocsTool = "search_docs"
	GetDocTool     = "get_doc"
)

// Reference is one document the agent may consult — an architecture decision
// record, usually. It is reference material and not an instruction: what it
// says about the code is worth knowing, what it says to do is not.
type Reference struct {
	Path  string
	Title string
}

// ToolMatches reports whether a tool the engine named is the one kibitz
// registered as want.
//
// The comparison is on the trailing name rather than the whole of it, because
// an engine namespaces a tool server's tools however it likes — opencode keys
// them "<server>_<tool>" — and kibitz has no business knowing the scheme.
// This is the reading half of the decision the prompt makes by naming these
// tools unqualified (see [SearchDocsTool]): state the name, let the engine
// address it.
//
// A separator is required before the suffix, so "forget_doc" is not a
// [GetDocTool]. Where it is still wrong it errs towards matching, because an
// undercount reads as "nobody opens these documents" and that is the reading
// somebody acts on.
func ToolMatches(reported, want string) bool {
	if len(reported) < len(want) {
		return false
	}
	if !strings.EqualFold(reported[len(reported)-len(want):], want) {
		return false
	}
	if len(reported) == len(want) {
		return true
	}
	switch reported[len(reported)-len(want)-1] {
	case '_', '.', '/', '-', ':':
		return true
	}
	return false
}

// ToolUse is what the agent did with the tools it was given.
//
// It answers what the token counts cannot: whether the material kibitz went
// to the trouble of assembling was read at all. An index of decision records
// that nothing ever opens is a line in every prompt and a tool definition in
// every request, bought for nothing — and counting is the only way to find
// that out.
type ToolUse struct {
	// Calls counts invocations by the name the engine reported, failures
	// included, and Failed counts the ones that came back an error. The names
	// are the engine's own: see [ToolMatches].
	Calls  map[string]int
	Failed map[string]int
	// Documents are the reference documents the agent opened in full, in the
	// order it first opened them.
	Documents []string
	// Searches counts searches across those documents. It is kept apart from
	// Documents because a search answers from excerpts: a run that searched
	// three times and opened nothing still consulted them.
	Searches int
}

// Total counts every tool invocation, which is the signal worth having when a
// review comes back empty.
func (t ToolUse) Total() int {
	n := 0
	for _, c := range t.Calls {
		n += c
	}
	return n
}

// Usage reports what a run cost.
//
// The token counts are kept apart rather than summed because providers bill
// them apart: input served from a prompt cache is much cheaper than input the
// model had to read, and reasoning tokens are billed as output that never
// appears in the output. Folding them together would turn the cost estimate
// into a number that is wrong in whichever direction the caching went.
type Usage struct {
	// InputTokens counts prompt tokens the model actually read — what the
	// agent reports as input, which excludes anything the cache served.
	InputTokens int
	// CacheReadTokens counts prompt tokens served from the provider's cache,
	// and CacheWriteTokens the ones written into it.
	CacheReadTokens  int
	CacheWriteTokens int
	// OutputTokens counts the tokens of the visible reply.
	OutputTokens int
	// ReasoningTokens counts thinking tokens, which are billed but not shown.
	ReasoningTokens int
	Duration        time.Duration
}

// Add accumulates another run's usage, which is how a review that needed a
// triage pass first reports what the whole thing cost rather than only the
// half anybody was watching.
func (u Usage) Add(other Usage) Usage {
	return Usage{
		InputTokens:      u.InputTokens + other.InputTokens,
		CacheReadTokens:  u.CacheReadTokens + other.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens + other.CacheWriteTokens,
		OutputTokens:     u.OutputTokens + other.OutputTokens,
		ReasoningTokens:  u.ReasoningTokens + other.ReasoningTokens,
		Duration:         u.Duration + other.Duration,
	}
}

// Input is every prompt token the run was billed for, cached or not.
func (u Usage) Input() int {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// Output is every token the model produced, shown or not.
func (u Usage) Output() int { return u.OutputTokens + u.ReasoningTokens }

// Tokens is the total of both directions.
func (u Usage) Tokens() int { return u.Input() + u.Output() }

// Result is what the engine produced.
type Result struct {
	Summary  string
	Findings []Finding
	// Reply is the answer in [ModeAnswer].
	Reply string
	// Triage is the selection made in [ModeTriage].
	Triage *TriageOutput
	// SessionID identifies the conversation, so a follow-up question can
	// continue it.
	SessionID string
	Usage     Usage
	// Tools is what the agent did with its tools, which is how the reference
	// material earns the space it takes in the prompt.
	Tools ToolUse
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
