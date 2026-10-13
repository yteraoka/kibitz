package adk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/yteraoka/kibitz/internal/reviewer"
)

func TestGlobRegexp(t *testing.T) {
	cases := []struct {
		pattern string
		match   []string
		miss    []string
	}{
		{"*.go", []string{"main.go"}, []string{"internal/x.go", "main.go.orig"}},
		{"**/*.go", []string{"main.go", "internal/x/x.go"}, []string{"README.md"}},
		{"internal/**", []string{"internal/x", "internal/x/y.go"}, []string{"internalx", "cmd/internal/x"}},
		{"internal/**/*_test.go", []string{"internal/a_test.go", "internal/x/y/a_test.go"}, []string{"internal/a.go"}},
		{"?.md", []string{"a.md"}, []string{"ab.md", "a/b.md"}},
		{"[ab].txt", []string{"a.txt"}, []string{"c.txt"}},
		{"[!ab].txt", []string{"c.txt"}, []string{"a.txt"}},
		{"*.{go,md}", []string{"a.go", "b.md"}, []string{"c.txt"}},
		{"a+b(c).txt", []string{"a+b(c).txt"}, []string{"aab(c).txt"}},
	}
	for _, c := range cases {
		re, err := globRegexp(c.pattern)
		if err != nil {
			t.Fatalf("globRegexp(%q): %v", c.pattern, err)
		}
		for _, p := range c.match {
			if !re.MatchString(p) {
				t.Errorf("%q does not match %q", c.pattern, p)
			}
		}
		for _, p := range c.miss {
			if re.MatchString(p) {
				t.Errorf("%q matches %q", c.pattern, p)
			}
		}
	}
	for _, bad := range []string{"[abc", "{a,b"} {
		if _, err := globRegexp(bad); err == nil {
			t.Errorf("globRegexp(%q) was accepted", bad)
		}
	}
}

