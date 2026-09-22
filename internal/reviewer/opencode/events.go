package opencode

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
	// text is the assistant's visible output, which is the answer itself in
	// answer mode.
	text string
	// sessionID identifies the conversation so a follow-up can continue it.
	sessionID string
	usage     reviewer.Usage
	// tools is what the agent did with the tools it was given, which is how
	// the reference material in the prompt is shown to be worth its space.
	tools reviewer.ToolUse
}

// parseEvents reads `opencode run --format json` output.
//
// The stream is read defensively: each line is decoded into a generic
// structure and only the fields kibitz needs are picked out, so a new event
// type or a renamed field degrades to less telemetry rather than a failed
// review. Lines that are not JSON at all are treated as plain output, which is
// what a non-JSON build of the CLI would produce.
func parseEvents(stdout []byte, logger *slog.Logger) transcript {
	var t transcript
	var text strings.Builder

	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	// Model output easily exceeds the default 64 KiB line limit.
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if line[0] != '{' {
			text.Write(line)
			text.WriteByte('\n')
			continue
		}

		var ev map[string]any
		if err := json.Unmarshal(line, &ev); err != nil {
			text.Write(line)
			text.WriteByte('\n')
			continue
		}

		if id := stringField(ev, "sessionID", "session_id", "sessionId"); id != "" {
			t.sessionID = id
		}
		t.usage = t.usage.Add(usageFrom(ev))
		t.recordTool(ev)
		if s := textFrom(ev); s != "" {
			text.WriteString(s)
		}
	}
	if err := scanner.Err(); err != nil && logger != nil {
		logger.Warn("truncated agent output", slog.String("error", err.Error()))
	}

	t.text = text.String()
	return t
}

// stringField returns the first of keys that holds a string.
func stringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// usageFrom pulls token counts out of a step-finish event.
//
// `opencode run --format json` prints one object per line, each of the shape
// {"type":…,"timestamp":…,"sessionID":…,"part":{…}}, and of those only the
// step-finish part carries usage:
//
//	{"type":"step_finish","sessionID":"ses_…","part":{
//	  "type":"step-finish","reason":"tool-calls","cost":0.0123,
//	  "tokens":{"input":8123,"output":214,"reasoning":0,
//	            "cache":{"read":41000,"write":0}}}}
//
// The counts are per step, and a run that calls tools finishes several steps,
// so they are summed. "input" is what the model read, excluding whatever the
// cache served; "output" excludes reasoning. Both are kept as the agent
// reports them — see [reviewer.Usage] for why they are not added up here.
//
// The part type is checked so that a version which starts reporting tokens
// somewhere else as well does not get counted twice; a version that renames
// them degrades to no telemetry rather than a failed review.
func usageFrom(ev map[string]any) reviewer.Usage {
	part, ok := ev["part"].(map[string]any)
	if !ok || stringField(part, "type") != "step-finish" {
		return reviewer.Usage{}
	}
	tokens, ok := part["tokens"].(map[string]any)
	if !ok {
		return reviewer.Usage{}
	}

	usage := reviewer.Usage{
		InputTokens:     intField(tokens, "input"),
		OutputTokens:    intField(tokens, "output"),
		ReasoningTokens: intField(tokens, "reasoning"),
	}
	if cache, ok := tokens["cache"].(map[string]any); ok {
		usage.CacheReadTokens = intField(cache, "read")
		usage.CacheWriteTokens = intField(cache, "write")
	}
	return usage
}

// recordTool counts one tool invocation, and notes which reference documents
// it opened.
//
// The stream reports a tool part twice at most — once completed, once errored
// — and only once it has finished:
//
//	{"type":"tool_use","sessionID":"ses_…","part":{
//	  "type":"tool","callID":"call_…","tool":"kibitz_get_doc",
//	  "state":{"status":"completed","input":{"path":"docs/adr/0010-….md"},
//	           "output":"…","title":"…"}}}
//
// The part's own type is what is checked, not the event name around it: the
// part comes from the schema both the CLI and the server share, while the
// event name belongs to this one command's JSON mode.
//
// The tool name is recorded as the engine gave it. opencode prefixes a tool
// server's tools with the server's name, so kibitz's own get_doc arrives here
// as "kibitz_get_doc" — which is the engine's business and not something to
// normalize away. [reviewer.ToolMatches] is what reads it back.
func (t *transcript) recordTool(ev map[string]any) {
	part, ok := ev["part"].(map[string]any)
	if !ok || stringField(part, "type") != "tool" {
		return
	}
	name := stringField(part, "tool")
	if name == "" {
		return
	}

	state, _ := part["state"].(map[string]any)
	switch stringField(state, "status") {
	case "completed", "error":
	default:
		// A call that has not finished is reported again when it does, and
		// counting it here would count it twice.
		return
	}

	if t.tools.Calls == nil {
		t.tools.Calls = map[string]int{}
	}
	t.tools.Calls[name]++
	if stringField(state, "status") == "error" {
		if t.tools.Failed == nil {
			t.tools.Failed = map[string]int{}
		}
		t.tools.Failed[name]++
		// A failed read opened nothing, so it is counted but not credited.
		return
	}

	input, _ := state["input"].(map[string]any)
	switch {
	case reviewer.ToolMatches(name, reviewer.GetDocTool):
		t.rememberDocument(stringField(input, "path"))
	case reviewer.ToolMatches(name, reviewer.SearchDocsTool):
		t.tools.Searches++
	}
}

// rememberDocument notes a document the agent read, keeping the order it read
// them in and each one once.
func (t *transcript) rememberDocument(path string) {
	if path == "" {
		return
	}
	if slices.Contains(t.tools.Documents, path) {
		return
	}
	t.tools.Documents = append(t.tools.Documents, path)
}

func intField(m map[string]any, keys ...string) int {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
	}
	return 0
}

// textFrom extracts assistant text from the shapes the stream uses for it.
func textFrom(ev map[string]any) string {
	if part, ok := ev["part"].(map[string]any); ok {
		// Reasoning parts carry text too, and it is not the answer: including
		// it would put the model's thinking in the review comment.
		if kind := stringField(part, "type"); kind != "" && kind != "text" {
			return ""
		}
		if s := stringField(part, "text"); s != "" {
			return s
		}
	}
	if kind := stringField(ev, "type"); kind == "text" || strings.HasSuffix(kind, ".text") {
		if s := stringField(ev, "text"); s != "" {
			return s
		}
	}
	return ""
}
