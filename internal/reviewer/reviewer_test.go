package reviewer_test

import (
	"strings"
	"testing"
	"time"

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

func TestParsePrices(t *testing.T) {
	prices, err := reviewer.ParsePrices([]string{
		"google-vertex/gemini-3.1-pro-preview=1.25/10",
		" *=2/8 ",
		"",
	})
	if err != nil {
		t.Fatalf("ParsePrices: %v", err)
	}

	usage := reviewer.Usage{InputTokens: 1_000_000, OutputTokens: 100_000}

	cost, ok := prices.Cost("google-vertex/gemini-3.1-pro-preview", usage)
	if !ok {
		t.Fatal("the named model has no price")
	}
	if want := 1.25 + 1.0; cost != want {
		t.Errorf("cost = %v, want %v", cost, want)
	}

	// Anything not named falls back.
	cost, ok = prices.Cost("something/else", usage)
	if !ok {
		t.Fatal("the fallback did not apply")
	}
	if want := 2.0 + 0.8; cost != want {
		t.Errorf("fallback cost = %v, want %v", cost, want)
	}
}

// Saying nothing is different from saying zero: a price nobody configured is
// not a review that was free.
func TestPricesWithoutAnEntry(t *testing.T) {
	prices, err := reviewer.ParsePrices([]string{"a/b=1/2"})
	if err != nil {
		t.Fatalf("ParsePrices: %v", err)
	}

	if _, ok := prices.Cost("c/d", reviewer.Usage{InputTokens: 1000}); ok {
		t.Error("an unpriced model reported a cost")
	}
	if _, ok := reviewer.Prices(nil).Cost("a/b", reviewer.Usage{InputTokens: 1000}); ok {
		t.Error("an unconfigured kibitz reported a cost")
	}
}

func TestParsePricesRejectsNonsense(t *testing.T) {
	for _, entry := range []string{"no-equals", "a/b=1", "a/b=x/2", "a/b=1/y", "a/b=-1/2"} {
		if _, err := reviewer.ParsePrices([]string{entry}); err == nil {
			t.Errorf("ParsePrices(%q) succeeded, want an error", entry)
		}
	}
}

func TestParsePricesEmpty(t *testing.T) {
	prices, err := reviewer.ParsePrices(nil)
	if err != nil {
		t.Fatalf("ParsePrices: %v", err)
	}
	if prices != nil {
		t.Errorf("prices = %v, want nil", prices)
	}
}

func TestUsageAdd(t *testing.T) {
	a := reviewer.Usage{InputTokens: 10, OutputTokens: 2, Duration: time.Second}
	b := reviewer.Usage{InputTokens: 5, OutputTokens: 3, Duration: 2 * time.Second}

	got := a.Add(b)
	if got.InputTokens != 15 || got.OutputTokens != 5 || got.Duration != 3*time.Second {
		t.Errorf("sum = %+v", got)
	}
	if got.Tokens() != 20 {
		t.Errorf("tokens = %d, want 20", got.Tokens())
	}
}
