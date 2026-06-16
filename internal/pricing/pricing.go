// Package pricing maps Claude model IDs to per-million-token USD
// rates and computes cost for a given token mix.
//
// Rates are sourced from a VENDORED, pinned release of the public
// ai-price-index dataset (github.com/RoninForge/ai-price-index). The
// raw artifacts live under internal/pricing/index/** (committed for
// audit), and internal/pricing/gen/main.go codegens table_gen.go from
// them at build time via `go generate`. There is NO runtime network
// access: the price table is embedded in the binary as generated Go.
//
// The dataset records POINT-IN-TIME pricing: each model carries an
// input and an output series of half-open [from, to) intervals, so a
// cost can be computed as of the instant an event occurred (RatesForAt
// / CostForModelAt) rather than only at "now". The "now" wrappers
// (RatesFor / CostForModel) preserve the original behavior and
// signatures so existing callers compile unchanged.
//
// To update prices: re-pin internal/pricing/PINNED_TAG to a newer
// dataset tag, run `go run ./internal/pricing/gen -fetch` to refresh
// the vendored artifacts + PROVENANCE.json, then `go generate
// ./internal/pricing/` to regenerate table_gen.go. The arithmetic and
// parity tests guard against silent drift.
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

//go:generate go run ./gen/main.go

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
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

// ErrUnknownModel is returned when a lookup sees a model identifier not
// in the pricing table (and not resolvable via an alias). Callers should
// log and skip the event rather than halting the watcher.
var ErrUnknownModel = errors.New("unknown model")

// ErrNoRateAtTime is returned when the model is known but no price
// interval covers the requested instant (for example, a date before the
// model's earliest recorded price).
var ErrNoRateAtTime = errors.New("no rate effective at time")

// Cache multipliers applied to the model's input rate to derive
// cache rates. Centralized so we only get them wrong in one place.
const (
	cacheReadMultiplier    = 0.10
	cacheWrite5mMultiplier = 1.25
	cacheWrite1hMultiplier = 2.00
)

// priceInterval is one half-open [from, to) price window for a single
// variation (input or output). to == nil means the interval is open
// (still current). Generated into table_gen.go.
type priceInterval struct {
	from     time.Time  // inclusive lower bound, UTC midnight
	to       *time.Time // exclusive upper bound, UTC midnight; nil = open
	priceUSD float64
}

// modelHist is the full point-in-time price history for one canonical
// model: separate input and output interval series. Generated into
// table_gen.go.
type modelHist struct {
	input  []priceInterval
	output []priceInterval
}

// ptrTime is a tiny helper used by the generated table to take the
// address of a time.Date literal for closed-interval upper bounds.
func ptrTime(t time.Time) *time.Time { return &t }

// canonicalModel resolves a raw model id to a canonical id in
// modelSeries. It tolerates a trailing "[1m]" display suffix (the 1M
// context variant is billed at standard rates) and then applies any
// alias mapping. It returns the canonical id and whether it is known.
func canonicalModel(model string) (string, bool) {
	m := strings.TrimSuffix(model, "[1m]")
	if _, ok := modelSeries[m]; ok {
		return m, true
	}
	if canon, ok := modelAliases[m]; ok {
		if _, ok := modelSeries[canon]; ok {
			return canon, true
		}
	}
	return "", false
}

// priceAt returns the price effective at instant t from a [from, to)
// interval series. The comparison is half-open and lower-bound
// inclusive: !t.Before(from) && (to == nil || t.Before(*to)).
func priceAt(series []priceInterval, t time.Time) (float64, bool) {
	for _, iv := range series {
		if !t.Before(iv.from) && (iv.to == nil || t.Before(*iv.to)) {
			return iv.priceUSD, true
		}
	}
	return 0, false
}

// RatesForAt returns the pricing rates for a model as of instant at,
// including derived cache rates. The model is resolved through the alias
// map (and tolerates a trailing "[1m]" suffix). It returns
// ErrUnknownModel if the model is not known at all, or ErrNoRateAtTime
// if the model is known but no interval covers at.
func RatesForAt(model string, at time.Time) (Rates, error) {
	canon, ok := canonicalModel(model)
	if !ok {
		return Rates{}, fmt.Errorf("%w: %q", ErrUnknownModel, model)
	}
	hist := modelSeries[canon]
	t := at.UTC()

	in, ok := priceAt(hist.input, t)
	if !ok {
		return Rates{}, fmt.Errorf("%w: %q at %s", ErrNoRateAtTime, canon, t.Format("2006-01-02"))
	}
	out, ok := priceAt(hist.output, t)
	if !ok {
		return Rates{}, fmt.Errorf("%w: %q at %s", ErrNoRateAtTime, canon, t.Format("2006-01-02"))
	}

	return Rates{
		InputPerMTok:        in,
		OutputPerMTok:       out,
		CacheReadPerMTok:    in * cacheReadMultiplier,
		CacheWrite5mPerMTok: in * cacheWrite5mMultiplier,
		CacheWrite1hPerMTok: in * cacheWrite1hMultiplier,
	}, nil
}

