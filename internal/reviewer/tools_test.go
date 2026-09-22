package reviewer_test

import (
	"testing"

	"github.com/yteraoka/kibitz/internal/reviewer"
)

// The prompt names kibitz's tools unqualified because an engine namespaces a
// tool server's tools however it likes. Reading the stream back has to make
// the same allowance, or the counters report that nobody opens the decision
// records when what actually happened is that opencode called them
// "kibitz_get_doc".
func TestToolMatches(t *testing.T) {
	cases := []struct {
		reported string
		want     string
		match    bool
	}{
		{"get_doc", reviewer.GetDocTool, true},
		{"kibitz_get_doc", reviewer.GetDocTool, true},
		{"kibitz.get_doc", reviewer.GetDocTool, true},
		{"kibitz/get_doc", reviewer.GetDocTool, true},
		{"kibitz-get_doc", reviewer.GetDocTool, true},
		{"kibitz:get_doc", reviewer.GetDocTool, true},
		{"mcp__kibitz__get_doc", reviewer.GetDocTool, true},
		{"KIBITZ_GET_DOC", reviewer.GetDocTool, true},
		{"search_docs", reviewer.SearchDocsTool, true},
		{"kibitz_search_docs", reviewer.SearchDocsTool, true},

		// A name that merely ends in the same letters is a different tool:
		// the character before the suffix has to be a separator.
		{"forget_doc", reviewer.GetDocTool, false},
		{"widget_doc", reviewer.GetDocTool, false},
		{"research_docs", reviewer.SearchDocsTool, false},

		{"get_doc_v2", reviewer.GetDocTool, false},
		{"read", reviewer.GetDocTool, false},
		{"doc", reviewer.GetDocTool, false},
		{"", reviewer.GetDocTool, false},
		{"kibitz_get_doc", reviewer.SearchDocsTool, false},
	}

	for _, c := range cases {
		if got := reviewer.ToolMatches(c.reported, c.want); got != c.match {
			t.Errorf("ToolMatches(%q, %q) = %v, want %v", c.reported, c.want, got, c.match)
		}
	}
}

func TestToolUseTotal(t *testing.T) {
	var empty reviewer.ToolUse
	if got := empty.Total(); got != 0 {
		t.Errorf("Total of nothing = %d, want 0", got)
	}

	tools := reviewer.ToolUse{Calls: map[string]int{"read": 3, "kibitz_get_doc": 2}}
	if got := tools.Total(); got != 5 {
		t.Errorf("Total = %d, want 5", got)
	}
}
