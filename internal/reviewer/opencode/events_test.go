package opencode_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/reviewer/opencode"
)

// toolStream plays an agent that used its tools, in the shape opencode 1.18.31
// prints them: one object per line, the tool part reported once it has
// finished, and kibitz's own tools carrying the server's name as a prefix.
const toolStream = `
cp "$OPENCODE_CONFIG" .kibitz/config-copy.json
mkdir -p .kibitz/out
cat > .kibitz/out/review.json <<'JSON'
{"schema_version": 1, "summary": "ordering key の扱いを変える", "comments": []}
JSON
echo '{"type":"tool_use","timestamp":1,"sessionID":"ses_1","part":{"id":"prt_1","type":"tool","callID":"c1","tool":"glob","state":{"status":"completed","input":{"pattern":"**/*.go"},"output":"queue.go"}}}'
echo '{"type":"tool_use","timestamp":2,"sessionID":"ses_1","part":{"id":"prt_2","type":"tool","callID":"c2","tool":"kibitz_search_docs","state":{"status":"completed","input":{"query":"ordering key 順序"},"output":"<<< docs/adr/0005-pubsub-with-per-pr-ordering-key.md … >>>"}}}'
echo '{"type":"tool_use","timestamp":3,"sessionID":"ses_1","part":{"id":"prt_3","type":"tool","callID":"c3","tool":"kibitz_get_doc","state":{"status":"completed","input":{"path":"docs/adr/0005-pubsub-with-per-pr-ordering-key.md"},"output":"# キューは…"}}}'
echo '{"type":"tool_use","timestamp":4,"sessionID":"ses_1","part":{"id":"prt_4","type":"tool","callID":"c4","tool":"read","state":{"status":"running","input":{"filePath":"queue.go"}}}}'
echo '{"type":"tool_use","timestamp":5,"sessionID":"ses_1","part":{"id":"prt_4","type":"tool","callID":"c4","tool":"read","state":{"status":"completed","input":{"filePath":"queue.go"},"output":"package main"}}}'
echo '{"type":"tool_use","timestamp":6,"sessionID":"ses_1","part":{"id":"prt_6","type":"tool","callID":"c6","tool":"kibitz_get_doc","state":{"status":"completed","input":{"path":"docs/adr/0005-pubsub-with-per-pr-ordering-key.md"},"output":"# キューは…"}}}'
echo '{"type":"tool_use","timestamp":7,"sessionID":"ses_1","part":{"id":"prt_7","type":"tool","callID":"c7","tool":"kibitz_get_doc","state":{"status":"error","input":{"path":"docs/adr/9999-nope.md"},"error":"設計文書の一覧にありません"}}}'
echo '{"type":"step_finish","timestamp":8,"sessionID":"ses_1","part":{"id":"prt_8","type":"step-finish","reason":"stop","tokens":{"input":900,"output":120,"reasoning":0,"cache":{"read":0,"write":0}}}}'
`

func toolUse(t *testing.T) reviewer.ToolUse {
	t.Helper()

	h := newHarness(t, toolStream)
	runner := opencode.New(opencode.Config{Bin: h.bin}, nil)
	result, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return result.Tools
}

func TestToolCallsAreCounted(t *testing.T) {
	tools := toolUse(t)

	want := map[string]int{
		"glob":               1,
		"kibitz_search_docs": 1,
		"kibitz_get_doc":     3,
		"read":               1,
	}
	for name, n := range want {
		if tools.Calls[name] != n {
			t.Errorf("Calls[%q] = %d, want %d (all: %v)", name, tools.Calls[name], n, tools.Calls)
		}
	}
	if len(tools.Calls) != len(want) {
		t.Errorf("Calls = %v, want exactly %v", tools.Calls, want)
	}
	// Six finished calls, not seven: the read is reported once running and
	// once completed, and counting both would count it twice.
	if got := tools.Total(); got != 6 {
		t.Errorf("Total = %d, want 6", got)
	}
	if tools.Failed["kibitz_get_doc"] != 1 {
		t.Errorf("Failed = %v, want one failed kibitz_get_doc", tools.Failed)
	}
}

