package opencode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
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
	// toolCalls counts tool invocations, which is a useful signal when a
	// review comes back empty.
	toolCalls int
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
		if in, out := usageFrom(ev); in > 0 || out > 0 {
			t.usage.InputTokens += in
			t.usage.OutputTokens += out
		}
		if kind := stringField(ev, "type"); strings.Contains(kind, "tool") {
			t.toolCalls++
		}
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

// usageFrom pulls token counts out of whichever shape the event uses.
func usageFrom(ev map[string]any) (in, out int) {
	usage, ok := ev["usage"].(map[string]any)
	if !ok {
		if nested, ok := ev["message"].(map[string]any); ok {
			usage, ok = nested["usage"].(map[string]any)
			if !ok {
				return 0, 0
			}
		} else {
			return 0, 0
		}
	}
	return intField(usage, "input", "inputTokens", "input_tokens", "prompt_tokens"),
		intField(usage, "output", "outputTokens", "output_tokens", "completion_tokens")
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
