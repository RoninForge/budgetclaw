package pricing

import (
	"errors"
	"math"
	"testing"
	"time"
)

// epsilon is the floating-point tolerance used for monetary
// comparisons. We're pricing token counts, not doing physics —
// 1e-9 USD is absurdly precise.
const epsilon = 1e-9

// TestRatesForKnownModels locks in the expected input/output rates
// for every model in the pricing table. If Anthropic changes prices
// and we update baseRates, these assertions will also need to move,
// which is the point: a silent rate drift breaks tests before it
// breaks users.
func TestRatesForKnownModels(t *testing.T) {
	cases := []struct {
		model      string
		wantInput  float64
		wantOutput float64
	}{
		{"claude-fable-5", 10.00, 50.00},
		{"claude-opus-4-8", 5.00, 25.00},
		{"claude-opus-4-7", 5.00, 25.00},
		{"claude-opus-4-6", 5.00, 25.00},
		{"claude-opus-4-5", 5.00, 25.00},
		{"claude-opus-4-5-20251101", 5.00, 25.00},
		{"claude-opus-4-1-20250805", 15.00, 75.00},
		{"claude-sonnet-4-6", 3.00, 15.00},
		{"claude-sonnet-4-5", 3.00, 15.00},
		{"claude-sonnet-4-5-20250929", 3.00, 15.00},
		{"claude-haiku-4-5", 1.00, 5.00},
		{"claude-haiku-4-5-20251001", 1.00, 5.00},
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			r, err := RatesFor(c.model)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r.InputPerMTok != c.wantInput {
				t.Errorf("input: got %v, want %v", r.InputPerMTok, c.wantInput)
			}
			if r.OutputPerMTok != c.wantOutput {
				t.Errorf("output: got %v, want %v", r.OutputPerMTok, c.wantOutput)
			}
		})
	}
}

// TestRatesForUnknownModel confirms unknown models produce a wrapped
// ErrUnknownModel checkable with errors.Is.
func TestRatesForUnknownModel(t *testing.T) {
	_, err := RatesFor("gpt-4")
	if !errors.Is(err, ErrUnknownModel) {
		t.Errorf("expected ErrUnknownModel, got %v", err)
	}
}

// TestCacheMultipliersDerived verifies the cache WRITE rates are
// derived from the input rate via the published Anthropic multipliers,
// for every tier, so a refactor of the multiplier constants can't
// break one tier while leaving others passing.
//
// Cache READ is deliberately not asserted here: it is no longer a fixed
// derivation. The dataset's recorded cache_read wins when present and
// 0.1x is only the fallback, because not every model uses that
// multiplier (Claude Fable 5.1 and Mythos 5.1 are priced at 0.025x).
// Engine-vs-dataset agreement on cache_read is covered by
// TestParityWithCurrentJSON; the precedence rule by TestCacheReadPrefersRecordedRate.
//
// KnownModels now includes historical models the vendored dataset
// retired (their newest interval is closed before "now"). Those are
// not priceable at the current instant, so we skip them here with
// ErrNoRateAtTime; the multiplier invariant is still exercised on every
// currently-priced model. Point-in-time coverage of the historical
// models lives in dataset_test.go.
func TestCacheMultipliersDerived(t *testing.T) {
	for _, model := range KnownModels() {
		t.Run(model, func(t *testing.T) {
			r, err := RatesFor(model)
			if errors.Is(err, ErrNoRateAtTime) {
				t.Skipf("model %q has no current rate (retired); covered point-in-time elsewhere", model)
			}
			if err != nil {
				t.Fatal(err)
			}
			wantWrite5m := r.InputPerMTok * 1.25
			wantWrite1h := r.InputPerMTok * 2.00
			if math.Abs(r.CacheWrite5mPerMTok-wantWrite5m) > epsilon {
				t.Errorf("cache_write_5m: got %v, want %v", r.CacheWrite5mPerMTok, wantWrite5m)
			}
			if math.Abs(r.CacheWrite1hPerMTok-wantWrite1h) > epsilon {
				t.Errorf("cache_write_1h: got %v, want %v", r.CacheWrite1hPerMTok, wantWrite1h)
			}
		})
	}
}

// TestCostArithmetic locks in cost calculation at known-round values
// so rounding changes or unit-conversion bugs are caught immediately.
// Values picked so the math is easy to verify by hand. We anchor on
// claude-opus-4-1-20250805 because it remained on the original
// $15/$75 tier when Opus 4.5+ moved to $5/$25 — using a stable
// price point keeps the arithmetic predictable across rate changes.
func TestCostArithmetic(t *testing.T) {
	r, err := RatesFor("claude-opus-4-1-20250805")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		usage Usage
		want  float64
	}{
		{
			// Input only: 1M tokens × $15/M = $15.00
			name:  "1M input",
			usage: Usage{Input: 1_000_000},
			want:  15.00,
		},
		{
			// Output only: 1M tokens × $75/M = $75.00
			name:  "1M output",
			usage: Usage{Output: 1_000_000},
			want:  75.00,
		},
		{
			// 1M in + 1M out: $15 + $75 = $90.00
			name:  "1M in + 1M out",
			usage: Usage{Input: 1_000_000, Output: 1_000_000},
			want:  90.00,
		},
		{
			// Cache read: 0.1 × $15 = $1.50/M
			name:  "1M cache read",
			usage: Usage{CacheRead: 1_000_000},
			want:  1.50,
		},
		{
			// Cache write 5m: 1.25 × $15 = $18.75/M
			name:  "1M cache write 5m",
			usage: Usage{CacheWrite5m: 1_000_000},
			want:  18.75,
		},
		{
			// Cache write 1h: 2.0 × $15 = $30.00/M
			name:  "1M cache write 1h",
			usage: Usage{CacheWrite1h: 1_000_000},
			want:  30.00,
		},
		{
			// Zero usage is a valid no-op.
			name:  "zero usage",
			usage: Usage{},
			want:  0.00,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Cost(r, c.usage)
			if math.Abs(got-c.want) > epsilon {
				t.Errorf("got $%v, want $%v", got, c.want)
			}
		})
	}
}