// The question this whole change exists to answer: were the decision records
// in the prompt's index actually opened?
func TestReferenceDocumentsAreNamed(t *testing.T) {
	tools := toolUse(t)

	want := []string{"docs/adr/0005-pubsub-with-per-pr-ordering-key.md"}
	if !slices.Equal(tools.Documents, want) {
		t.Errorf("Documents = %v, want %v", tools.Documents, want)
	}
	if tools.Searches != 1 {
		t.Errorf("Searches = %d, want 1", tools.Searches)
	}
}

// A run that used no tools reports none rather than an empty map that reads
// like a failure to parse.
func TestNoToolsIsNotAnError(t *testing.T) {
	h := newHarness(t, writeOutputWithoutTools)
	runner := opencode.New(opencode.Config{Bin: h.bin}, nil)

	result, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := result.Tools.Total(); got != 0 {
		t.Errorf("Total = %d, want 0", got)
	}
	if result.Tools.Documents != nil || result.Tools.Searches != 0 {
		t.Errorf("Tools = %+v, want nothing consulted", result.Tools)
	}
}

const writeOutputWithoutTools = `
mkdir -p .kibitz/out
cat > .kibitz/out/review.json <<'JSON'
{"schema_version": 1, "summary": "変更なし", "comments": []}
JSON
echo '{"type":"step_finish","timestamp":1,"sessionID":"ses_2","part":{"id":"p","type":"step-finish","reason":"stop","tokens":{"input":10,"output":2,"reasoning":0,"cache":{"read":0,"write":0}}}}'
`

// The fixture this reads is not written by hand: it is the stdout of a real
// `opencode run --format json` (1.18.31), driven against a stub of an
// OpenAI-compatible endpoint that answered with one tool call, with the real
// kibitz-mcp registered and serving this repository's own decision records.
//
// It is here because the shape of that stream is the one thing in this file
// kibitz does not control. It also settles how opencode namespaces a tool
// server's tools — "kibitz_get_doc", the server's name and an underscore —
// which is what [reviewer.ToolMatches] exists to absorb without depending on.
func TestRealOpencodeStream(t *testing.T) {
	stream, err := os.ReadFile(filepath.Join("testdata", "opencode-1.18.31-tool-use.jsonl"))
	if err != nil {
		t.Fatalf("reading the captured stream: %v", err)
	}

	h := newHarness(t, replay(t, stream))
	runner := opencode.New(opencode.Config{Bin: h.bin}, nil)
	result, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := result.Tools.Calls["kibitz_get_doc"]; got != 1 {
		t.Errorf("Calls = %v, want one kibitz_get_doc", result.Tools.Calls)
	}
	want := []string{"docs/adr/0005-pubsub-with-per-pr-ordering-key.md"}
	if !slices.Equal(result.Tools.Documents, want) {
		t.Errorf("Documents = %v, want %v", result.Tools.Documents, want)
	}
	// The same stream still has to yield the token counts, which come from a
	// different part of it.
	if result.Usage.InputTokens != 1800 || result.Usage.OutputTokens != 40 {
		t.Errorf("Usage = %+v, want 1800 in and 40 out across the two steps", result.Usage)
	}
	if result.SessionID != "ses_f367e4448ffeViBfEaApTR178w" {
		t.Errorf("SessionID = %q", result.SessionID)
	}
}

// replay makes a fake CLI that prints a captured stream verbatim, and writes
// the output document the runner requires alongside it.
func replay(t *testing.T, stream []byte) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "stream.jsonl")
	if err := os.WriteFile(path, stream, 0o600); err != nil {
		t.Fatalf("writing the stream: %v", err)
	}
	return `
mkdir -p .kibitz/out
cat > .kibitz/out/review.json <<'JSON'
{"schema_version": 1, "summary": "ordering key の扱い", "comments": []}
JSON
cat '` + path + `'
`
}
