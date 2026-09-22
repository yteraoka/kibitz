package reviewer_test

import (
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/reviewer"
)

func TestParseTriage(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "a selection",
			body: `{"schema_version":1,"paths":["a.go"," b.go ",""],"notes":"生成物を除外"}`,
			want: []string{"a.go", "b.go"},
		},
		{name: "not json", body: "{"},
		{name: "wrong schema", body: `{"schema_version":99,"paths":["a.go"]}`},
		{name: "nothing selected", body: `{"schema_version":1,"paths":[]}`},
		{name: "only blanks", body: `{"schema_version":1,"paths":["  "]}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := reviewer.ParseTriage([]byte(tc.body))
			if tc.want == nil {
				if err == nil {
					t.Fatalf("ParseTriage = %+v, want an error", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTriage: %v", err)
			}
			if len(out.Paths) != len(tc.want) {
				t.Fatalf("paths = %v, want %v", out.Paths, tc.want)
			}
			for i, p := range tc.want {
				if out.Paths[i] != p {
					t.Errorf("path %d = %q, want %q", i, out.Paths[i], p)
				}
			}
		})
	}
}

// The triage prompt exists to avoid reading the diff, so it must not contain
// it.
func TestTriagePromptCarriesNamesNotCode(t *testing.T) {
	req := reviewer.Request{
		Mode:        reviewer.ModeTriage,
		Event:       &event.ReviewEvent{Repository: event.Repository{FullName: "yteraoka/kibitz"}},
		PullRequest: &event.PullRequest{Number: 42, Title: "大きな変更"},
		Diff: &forge.Diff{Files: []forge.File{
			{Path: "queue.go", Status: forge.FileModified, Additions: 10, Deletions: 2, Patch: "@@ -1 +1 @@\n-secret\n+alsosecret"},
		}},
	}

	prompt := reviewer.BuildPrompt(req)
	if !strings.Contains(prompt, "queue.go") {
		t.Errorf("the prompt does not list the changed file:\n%s", prompt)
	}
	if !strings.Contains(prompt, "+10/-2") {
		t.Errorf("the prompt does not say how much changed:\n%s", prompt)
	}
	if strings.Contains(prompt, "alsosecret") {
		t.Errorf("the prompt carries the patch it exists to avoid reading:\n%s", prompt)
	}
	if !strings.Contains(prompt, reviewer.TriageOutputPath) {
		t.Errorf("the prompt does not state the output contract:\n%s", prompt)
	}
}
