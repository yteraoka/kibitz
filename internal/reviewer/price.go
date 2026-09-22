package reviewer

import (
	"fmt"
	"strconv"
	"strings"
)

// PriceFallback is the model id that prices anything not named explicitly.
const PriceFallback = "*"

// Price is what a model charges, in units of currency per million tokens.
// kibitz does not know what anything costs — prices differ by provider,
// region and contract, and they change — so this is configured rather than
// carried around in the code. A figure nobody entered is a figure nobody
// should trust.
type Price struct {
	Input  float64
	Output float64
}

// Prices maps a model id to its price. The key [PriceFallback] applies to any
// model without an entry of its own.
type Prices map[string]Price

// ParsePrices reads the configured form: one "model=input/output" per entry,
// where the numbers are per million tokens.
//
//	google-vertex/gemini-3.1-pro-preview=1.25/10
//	*=2/8
func ParsePrices(entries []string) (Prices, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	prices := make(Prices, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		model, rate, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("price %q is not model=input/output", entry)
		}
		in, out, ok := strings.Cut(rate, "/")
		if !ok {
			return nil, fmt.Errorf("price for %q is not input/output", model)
		}

		price, err := parsePrice(in, out)
		if err != nil {
			return nil, fmt.Errorf("price for %q: %w", model, err)
		}
		prices[strings.TrimSpace(model)] = price
	}
	if len(prices) == 0 {
		return nil, nil
	}
	return prices, nil
}

func parsePrice(in, out string) (Price, error) {
	input, err := strconv.ParseFloat(strings.TrimSpace(in), 64)
	if err != nil {
		return Price{}, fmt.Errorf("input rate %q is not a number", in)
	}
	output, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
	if err != nil {
		return Price{}, fmt.Errorf("output rate %q is not a number", out)
	}
	if input < 0 || output < 0 {
		return Price{}, fmt.Errorf("rates must not be negative")
	}
	return Price{Input: input, Output: output}, nil
}

// Cost estimates what a run cost. It reports false when the model has no
// price, which is the difference between "this was cheap" and "nobody said".
//
// The estimate counts input and output only. A provider that discounts cached
// input charges less than this says, so the figure is a ceiling rather than a
// bill.
func (p Prices) Cost(model string, usage Usage) (float64, bool) {
	price, ok := p.lookup(model)
	if !ok {
		return 0, false
	}

	const perMillion = 1_000_000.0
	return float64(usage.InputTokens)/perMillion*price.Input +
		float64(usage.OutputTokens)/perMillion*price.Output, true
}

func (p Prices) lookup(model string) (Price, bool) {
	if p == nil {
		return Price{}, false
	}
	if price, ok := p[model]; ok {
		return price, true
	}
	price, ok := p[PriceFallback]
	return price, ok
}
