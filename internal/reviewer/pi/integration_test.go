package pi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/reviewer/opencode"
	"github.com/yteraoka/kibitz/internal/reviewer/pi"
)

// turn is one scripted model response: a tool call or a final text.
type turn struct {
	tool string
	args any
	text string
}

// scriptedModel serves the OpenAI chat completions API with a fixed script,
// one turn per request, and records the tools each request declared.
type scriptedModel struct {
	mu       sync.Mutex
	script   []turn
	next     int
	declared [][]string
	// first is the first request's body, which carries the prompt.
	first string
}

func (m *scriptedModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(raw, &body)

	m.mu.Lock()
	if m.first == "" {
		m.first = string(raw)
	}
	var names []string
	for _, tool := range body.Tools {
		names = append(names, tool.Function.Name)
	}
	m.declared = append(m.declared, names)
	t := m.script[min(m.next, len(m.script)-1)]
	m.next++
	n := m.next
	m.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	send := func(v any) {
		data, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	}
	chunk := func(delta map[string]any, finish any, usage any) map[string]any {
		c := map[string]any{"id": fmt.Sprint("c", n), "object": "chat.completion.chunk", "model": "m1",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		if usage != nil {
			c["usage"] = usage
		}
		return c
	}
	usage := map[string]int{"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110}
	if t.tool != "" {
		args, _ := json.Marshal(t.args)
		send(chunk(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
			"index": 0, "id": fmt.Sprint("call_", n), "type": "function",
			"function": map[string]any{"name": t.tool, "arguments": string(args)},
		}}}, nil, nil))
		send(chunk(map[string]any{}, "tool_calls", usage))
	} else {
		send(chunk(map[string]any{"role": "assistant", "content": t.text}, nil, nil))
		send(chunk(map[string]any{}, "stop", usage))
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

// TestWithTheRealCLI runs pi itself, with the guard extension and kibitz-mcp,
// against a scripted model. It needs both binaries:
//
//	KIBITZ_PI_IT_BIN=$(which pi) KIBITZ_PI_IT_MCP=/path/to/kibitz-mcp go test ./internal/reviewer/pi/
func TestWithTheRealCLI(t *testing.T) {
	bin, mcp := os.Getenv("KIBITZ_PI_IT_BIN"), os.Getenv("KIBITZ_PI_IT_MCP")
	if bin == "" || mcp == "" {
		t.Skip("KIBITZ_PI_IT_BIN and KIBITZ_PI_IT_MCP are not set")
	}
	guard, err := filepath.Abs("../../../deploy/pi/kibitz-guard.ts")
	if err != nil {
		t.Fatal(err)
	}

	agents, err := filepath.Abs("../../../deploy/opencode/agents")
	if err != nil {
		t.Fatal(err)
	}

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "queue.go"), []byte("package queue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("do not read"), 0o600); err != nil {
		t.Fatal(err)
	}

	review := `{"schema_version":1,"summary":"ok","comments":[{"path":"queue.go","line":1,"severity":"high","title":"t","body":"b"}]}`
	model := &scriptedModel{script: []turn{
		{tool: "read", args: map[string]any{"path": "queue.go"}},
		{tool: "read", args: map[string]any{"path": outside}},
		{tool: "bash", args: map[string]any{"command": "id"}},
		{tool: "write", args: map[string]any{"path": "queue.go", "content": "overwritten"}},
		{tool: "mcp__kibitz__get_pr_metadata", args: map[string]any{}},
		{tool: "write", args: map[string]any{"path": ".kibitz/out/review.json", "content": review}},
		{text: "書きました"},
	}}
	server := httptest.NewServer(model)
	defer server.Close()

	runner := pi.New(pi.Config{
		Bin:            bin,
		Model:          "mock/m1",
		GuardExtension: guard,
		AgentsDir:      agents,
		SessionDir:     t.TempDir(),
		ContextBin:     mcp,
		CustomProvider: &opencode.CustomProvider{
			ID:      "mock",
			BaseURL: server.URL + "/v1",
			Token:   func(context.Context) (string, error) { return "x", nil },
		},
	}, discardLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := runner.Run(ctx, request(workspace, reviewer.ModeReview))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(result.Findings) != 1 {
		t.Errorf("findings = %+v", result.Findings)
	}
	if data, _ := os.ReadFile(filepath.Join(workspace, "queue.go")); string(data) != "package queue\n" {
		t.Errorf("the checkout was written: %q", data)
	}
	// The read outside the checkout, the shell and the stray write all
	// came back as errors; the rest succeeded.
	wantFailed := map[string]int{"read": 1, "bash": 1, "write": 1}
	for name, n := range wantFailed {
		if result.Tools.Failed[name] != n {
			t.Errorf("%s failed %d times, want %d (calls %v, failed %v)", name, result.Tools.Failed[name], n, result.Tools.Calls, result.Tools.Failed)
		}
	}
	if result.Tools.Calls["mcp__kibitz__get_pr_metadata"] != 1 || result.Tools.Failed["mcp__kibitz__get_pr_metadata"] != 0 {
		t.Errorf("kibitz-mcp was not reached: %v / %v", result.Tools.Calls, result.Tools.Failed)
	}
	if result.Usage.InputTokens == 0 || !strings.HasPrefix(result.SessionID, "kibitz-") {
		t.Errorf("usage = %+v, session = %q", result.Usage, result.SessionID)
	}

	// The prompt file was attached, and the agent definition appended to
	// the system prompt.
	for _, want := range []string{"Add the SQS subscriber", "あなたはコードレビュアーです"} {
		if !strings.Contains(model.first, want) {
			t.Errorf("the first request does not carry %q", want)
		}
	}

	declared := model.declared[0]
	for _, want := range []string{"read", "grep", "find", "ls", "write", "mcp__kibitz__get_pr_metadata"} {
		if !slices.Contains(declared, want) {
			t.Errorf("%s was not declared: %v", want, declared)
		}
	}
	for _, unwanted := range []string{"bash", "edit", "codemode", "tool_search"} {
		if slices.Contains(declared, unwanted) {
			t.Errorf("%s was declared: %v", unwanted, declared)
		}
	}
}