// TestCostRealisticFixtureEvent reproduces the cost of the first
// billable event in the parser fixture (opus, 100 input, 4000 cache
// 5m write, 1000 cache 1h write, 50 output) by hand, then asserts
// CostForModel matches. This is the end-to-end smoke test for the
// pricing pipeline.
func TestCostRealisticFixtureEvent(t *testing.T) {
	// By hand:
	//   input:         100 / 1e6 * 15.00  = 0.00150
	//   output:         50 / 1e6 * 75.00  = 0.00375
	//   cache_5m:     4000 / 1e6 * 18.75  = 0.07500
	//   cache_1h:     1000 / 1e6 * 30.00  = 0.03000
	//   total:                             0.11025 USD
	want := 0.11025
	got, err := CostForModel("claude-opus-4-1-20250805", Usage{
		Input:        100,
		Output:       50,
		CacheWrite5m: 4000,
		CacheWrite1h: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-want) > epsilon {
		t.Errorf("got $%.5f, want $%.5f", got, want)
	}
}

// TestCostForUnknownModel confirms CostForModel propagates the
// unknown-model error rather than silently returning $0.
func TestCostForUnknownModel(t *testing.T) {
	_, err := CostForModel("gpt-4", Usage{Input: 100})
	if !errors.Is(err, ErrUnknownModel) {
		t.Errorf("expected ErrUnknownModel, got %v", err)
	}
}

// TestKnownModelsSorted verifies the helper returns sorted, non-empty
// output that includes the flagship opus model.
func TestKnownModelsSorted(t *testing.T) {
	got := KnownModels()
	if len(got) == 0 {
		t.Fatal("KnownModels returned empty slice")
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("KnownModels not sorted: %v", got)
			break
		}
	}
	found := false
	for _, m := range got {
		if m == "claude-opus-4-7" {
			found = true
			break
		}
	}
	if !found {
		t.Error("claude-opus-4-7 missing from KnownModels")
	}
}

// TestAllKnownModelsPriceable is a cross-package sanity check: every
// currently-priced model in the pricing table must be priceable with
// zero-usage and non-zero usage without error. Guards against table
// entries that were added without being wired through RatesFor
// correctly. Historical (retired) models in the vendored dataset have
// no rate at "now" (ErrNoRateAtTime) and are skipped; they are covered
// point-in-time in dataset_test.go.
func TestAllKnownModelsPriceable(t *testing.T) {
	for _, model := range KnownModels() {
		t.Run(model, func(t *testing.T) {
			// Zero usage should be $0 with no error.
			c, err := CostForModel(model, Usage{})
			if errors.Is(err, ErrNoRateAtTime) {
				t.Skipf("model %q has no current rate (retired); covered point-in-time elsewhere", model)
			}
			if err != nil {
				t.Fatalf("zero usage: %v", err)
			}
			if c != 0 {
				t.Errorf("zero usage: got $%v, want $0", c)
			}

			// Non-zero usage should produce a positive cost.
			c, err = CostForModel(model, Usage{Input: 1000, Output: 500})
			if err != nil {
				t.Fatalf("non-zero usage: %v", err)
			}
			if c <= 0 {
				t.Errorf("non-zero usage: got $%v, want > 0", c)
			}
		})
	}
}

// TestCacheReadPrefersRecordedRate pins the precedence rule that the
// 0.1x multiplier is a FALLBACK, not the definition. A model whose
// dataset rate is not 0.1x of input (Claude Fable 5.1 and Mythos 5.1
// are priced at 0.025x) must price at the recorded rate. This test owns
// its input, so it holds regardless of which models the vendored
// dataset happens to carry.
func TestCacheReadPrefersRecordedRate(t *testing.T) {
	at := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	input := []priceInterval{{from: from, to: nil, priceUSD: 10}}

	recorded := modelHist{
		input:     input,
		output:    []priceInterval{{from: from, to: nil, priceUSD: 50}},
		cacheRead: []priceInterval{{from: from, to: nil, priceUSD: 0.25}},
	}
	if got := cacheReadAt(recorded, at, 10); math.Abs(got-0.25) > epsilon {
		t.Errorf("recorded cache_read: got %v, want 0.25 (must not derive 0.1x = 1.0)", got)
	}

	derived := modelHist{input: input, output: recorded.output}
	if got := cacheReadAt(derived, at, 10); math.Abs(got-1.0) > epsilon {
		t.Errorf("fallback cache_read: got %v, want 1.0 (0.1 x 10)", got)
	}

	// Before the recorded interval opens there is no rate to prefer, so
	// the multiplier fallback still applies.
	before := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	if got := cacheReadAt(recorded, before, 10); math.Abs(got-1.0) > epsilon {
		t.Errorf("pre-interval cache_read: got %v, want 1.0 (0.1 x 10)", got)
	}
}
