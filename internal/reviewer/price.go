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
	// CacheRead and CacheWrite are what the provider charges for prompt
	// tokens it served from, or wrote into, its cache. Left unconfigured they
	// are the input rate, which is what a provider that does not discount
	// caching charges and an over-estimate for one that does.
	CacheRead  float64
	CacheWrite float64
}

// Prices maps a model id to its price. The key [PriceFallback] applies to any
// model without an entry of its own.
type Prices map[string]Price

// ParsePrices reads the configured form: one entry per model, as
//
//	model=input/output[/cache_read[/cache_write]]
//
// where the numbers are per million tokens. The cache rates are optional and
// default to the input rate.
//
//	google-vertex/gemini-3.1-pro-preview=1.25/10/0.31
//	anthropic/claude-sonnet-4-5=3/15/0.3/3.75
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

		price, err := parsePrice(strings.Split(rate, "/"))
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

func parsePrice(rates []string) (Price, error) {
	if len(rates) < 2 || len(rates) > 4 {
		return Price{}, fmt.Errorf("rates are input/output[/cache_read[/cache_write]]")
	}

	names := []string{"input", "output", "cache read", "cache write"}
	parsed := make([]float64, len(rates))
	for i, rate := range rates {
		value, err := strconv.ParseFloat(strings.TrimSpace(rate), 64)
		if err != nil {
			return Price{}, fmt.Errorf("%s rate %q is not a number", names[i], rate)
		}
		if value < 0 {
			return Price{}, fmt.Errorf("%s rate must not be negative", names[i])
		}
		parsed[i] = value
	}

	// An unconfigured cache rate is the input rate rather than free: guessing
	// a discount nobody quoted would under-report the bill.
	price := Price{Input: parsed[0], Output: parsed[1]}
	price.CacheRead, price.CacheWrite = price.Input, price.Input
	if len(parsed) > 2 {
		price.CacheRead = parsed[2]
	}
	if len(parsed) > 3 {
		price.CacheWrite = parsed[3]
	}
	return price, nil
}

// Cost estimates what a run cost. It reports false when the model has no
// price, which is the difference between "this was cheap" and "nobody said".
//
// Reasoning tokens are billed at the output rate: providers charge for them
// as output even though they never appear in it.
//
// It stays an estimate. The rates are whatever was configured, and a run that
// hit a rate limit or a discount nobody wrote down is not billed by this.
func (p Prices) Cost(model string, usage Usage) (float64, bool) {
	price, ok := p.lookup(model)
	if !ok {
		return 0, false
	}

	const perMillion = 1_000_000.0
	total := float64(usage.InputTokens)*price.Input +
		float64(usage.CacheReadTokens)*price.CacheRead +
		float64(usage.CacheWriteTokens)*price.CacheWrite +
		float64(usage.OutputTokens+usage.ReasoningTokens)*price.Output
	return total / perMillion, true
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
