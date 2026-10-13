package adk

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/reviewer"
)

// TestRunWithKibitzMCP runs kibitz's own MCP server as a real process behind
// ADK's MCP toolset. It is what tells whether the minimal server in
// internal/mcp and the official Go SDK's client understand each other, which
// no amount of faking either side can.
func TestRunWithKibitzMCP(t *testing.T) {
	if testing.Short() {
		t.Skip("builds kibitz-mcp")
	}
	bin := filepath.Join(t.TempDir(), "kibitz-mcp")
	build := exec.Command("go", "build", "-o", bin, "github.com/yteraoka/kibitz/cmd/kibitz-mcp")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building kibitz-mcp: %v\n%s", err, out)
	}

	h := newHarness(t)
	var metadata map[string]any
	m := &scriptedModel{steps: []func(*model.LLMRequest) *model.LLMResponse{
		call("get_pr_metadata", map[string]any{}),
		func(req *model.LLMRequest) *model.LLMResponse {
			metadata = lastResponse(req)
			return say("タイトルは「panic を直す」です。")(req)
		},
	}}
	r := New(Config{
		Model:      "gemini-3.1-pro-preview",
		AgentsDir:  h.agents,
		ContextBin: bin,
		newModel:   func(context.Context, string) (model.LLM, error) { return m, nil },
	}, discardLogger())

	req := h.request(reviewer.ModeAnswer)
	req.PullRequest = &event.PullRequest{Number: 7, Title: "panic を直す"}
	req.References = []reviewer.Reference{{Path: "main.go", Title: "entry point"}}
	res, err := r.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := declared(m.requests[0])
	for _, want := range []string{"get_pr_metadata", "get_pr_diff", reviewer.GetDocTool, reviewer.SearchDocsTool, ReadFileTool} {
		if !slices.Contains(got, want) {
			t.Errorf("declared tools = %v, want %s among them", got, want)
		}
	}
	if text := fmt.Sprint(metadata); !strings.Contains(text, "panic を直す") || strings.Contains(text, "error") {
		t.Errorf("get_pr_metadata returned %v", metadata)
	}
	if res.Tools.Calls["get_pr_metadata"] != 1 || res.Tools.Failed["get_pr_metadata"] != 0 {
		t.Errorf("tools = %+v", res.Tools)
	}
}