// RatesFor returns the current pricing rates for a model (rates as of
// now). Unknown models produce ErrUnknownModel. This is a thin "now"
// wrapper over RatesForAt that preserves the original signature.
func RatesFor(model string) (Rates, error) {
	return RatesForAt(model, time.Now().UTC())
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

// CostForModelAt is the point-in-time one-shot: resolve the model's
// rates as of at, then price the usage. Returns ErrUnknownModel for an
// unknown model or ErrNoRateAtTime if no interval covers at.
func CostForModelAt(model string, at time.Time, u Usage) (float64, error) {
	r, err := RatesForAt(model, at)
	if err != nil {
		return 0, err
	}
	return Cost(r, u), nil
}

// CostForModel is the convenience one-shot at current rates: look up the
// model, then price the usage. Returns ErrUnknownModel if the model is
// unknown. Thin "now" wrapper over CostForModelAt; signature preserved.
func CostForModel(model string, u Usage) (float64, error) {
	return CostForModelAt(model, time.Now().UTC(), u)
}

// Interval is one exported half-open [From, To) price window for a
// single model, derived input+output rates included. To is nil when
// the interval is still current (open). Returned by History for
// rendering the full point-in-time table to humans.
type Interval struct {
	From  time.Time  // inclusive lower bound, UTC midnight
	To    *time.Time // exclusive upper bound, UTC midnight; nil = open
	Rates Rates      // input/output/cache rates effective in [From, To)
}

// History returns the full point-in-time interval series for a model,
// each interval carrying the rates effective in its window. The model
// is resolved through the alias map (and tolerates a trailing "[1m]"
// suffix). Returns ErrUnknownModel if the model is not known.
//
// The series is built from the union of the input and output series
// boundary dates so a price change on either variation produces a new
// interval. The rates for each interval are resolved at its From
// instant, which is exactly how cost would be computed for an event at
// that time.
func History(model string) ([]Interval, error) {
	canon, ok := canonicalModel(model)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownModel, model)
	}
	hist := modelSeries[canon]

	// Collect every distinct interval boundary across both series.
	bounds := make(map[time.Time]struct{})
	for _, iv := range hist.input {
		bounds[iv.from] = struct{}{}
		if iv.to != nil {
			bounds[*iv.to] = struct{}{}
		}
	}
	for _, iv := range hist.output {
		bounds[iv.from] = struct{}{}
		if iv.to != nil {
			bounds[*iv.to] = struct{}{}
		}
	}
	starts := make([]time.Time, 0, len(bounds))
	for t := range bounds {
		starts = append(starts, t)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })

	out := make([]Interval, 0, len(starts))
	for _, from := range starts {
		in, okIn := priceAt(hist.input, from)
		o, okOut := priceAt(hist.output, from)
		if !okIn || !okOut {
			// A boundary that is only an upper bound (the close of the
			// last interval, with no successor) has no rate; skip it.
			continue
		}
		iv := Interval{
			From: from,
			Rates: Rates{
				InputPerMTok:        in,
				OutputPerMTok:       o,
				CacheReadPerMTok:    in * cacheReadMultiplier,
				CacheWrite5mPerMTok: in * cacheWrite5mMultiplier,
				CacheWrite1hPerMTok: in * cacheWrite1hMultiplier,
			},
		}
		out = append(out, iv)
	}
	// Fill each interval's To from the next interval's From; the last
	// interval is open (To == nil) only if the underlying series is
	// open at that point, otherwise it closes at the series upper bound.
	for i := range out {
		if i+1 < len(out) {
			next := out[i+1].From
			out[i].To = &next
			continue
		}
		// Last interval: open iff a covering series is still open at
		// From, otherwise closed at the earliest series upper bound.
		out[i].To = lastUpperBound(hist, out[i].From)
	}
	return out, nil
}

// lastUpperBound returns the exclusive upper bound for the final
// interval starting at from: nil if any covering series is still open
// there, otherwise the earliest series close among the covering
// intervals.
func lastUpperBound(h modelHist, from time.Time) *time.Time {
	var bound *time.Time
	// covering returns the interval that covers from, if any.
	covering := func(series []priceInterval) (priceInterval, bool) {
		for _, iv := range series {
			if !from.Before(iv.from) && (iv.to == nil || from.Before(*iv.to)) {
				return iv, true
			}
		}
		return priceInterval{}, false
	}
	for _, series := range [][]priceInterval{h.input, h.output} {
		iv, ok := covering(series)
		if !ok {
			continue
		}
		if iv.to == nil {
			return nil // an open series keeps the interval open
		}
		if bound == nil || iv.to.Before(*bound) {
			t := *iv.to
			bound = &t
		}
	}
	return bound
}

// Provenance returns the pinned dataset tag and the ai-price-index repo
// commit the embedded pricing table was generated from. Surfaced by
// `budgetclaw pricing provenance` so an auditor can trace every rate to
// an exact upstream commit.
func Provenance() (tag, commit string) {
	return generatedTag, generatedIndexCommit
}

// KnownModels returns a sorted list of model IDs that price successfully:
// the union of canonical series ids and alias ids. Useful for
// `budgetclaw pricing list` and for fuzzing tests.
func KnownModels() []string {
	seen := make(map[string]bool, len(modelSeries)+len(modelAliases))
	for k := range modelSeries {
		seen[k] = true
	}
	for k := range modelAliases {
		seen[k] = true
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
