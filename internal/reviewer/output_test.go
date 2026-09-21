package reviewer_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/reviewer"
)

func validOutput() map[string]any {
	return map[string]any{
		"schema_version": reviewer.OutputSchemaVersion,
		"summary":        "SQS subscriber を追加する変更",
		"comments": []map[string]any{
			{
				"path": "queue.go", "line": 12, "severity": "high",
				"title": "goroutine が漏れる", "body": "ctx がキャンセルされても終了しない",
			},
		},
	}
}

func encode(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	return b
}

func TestParseOutput(t *testing.T) {
	out, err := reviewer.ParseOutput(encode(t, validOutput()))
	if err != nil {
		t.Fatalf("ParseOutput: %v", err)
	}
	if out.Summary == "" || len(out.Comments) != 1 {
		t.Fatalf("output = %+v", out)
	}
}

// The parse errors are fed back to the agent on a retry, so each one has to
// name the field that was wrong.
func TestParseOutputRejections(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"wrong schema", func(m map[string]any) { m["schema_version"] = 99 }, "schema_version"},
		{"no summary", func(m map[string]any) { m["summary"] = "  " }, "summary"},
		{
			name:   "no path",
			mutate: func(m map[string]any) { m["comments"].([]map[string]any)[0]["path"] = "" },
			want:   "path",
		},
		{
			name:   "line zero",
			mutate: func(m map[string]any) { m["comments"].([]map[string]any)[0]["line"] = 0 },
			want:   "line",
		},
		{
			name:   "range runs backwards",
			mutate: func(m map[string]any) { m["comments"].([]map[string]any)[0]["end_line"] = 3 },
			want:   "end_line",
		},
		{
			name:   "no body",
			mutate: func(m map[string]any) { m["comments"].([]map[string]any)[0]["body"] = "" },
			want:   "body",
		},
		{
			name:   "invented severity",
			mutate: func(m map[string]any) { m["comments"].([]map[string]any)[0]["severity"] = "catastrophic" },
			want:   "severity",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := validOutput()
			tc.mutate(payload)

			_, err := reviewer.ParseOutput(encode(t, payload))
			if err == nil {
				t.Fatal("ParseOutput accepted invalid output")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}

	if _, err := reviewer.ParseOutput([]byte("not json")); err == nil {
		t.Error("ParseOutput accepted non-JSON")
	}
}

func TestPositions(t *testing.T) {
	diff := &forge.Diff{Files: []forge.File{
		{
			Path: "queue.go",
			Patch: strings.Join([]string{
				"@@ -10,6 +10,8 @@ func Receive(",
				" \tfor {",
				"-\t\told()",
				"+\t\tnew()",
				"+\t\tmore()",
				" \t\tcontext()",
				" \t}",
			}, "\n"),
		},
		{Path: "logo.png", Patch: ""},
	}}

	p := reviewer.NewPositions(diff)

	// The hunk starts at line 10 on the new side: context 10, added 11 and 12,
	// then context 13 and 14.
	for _, line := range []int{10, 11, 12, 13, 14} {
		if !p.Allows("queue.go", line) {
			t.Errorf("line %d should be commentable", line)
		}
	}
	for _, line := range []int{9, 15, 100} {
		if p.Allows("queue.go", line) {
			t.Errorf("line %d should not be commentable", line)
		}
	}
	if p.Allows("logo.png", 1) {
		t.Error("a binary file has no commentable lines")
	}
	if p.Allows("untouched.go", 1) {
		t.Error("a file outside the diff has no commentable lines")
	}
	if p.Files() != 1 {
		t.Errorf("Files() = %d, want 1", p.Files())
	}
}

func TestPositionsHandlesMultipleHunks(t *testing.T) {
	diff := &forge.Diff{Files: []forge.File{{
		Path: "main.go",
		Patch: strings.Join([]string{
			"@@ -1,3 +1,4 @@",
			" package main",
			"+import \"os\"",
			" ",
			"@@ -20,2 +21,3 @@ func main() {",
			" \tx := 1",
			"+\ty := 2",
		}, "\n"),
	}}}

	p := reviewer.NewPositions(diff)
	for _, line := range []int{1, 2, 3, 21, 22} {
		if !p.Allows("main.go", line) {
			t.Errorf("line %d should be commentable", line)
		}
	}
	if p.Allows("main.go", 10) {
		t.Error("a line between hunks should not be commentable")
	}
}

func positionsFor(paths map[string][]int) *reviewer.Positions {
	files := make([]forge.File, 0, len(paths))
	for path, lines := range paths {
		var b strings.Builder
		for _, line := range lines {
			fmt.Fprintf(&b, "@@ -%d,1 +%d,1 @@\n+x\n", line, line)
		}
		files = append(files, forge.File{Path: path, Patch: b.String()})
	}
	return reviewer.NewPositions(&forge.Diff{Files: files})
}

