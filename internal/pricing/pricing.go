// Package pricing maps Claude model IDs to per-million-token USD
// rates and computes cost for a given token mix.
//
// Rates are based on Anthropic's published pricing page. The table
// is deliberately small and easy to update: when Anthropic changes
// prices, edit baseRates, update docs/decisions.md with the date and
// reason, and adjust the arithmetic tests to match.
//
// Cache semantics follow Anthropic's published multipliers applied
// to the model's input rate:
//
//	cache read         = 0.1  × input rate
//	cache write 5m     = 1.25 × input rate
//	cache write 1h     = 2.0  × input rate
//
// The multipliers are centralized in one place (below) so a future
// Anthropic change only needs a single edit.
package pricing

import (
	"errors"
	"fmt"
	"sort"
)

// Rates is the per-million-token cost in USD for one model.
type Rates struct {
	InputPerMTok        float64
	OutputPerMTok       float64
	CacheReadPerMTok    float64
	CacheWrite5mPerMTok float64
	CacheWrite1hPerMTok float64
}

// Usage is the token counts we price. All fields are optional; zero
// counts contribute zero cost.
type Usage struct {
	Input        int
	Output       int
	CacheRead    int
	CacheWrite5m int
	CacheWrite1h int
}

// ErrUnknownModel is returned when RatesFor sees a model identifier
// not in the pricing table. Callers should log and skip the event
// rather than halting the watcher.
var ErrUnknownModel = errors.New("unknown model")

// Cache multipliers applied to the model's input rate to derive
// cache rates. Centralized so we only get them wrong in one place.
const (
	cacheReadMultiplier    = 0.10
	cacheWrite5mMultiplier = 1.25
	cacheWrite1hMultiplier = 2.00
)

// baseRates holds the raw input/output rates per million tokens, in
// USD. Cache rates are derived from the input rate using the
// multipliers above.
//
// Last updated: 2026-06-10 (added Claude Fable 5 at $10/$50, a new
// flagship tier above Opus, after the 2026-06-09 Fable 5 / Mythos 5
// launch. Before this, Fable 5 events were silently skipped as an
// unknown model. Re-verified every existing entry against the live
// Anthropic pricing page on the same day: no older model changed
// price, and the cache multipliers are unchanged. Mythos 5 ships at
// the same $10/$50 rate but is restricted-access (Project Glasswing
// cybersecurity + biology researchers), has no published API model
// ID, and never reaches Claude Code's JSONL, so it gets no entry.
// The prior 2026-05-29 update added Opus 4.8 at $5/$25.
// Source: platform.claude.com/docs/en/docs/about-claude/pricing.
// The 2026-04-28 (v0.1.4) update corrected Opus 4.5/4.6/4.7 rates
// after a LiteLLM cross-check + maintainer screenshot revealed
// Anthropic had moved them to a new lower tier.
//
// When adding a model:
//  1. Add an entry here.
//  2. Add a line to TestRatesForKnownModels in pricing_test.go that
//     asserts the expected input/output rates.
//  3. Bump the "last updated" date above.
var baseRates = map[string]struct {
	Input  float64
	Output float64
}{
	// Fable: new flagship tier above Opus, launched 2026-06-09 at
	// $10/$50. Claude Code emits the undated form "claude-fable-5"
	// in message.model (verified against live logs). The 1M context
	// variant (display ID "...[1m]") is billed at standard rates and
	// never reaches message.model, so it needs no separate entry.
	"claude-fable-5": {Input: 10.00, Output: 50.00},

	// Opus: highest-capability tier. Anthropic dropped Opus
	// pricing for 4.5+ to a new lower tier ($5/$25); 4.1 and
	// older remain at the original $15/$75. Both undated and
	// dated variants are listed because Claude Code emits both
	// forms in the wild. The 1M context variant (display ID
	// "...[1m]") is billed at standard rates and never reaches
	// message.model in the JSONL, so it needs no separate entry.
	"claude-opus-4-8":          {Input: 5.00, Output: 25.00},
	"claude-opus-4-7":          {Input: 5.00, Output: 25.00},
	"claude-opus-4-6":          {Input: 5.00, Output: 25.00},
	"claude-opus-4-5":          {Input: 5.00, Output: 25.00},
	"claude-opus-4-5-20251101": {Input: 5.00, Output: 25.00},
	"claude-opus-4-1-20250805": {Input: 15.00, Output: 75.00},

	// Sonnet: mid tier. Both undated and dated variants included
	// because Claude Code emits both forms in the wild.
	"claude-sonnet-4-6":          {Input: 3.00, Output: 15.00},
	"claude-sonnet-4-5":          {Input: 3.00, Output: 15.00},
	"claude-sonnet-4-5-20250929": {Input: 3.00, Output: 15.00},

	// Haiku: cheapest tier. Both undated and dated variants.
	"claude-haiku-4-5":          {Input: 1.00, Output: 5.00},
	"claude-haiku-4-5-20251001": {Input: 1.00, Output: 5.00},
}

// RatesFor returns the pricing rates for a model, including derived
// cache rates. Unknown models produce ErrUnknownModel.
func RatesFor(model string) (Rates, error) {
	br, ok := baseRates[model]
	if !ok {
		return Rates{}, fmt.Errorf("%w: %q", ErrUnknownModel, model)
	}
	return Rates{
		InputPerMTok:        br.Input,
		OutputPerMTok:       br.Output,
		CacheReadPerMTok:    br.Input * cacheReadMultiplier,
		CacheWrite5mPerMTok: br.Input * cacheWrite5mMultiplier,
		CacheWrite1hPerMTok: br.Input * cacheWrite1hMultiplier,
	}, nil
}

// Cost computes the total USD cost for a given (Rates, Usage) pair.
// Zero-cost calls are valid and return 0 without error.
func Cost(r Rates, u Usage) float64 {
	const perMillion = 1_000_000.0
	return float64(u.Input)*r.InputPerMTok/perMillion +
		float64(u.Output)*r.OutputPerMTok/perMillion +
		float64(u.CacheRead)*r.CacheReadPerMTok/perMillion +
		float64(u.CacheWrite5m)*r.CacheWrite5mPerMTok/perMillion +
		float64(u.CacheWrite1h)*r.CacheWrite1hPerMTok/perMillion
}

// CostForModel is the convenience one-shot: look up the model, then
// price the usage. Returns ErrUnknownModel if the model is unknown.
func CostForModel(model string, u Usage) (float64, error) {
	r, err := RatesFor(model)
	if err != nil {
		return 0, err
	}
	return Cost(r, u), nil
}

// KnownModels returns a sorted list of model IDs in the pricing
// table. Useful for `budgetclaw pricing list` and for fuzzing tests.
func KnownModels() []string {
	keys := make([]string, 0, len(baseRates))
	for k := range baseRates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