func testWorkspace(t *testing.T, writable func(string) bool) *workspace {
	t.Helper()
	h := newHarness(t)
	if err := os.MkdirAll(filepath.Join(h.workspace, ".git", "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		".git/objects/blob":  "panic in the object store\n",
		"bin.dat":            "panic\x00binary\n",
		"internal/x/long.go": strings.Repeat("line\n", 3000),
	} {
		if err := os.WriteFile(filepath.Join(h.workspace, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := openWorkspace(h.workspace, writable)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

func TestReadPagesAndRefuses(t *testing.T) {
	ws := testWorkspace(t, nil)
	var ctx agent.Context

	res, err := ws.read(ctx, readArgs{Path: "/internal/x/long.go"})
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalLines != 3000 || !res.Truncated || strings.Count(res.Content, "\n") != defaultReadLines {
		t.Errorf("total=%d truncated=%v lines=%d", res.TotalLines, res.Truncated, strings.Count(res.Content, "\n"))
	}
	res, err = ws.read(ctx, readArgs{Path: "main.go", Offset: 4, Limit: 1})
	if err != nil || res.Content != "     4\t\tpanic(\"boom\")\n" {
		t.Errorf("read = %q, %v", res.Content, err)
	}

	for _, p := range []string{"bin.dat", "internal", "missing.go", "../secret.txt", "link.txt"} {
		if _, err := ws.read(ctx, readArgs{Path: p}); err == nil {
			t.Errorf("read(%q) succeeded", p)
		} else if strings.Contains(err.Error(), os.TempDir()) {
			t.Errorf("read(%q) error names a host path: %v", p, err)
		}
	}
}

func TestListGlobGrep(t *testing.T) {
	ws := testWorkspace(t, nil)
	var ctx agent.Context

	list, err := ws.list(ctx, listArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(list.Entries, "internal/") || !slices.Contains(list.Entries, "main.go") {
		t.Errorf("list = %v", list.Entries)
	}

	glob, err := ws.glob(ctx, globArgs{Pattern: "**/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(glob.Paths, []string{"internal/x/long.go", "internal/x/x.go", "main.go"}) {
		t.Errorf("glob = %v", glob.Paths)
	}
	if glob, _ := ws.glob(ctx, globArgs{Pattern: "**/blob"}); len(glob.Paths) != 0 {
		t.Errorf("glob reached into .git: %v", glob.Paths)
	}

	grep, err := ws.grepIn(context.Background(), grepArgs{Pattern: `panic\(`})
	if err != nil {
		t.Fatal(err)
	}
	// Not the binary file, not .git, and not the file the link points to.
	if !slices.Equal(grep.Matches, []string{`main.go:4: 	panic("boom")`}) {
		t.Errorf("grep = %q", grep.Matches)
	}
	if grep, _ := ws.grepIn(context.Background(), grepArgs{Pattern: "package", Include: "internal/**"}); len(grep.Matches) != 1 {
		t.Errorf("grep with include = %q", grep.Matches)
	}
	if _, err := ws.grepIn(context.Background(), grepArgs{Pattern: "("}); err == nil {
		t.Error("an invalid pattern was accepted")
	}
	if _, err := ws.grepIn(context.Background(), grepArgs{Pattern: "x", Path: "../"}); err == nil {
		t.Error("grep outside the checkout succeeded")
	}
}

func TestWriteOnlyWhereAllowed(t *testing.T) {
	ws := testWorkspace(t, writableFor(reviewer.ModeReview))
	var ctx agent.Context

	if _, err := ws.write(ctx, writeArgs{Path: reviewer.OutputPath, Content: "{}"}); err != nil {
		t.Fatalf("write to the output: %v", err)
	}
	for _, p := range []string{"main.go", ".kibitz/out/other.json", ".kibitz/out/../../main.go", "../escape.json", ".kibitz/prompt.md"} {
		if _, err := ws.write(ctx, writeArgs{Path: p, Content: "x"}); err == nil {
			t.Errorf("write(%q) succeeded", p)
		}
	}

	noWrite := testWorkspace(t, nil)
	tools, err := noWrite.tools()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(toolNames(tools), WriteFileTool) {
		t.Errorf("a workspace without writable paths offers %s", WriteFileTool)
	}
}

func TestUsageOf(t *testing.T) {
	got := usageOf(&genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:        1000,
		CachedContentTokenCount: 700,
		ToolUsePromptTokenCount: 50,
		CandidatesTokenCount:    80,
		ThoughtsTokenCount:      40,
	})
	want := reviewer.Usage{InputTokens: 350, CacheReadTokens: 700, OutputTokens: 80, ReasoningTokens: 40}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

func TestTranscriptCountsDocuments(t *testing.T) {
	var tr transcript
	tr.record(nil)
	for _, p := range []*genai.Part{
		{FunctionCall: &genai.FunctionCall{Name: "get_doc", Args: map[string]any{"path": "docs/adr/0001.md"}}},
		{FunctionCall: &genai.FunctionCall{Name: "get_doc", Args: map[string]any{"path": "docs/adr/0001.md"}}},
		{FunctionCall: &genai.FunctionCall{Name: "search_docs", Args: map[string]any{"query": "x"}}},
		{FunctionResponse: &genai.FunctionResponse{Name: "get_doc", Response: map[string]any{"output": "…"}}},
		{FunctionResponse: &genai.FunctionResponse{Name: "get_doc", Response: map[string]any{"error": "no such document"}}},
	} {
		tr.record(eventWith(p))
	}
	if !slices.Equal(tr.tools.Documents, []string{"docs/adr/0001.md"}) || tr.tools.Searches != 1 {
		t.Errorf("tools = %+v", tr.tools)
	}
	if tr.tools.Calls["get_doc"] != 2 || tr.tools.Failed["get_doc"] != 1 {
		t.Errorf("calls = %v failed = %v", tr.tools.Calls, tr.tools.Failed)
	}
}

type fakeTool struct{ name string }

func (f fakeTool) Name() string        { return f.name }
func (f fakeTool) Description() string { return "" }
func (f fakeTool) IsLongRunning() bool { return false }

type fakeToolset struct {
	name  string
	tools []string
	err   error
}

func (f fakeToolset) Name() string { return f.name }
func (f fakeToolset) Tools(agent.ReadonlyContext) ([]tool.Tool, error) {
	var out []tool.Tool
	for _, n := range f.tools {
		out = append(out, fakeTool{n})
	}
	return out, f.err
}

func TestUniqueToolsetDropsTakenNames(t *testing.T) {
	u := &uniqueToolset{
		reserved: []string{ReadFileTool},
		sets: []tool.Toolset{
			fakeToolset{name: "a", tools: []string{"search", ReadFileTool}},
			fakeToolset{name: "b", err: errors.New("down")},
			fakeToolset{name: "c", tools: []string{"search", "fetch"}},
		},
		logger: discardLogger(),
	}
	tools, err := u.Tools(readonlyContext{ctx: context.Background()})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(tools); !slices.Equal(got, []string{"fetch", "search"}) {
		t.Errorf("tools = %v", got)
	}
}

func eventWith(p *genai.Part) *session.Event {
	ev := session.NewEvent(context.Background(), "test")
	ev.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{p}}
	return ev
}

// readonlyContext is the part of agent.ReadonlyContext a toolset uses: a
// context. The rest panics if anything reaches for it.
type readonlyContext struct {
	ctx context.Context
	agent.ReadonlyContext
}

func (c readonlyContext) Deadline() (time.Time, bool) { return c.ctx.Deadline() }
func (c readonlyContext) Done() <-chan struct{}       { return c.ctx.Done() }
func (c readonlyContext) Err() error                  { return c.ctx.Err() }
func (c readonlyContext) Value(key any) any           { return c.ctx.Value(key) }
