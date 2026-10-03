package pi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"

	"github.com/yteraoka/kibitz/internal/reviewer"
)

// transcript is what a run's event stream tells us.
type transcript struct {
	// text is the visible text of the last assistant message, which is the
	// answer itself in answer and plan mode. Earlier messages are the model
	// narrating its way there ("I'll read the file first"), not the answer.
	text      string
	sessionID string
	usage     reviewer.Usage
	tools     reviewer.ToolUse
	// steps counts assistant messages, one per model turn.
	steps int
	// stopReason and errorMessage are the last assistant message's, which
	// is how a run that exited cleanly can still have failed.
	stopReason   string
	errorMessage string
}

// event is the part of a `pi --mode json` record kibitz reads. The stream
// is documented in pi's docs/json.md; only these fields matter here, and a
// record that has none of them is skipped.
type event struct {
	Type string `json:"type"`
	// ID is the session header's.
	ID string `json:"id"`
	// Message is on message_end.
	Message *message `json:"message"`
	// The tool_execution_* fields.
	ToolCallID       string         `json:"toolCallId"`
	ParentToolCallID string         `json:"parentToolCallId"`
	ToolName         string         `json:"toolName"`
	Args             map[string]any `json:"args"`
	IsError          bool           `json:"isError"`
}

type message struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	Usage        *usage          `json:"usage"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
}

// usage is pi's, per assistant message. Reasoning is already included in
// output, unlike opencode, so it is taken out again: see [reviewer.Usage].
type usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
	Reasoning  int `json:"reasoning"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// parseEvents reads the JSONL stream. Like the opencode parser it is
// defensive: a record it does not understand costs telemetry, not the review.
func parseEvents(stdout []byte, logger *slog.Logger) transcript {
	var t transcript
	// Arguments arrive on tool_execution_start and the outcome on
	// tool_execution_end, so the first is kept until the second.
	args := map[string]map[string]any{}

	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}

		switch ev.Type {
		case "session":
			if ev.ID != "" {
				t.sessionID = ev.ID
			}
		case "message_end":
			t.recordMessage(ev.Message)
		case "tool_execution_start":
			args[ev.ToolCallID] = ev.Args
		case "tool_execution_end":
			if ev.Args == nil {
				ev.Args = args[ev.ToolCallID]
			}
			delete(args, ev.ToolCallID)
			t.recordTool(ev)
		}
	}
	if err := scanner.Err(); err != nil && logger != nil {
		logger.Warn("truncated agent output", slog.String("error", err.Error()))
	}
	return t
}

func (t *transcript) recordMessage(m *message) {
	if m == nil || m.Role != "assistant" {
		return
	}
	t.steps++
	if m.Usage != nil {
		reasoning := min(m.Usage.Reasoning, m.Usage.Output)
		t.usage = t.usage.Add(reviewer.Usage{
			InputTokens:      m.Usage.Input,
			CacheReadTokens:  m.Usage.CacheRead,
			CacheWriteTokens: m.Usage.CacheWrite,
			OutputTokens:     m.Usage.Output - reasoning,
			ReasoningTokens:  reasoning,
		})
	}
	t.stopReason = m.StopReason
	t.errorMessage = m.ErrorMessage

	// Thinking blocks are left out: they are the model's reasoning, not
	// something to post.
	var blocks []contentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return
	}
	var text strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	if s := strings.TrimSpace(text.String()); s != "" {
		t.text = s
	}
}

// recordTool counts one finished invocation. Calls nested inside another
// tool (codemode's, if it is ever turned on) are counted under their own
// name: they are the reads that happened.
func (t *transcript) recordTool(ev event) {
	if ev.ToolName == "" {
		return
	}
	if t.tools.Calls == nil {
		t.tools.Calls = map[string]int{}
	}
	t.tools.Calls[ev.ToolName]++
	if ev.IsError {
		if t.tools.Failed == nil {
			t.tools.Failed = map[string]int{}
		}
		t.tools.Failed[ev.ToolName]++
		return
	}
	switch {
	case reviewer.ToolMatches(ev.ToolName, reviewer.GetDocTool):
		if path, _ := ev.Args["path"].(string); path != "" && !slices.Contains(t.tools.Documents, path) {
			t.tools.Documents = append(t.tools.Documents, path)
		}
	case reviewer.ToolMatches(ev.ToolName, reviewer.SearchDocsTool):
		t.tools.Searches++
	}
}