func TestSanitize(t *testing.T) {
	positions := positionsFor(map[string][]int{"queue.go": {10, 20, 30}})

	out := &reviewer.Output{
		SchemaVersion: reviewer.OutputSchemaVersion,
		Summary:       "ok",
		Comments: []reviewer.OutputComment{
			{Path: "queue.go", Line: 10, Severity: "high", Title: "A", Body: "a"},
			// Not part of the diff: posting this would make the forge reject
			// the entire review.
			{Path: "queue.go", Line: 11, Severity: "high", Title: "B", Body: "b"},
			// A file that was not touched at all.
			{Path: "other.go", Line: 10, Severity: "high", Title: "C", Body: "c"},
			// Too minor to report.
			{Path: "queue.go", Line: 20, Severity: "info", Title: "D", Body: "d"},
			// The same point again.
			{Path: "queue.go", Line: 10, Severity: "medium", Title: "a", Body: "duplicate"},
			{Path: "queue.go", Line: 30, Severity: "critical", Title: "E", Body: "e"},
		},
	}

	got := reviewer.Sanitize(out, positions, reviewer.Limits{MaxComments: 10, MinSeverity: reviewer.SeverityMedium})

	if len(got.Findings) != 2 {
		t.Fatalf("%d findings survived, want 2: %+v", len(got.Findings), got.Findings)
	}
	// The most serious finding is reported first.
	if got.Findings[0].Severity != reviewer.SeverityCritical {
		t.Errorf("first finding = %+v, want the critical one", got.Findings[0])
	}
	if got.OutOfDiff != 2 {
		t.Errorf("OutOfDiff = %d, want 2", got.OutOfDiff)
	}
	if got.BelowSeverity != 1 {
		t.Errorf("BelowSeverity = %d, want 1", got.BelowSeverity)
	}
	if got.Duplicate != 1 {
		t.Errorf("Duplicate = %d, want 1", got.Duplicate)
	}
	if got.Dropped() != 4 {
		t.Errorf("Dropped() = %d, want 4", got.Dropped())
	}
}

// When the cap bites, the serious findings are the ones that survive.
func TestSanitizeKeepsTheMostSerious(t *testing.T) {
	positions := positionsFor(map[string][]int{"queue.go": {1, 2, 3, 4}})

	out := &reviewer.Output{
		SchemaVersion: reviewer.OutputSchemaVersion,
		Summary:       "ok",
		Comments: []reviewer.OutputComment{
			{Path: "queue.go", Line: 1, Severity: "low", Title: "A", Body: "a"},
			{Path: "queue.go", Line: 2, Severity: "low", Title: "B", Body: "b"},
			{Path: "queue.go", Line: 3, Severity: "critical", Title: "C", Body: "c"},
			{Path: "queue.go", Line: 4, Severity: "medium", Title: "D", Body: "d"},
		},
	}

	got := reviewer.Sanitize(out, positions, reviewer.Limits{MaxComments: 2})

	if len(got.Findings) != 2 || got.Excess != 2 {
		t.Fatalf("findings = %+v, excess = %d", got.Findings, got.Excess)
	}
	if got.Findings[0].Severity != reviewer.SeverityCritical || got.Findings[1].Severity != reviewer.SeverityMedium {
		t.Errorf("kept %s and %s, want the two most serious",
			got.Findings[0].Severity, got.Findings[1].Severity)
	}
}

// A range whose end is outside the diff is anchored to its first line rather
// than thrown away.
func TestSanitizeNarrowsAnUnpostableRange(t *testing.T) {
	positions := positionsFor(map[string][]int{"queue.go": {10}})

	out := &reviewer.Output{
		SchemaVersion: reviewer.OutputSchemaVersion,
		Summary:       "ok",
		Comments: []reviewer.OutputComment{
			{Path: "queue.go", Line: 10, EndLine: 40, Severity: "high", Title: "A", Body: "a"},
		},
	}

	got := reviewer.Sanitize(out, positions, reviewer.Limits{MaxComments: 5})
	if len(got.Findings) != 1 {
		t.Fatalf("%d findings, want 1", len(got.Findings))
	}
	if got.Findings[0].EndLine != 0 {
		t.Errorf("EndLine = %d, want the comment narrowed to one line", got.Findings[0].EndLine)
	}
}

func TestSanitizeTruncatesLongBodies(t *testing.T) {
	positions := positionsFor(map[string][]int{"queue.go": {10}})

	out := &reviewer.Output{
		SchemaVersion: reviewer.OutputSchemaVersion,
		Summary:       "ok",
		Comments: []reviewer.OutputComment{
			{Path: "queue.go", Line: 10, Severity: "high", Title: "A", Body: strings.Repeat("x", 10000)},
		},
	}

	got := reviewer.Sanitize(out, positions, reviewer.Limits{MaxComments: 5, MaxBodyBytes: 100})
	if len(got.Findings[0].Body) > 100 {
		t.Errorf("body is %d bytes, want it truncated to 100", len(got.Findings[0].Body))
	}
}

func TestSeverityOrdering(t *testing.T) {
	if !reviewer.SeverityHigh.AtLeast(reviewer.SeverityMedium) {
		t.Error("high should clear a medium threshold")
	}
	if reviewer.SeverityLow.AtLeast(reviewer.SeverityMedium) {
		t.Error("low should not clear a medium threshold")
	}
	if reviewer.Severity("nonsense").Known() {
		t.Error("an unknown severity reported itself as known")
	}
	// No threshold means everything is reported.
	if !reviewer.SeverityInfo.AtLeast("") {
		t.Error("an empty threshold should accept everything")
	}
}
