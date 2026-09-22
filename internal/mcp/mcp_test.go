package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/mcp"
)

func testServer() *mcp.Server {
	return &mcp.Server{
		Name:         "kibitz",
		Version:      "test",
		Instructions: "facts about one pull request",
		Tools: []mcp.Tool{{
			Name:        "echo",
			Description: "returns what it was given",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"text": map[string]any{"type": "string"}},
			},
			Handler: func(_ context.Context, arguments json.RawMessage) (string, error) {
				var args struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(arguments, &args)
				if args.Text == "boom" {
					return "", errors.New("it went wrong")
				}
				return "you said " + args.Text, nil
			},
		}},
	}
}

// exchange runs a session and returns one decoded response per answered
// request, in order.
func exchange(t *testing.T, lines ...string) []map[string]any {
	t.Helper()

	var out strings.Builder
	if err := testServer().Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	var messages []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("the server wrote a line that is not JSON: %q", line)
		}
		if m["jsonrpc"] != "2.0" {
			t.Errorf("jsonrpc = %v, want 2.0", m["jsonrpc"])
		}
		messages = append(messages, m)
	}
	return messages
}

func TestInitializeEchoesTheClientsVersion(t *testing.T) {
	got := exchange(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{}}}`)
	if len(got) != 1 {
		t.Fatalf("%d responses, want 1", len(got))
	}
	result := got[0]["result"].(map[string]any)
	// Agreeing with the client is safer than insisting: this server speaks
	// only the methods that have not changed between versions.
	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v, want the client's", result["protocolVersion"])
	}
	if _, ok := result["capabilities"].(map[string]any)["tools"]; !ok {
		t.Errorf("tools capability is missing: %v", result["capabilities"])
	}
	if result["serverInfo"].(map[string]any)["name"] != "kibitz" {
		t.Errorf("serverInfo = %v", result["serverInfo"])
	}
}

func TestInitializeWithoutAVersion(t *testing.T) {
	got := exchange(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if v := got[0]["result"].(map[string]any)["protocolVersion"]; v != mcp.DefaultProtocolVersion {
		t.Errorf("protocolVersion = %v, want the default", v)
	}
}

// A notification has no id, and the protocol forbids answering it. Every
// client sends notifications/initialized, so a server that replied would put a
// response nobody asked for in front of the next one.
func TestNotificationsAreNotAnswered(t *testing.T) {
	got := exchange(t,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":7,"method":"ping"}`,
	)
	if len(got) != 1 {
		t.Fatalf("%d responses, want only the one for ping", len(got))
	}
	if got[0]["id"].(float64) != 7 {
		t.Errorf("the response is for id %v", got[0]["id"])
	}
}

func TestToolsList(t *testing.T) {
	got := exchange(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := got[0]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("%d tools, want 1", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "echo" {
		t.Errorf("name = %v", tool["name"])
	}
	// The schema is required even for a tool that takes nothing.
	if tool["inputSchema"].(map[string]any)["type"] != "object" {
		t.Errorf("inputSchema = %v", tool["inputSchema"])
	}
	// The handler is not part of the wire format.
	if _, leaked := tool["Handler"]; leaked {
		t.Error("the handler was serialized")
	}
}

func TestToolsCall(t *testing.T) {
	got := exchange(t,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"boom"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
	)
	if len(got) != 3 {
		t.Fatalf("%d responses, want 3", len(got))
	}

	first := got[0]["result"].(map[string]any)
	if first["isError"] != false {
		t.Errorf("a tool that worked reported an error: %v", first)
	}
	if text := first["content"].([]any)[0].(map[string]any)["text"]; text != "you said hi" {
		t.Errorf("text = %v", text)
	}

	// A tool that fails says so as content, not as a JSON-RPC error: the model
	// is the one who can do something about it, and a protocol error never
	// reaches it.
	for _, i := range []int{1, 2} {
		if _, isProtocolError := got[i]["error"]; isProtocolError {
			t.Errorf("response %d is a protocol error: %v", i, got[i])
			continue
		}
		result := got[i]["result"].(map[string]any)
		if result["isError"] != true {
			t.Errorf("response %d did not report the failure: %v", i, result)
		}
	}
}

// One bad line is not a reason to drop a session the client still believes in.
func TestMalformedLinesDoNotEndTheSession(t *testing.T) {
	got := exchange(t,
		`this is not json`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
	)
	if len(got) != 2 {
		t.Fatalf("%d responses, want 2", len(got))
	}
	if code := got[0]["error"].(map[string]any)["code"].(float64); int(code) != mcp.CodeParseError {
		t.Errorf("code = %v, want a parse error", code)
	}
	if _, ok := got[1]["result"]; !ok {
		t.Errorf("the session did not continue: %v", got[1])
	}
}

func TestUnknownMethod(t *testing.T) {
	got := exchange(t, `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`)
	if code := got[0]["error"].(map[string]any)["code"].(float64); int(code) != mcp.CodeMethodNotFound {
		t.Errorf("code = %v, want method not found", code)
	}
}

// The CLI and the server share one set of handlers, so the two cannot drift.
func TestCall(t *testing.T) {
	text, err := testServer().Call(context.Background(), "echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if text != "you said hi" {
		t.Errorf("text = %q", text)
	}
	if _, err := testServer().Call(context.Background(), "nope", nil); !errors.Is(err, mcp.ErrNoTool) {
		t.Errorf("err = %v, want ErrNoTool", err)
	}
}
