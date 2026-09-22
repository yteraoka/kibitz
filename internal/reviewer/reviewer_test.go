package reviewer_test

import (
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/reviewer"
)

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
