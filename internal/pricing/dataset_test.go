package pricing

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// asOf is the reference "current" instant used by the dataset parity
// tests. It is derived from the embedded table rather than hardcoded: it
// is the latest `from` across every OPEN (to == nil) interval in
// modelSeries, i.e. the newest currently-effective price start date.
//
// Why this is correct and self-contained:
//   - current.json lists exactly the models with an open interval, so
//     each one is effective on [from, +inf). The max open `from` is on or
//     after every open interval's `from` and strictly before every open
//     interval's `to` (there is none), so RatesForAt(currentModel, asOf)
//     resolves for ALL of them.
//   - It is deterministic (no time.Now()): it depends only on the vendored
//     data baked into table_gen.go, so a re-vendor that adds a genuinely
//     new model (e.g. claude-mythos-5, effective after the previous max)
//     advances asOf automatically and parity never goes stale.
//   - It needs no codegen change: the test reads modelSeries directly
//     (this is an in-package test).
var asOf = latestOpenIntervalDate()

// latestOpenIntervalDate returns the maximum `from` among all open
// (still-current) intervals across every model's input and output series.
// It panics if no open interval exists, which would mean the embedded
// table has no currently-priced model and parity has nothing to assert.
func latestOpenIntervalDate() time.Time {
	var max time.Time
	found := false
	for _, hist := range modelSeries {
		for _, series := range [][]priceInterval{hist.input, hist.output} {
			for _, iv := range series {
				if iv.to != nil {
					continue // closed (retired) interval, not "current"
				}
				if !found || iv.from.After(max) {
					max = iv.from
					found = true
				}
			}
		}
	}
	if !found {
		panic("pricing: no open price interval in embedded table; cannot derive parity asOf")
	}
	return max.UTC()
}

func mustParseDate(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse date %q: %v", s, err)
	}
	return ts.UTC()
}

// ---------------------------------------------------------------------------
// Parity with the vendored current.json oracle
// ---------------------------------------------------------------------------

type currentDoc struct {
	Prices []struct {
		Provider  string  `json:"provider"`
		Model     string  `json:"model"`
		Variation string  `json:"variation"`
		PriceUSD  float64 `json:"price_usd"`
	} `json:"prices"`
}

// TestParityWithCurrentJSON asserts that the generated point-in-time
// table, evaluated as of the dataset's current date, reproduces the flat
// current.json oracle exactly for every anthropic row.
func TestParityWithCurrentJSON(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("index", "current.json"))
	if err != nil {
		t.Fatalf("read current.json: %v", err)
	}
	var doc currentDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse current.json: %v", err)
	}

	checked := 0
	for _, p := range doc.Prices {
		if p.Provider != "anthropic" {
			continue
		}
		r, err := RatesForAt(p.Model, asOf)
		if err != nil {
			t.Errorf("%s: RatesForAt error: %v", p.Model, err)
			continue
		}
		var got float64
		switch p.Variation {
		case "input":
			got = r.InputPerMTok
		case "output":
			got = r.OutputPerMTok
		case "cache_read":
			// Some models publish explicit cache rows. BudgetClaw derives
			// cache rates from the input rate via fixed multipliers; assert
			// the derivation reproduces the dataset's published value.
			got = r.CacheReadPerMTok
		case "cache_write_5m":
			got = r.CacheWrite5mPerMTok
		case "cache_write_1h":
			got = r.CacheWrite1hPerMTok
		default:
			t.Errorf("%s: unexpected variation %q", p.Model, p.Variation)
			continue
		}
		if math.Abs(got-p.PriceUSD) > epsilon {
			t.Errorf("%s %s: got %v, want %v", p.Model, p.Variation, got, p.PriceUSD)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no anthropic rows checked - current.json oracle empty?")
	}
}

// TestParityWithLegacyRates hardcodes the OLD hand-maintained baseRates
// values and asserts the generated current rates match them exactly.
// This proves the vendored-dataset migration is a zero-regression change.
func TestParityWithLegacyRates(t *testing.T) {
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
			r, err := RatesForAt(c.model, asOf)
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

// ---------------------------------------------------------------------------
// Point-in-time resolution
// ---------------------------------------------------------------------------

// TestRatesForAt_PointInTime exercises a model with multi-interval
// history: claude-3-5-haiku-20241022 stepped from $1/$5 to $0.80/$4 on
// 2024-12-03 (lower-bound inclusive). We assert before, on, and after
// the boundary.
func TestRatesForAt_PointInTime(t *testing.T) {
	const model = "claude-3-5-haiku-20241022"
	cases := []struct {
		name       string
		at         string
		wantInput  float64
		wantOutput float64
	}{
		{"before boundary", "2024-11-20", 1.0, 5.0},
		{"on boundary (inclusive lower bound)", "2024-12-03", 0.8, 4.0},
		{"after boundary", "2024-12-15", 0.8, 4.0},
		{"day before boundary still old tier", "2024-12-02", 1.0, 5.0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := RatesForAt(model, mustParseDate(t, c.at))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r.InputPerMTok != c.wantInput {
				t.Errorf("input @ %s: got %v, want %v", c.at, r.InputPerMTok, c.wantInput)
			}
			if r.OutputPerMTok != c.wantOutput {
				t.Errorf("output @ %s: got %v, want %v", c.at, r.OutputPerMTok, c.wantOutput)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Alias resolution
// ---------------------------------------------------------------------------

// TestAliasResolution verifies short alias forms resolve to the same
// rates as their dated canonical id, and that KnownModels includes them.
func TestAliasResolution(t *testing.T) {
	pairs := []struct {
		alias, canonical string
	}{
		{"claude-opus-4-5", "claude-opus-4-5-20251101"},
		{"claude-sonnet-4-5", "claude-sonnet-4-5-20250929"},
		{"claude-haiku-4-5", "claude-haiku-4-5-20251001"},
		{"claude-opus-4-1", "claude-opus-4-1-20250805"},
	}
	for _, p := range pairs {
		t.Run(p.alias, func(t *testing.T) {
			ra, err := RatesForAt(p.alias, asOf)
			if err != nil {
				t.Fatalf("alias %s: %v", p.alias, err)
			}
			rc, err := RatesForAt(p.canonical, asOf)
			if err != nil {
				t.Fatalf("canonical %s: %v", p.canonical, err)
			}
			if ra != rc {
				t.Errorf("alias %s rates %+v != canonical %s rates %+v", p.alias, ra, p.canonical, rc)
			}
		})
	}

	known := make(map[string]bool)
	for _, m := range KnownModels() {
		known[m] = true
	}
	for _, p := range pairs {
		if !known[p.alias] {
			t.Errorf("KnownModels missing alias %q", p.alias)
		}
		if !known[p.canonical] {
			t.Errorf("KnownModels missing canonical %q", p.canonical)
		}
	}
}

// ---------------------------------------------------------------------------
// Error paths
// ---------------------------------------------------------------------------

// TestRatesForAt_Unknown asserts an unrecognised model yields
// ErrUnknownModel.
func TestRatesForAt_Unknown(t *testing.T) {
	_, err := RatesForAt("totally-not-a-model", asOf)
	if !errors.Is(err, ErrUnknownModel) {
		t.Errorf("expected ErrUnknownModel, got %v", err)
	}
}

// TestRatesForAt_NoRate asserts that a known model queried before its
// earliest recorded interval yields ErrNoRateAtTime (not ErrUnknownModel
// and not a silent zero).
func TestRatesForAt_NoRate(t *testing.T) {
	// claude-3-5-haiku-20241022's earliest interval starts 2024-11-04.
	_, err := RatesForAt("claude-3-5-haiku-20241022", mustParseDate(t, "2024-01-01"))
	if !errors.Is(err, ErrNoRateAtTime) {
		t.Errorf("expected ErrNoRateAtTime, got %v", err)
	}
	if errors.Is(err, ErrUnknownModel) {
		t.Errorf("a known model should not report ErrUnknownModel: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Cross-engine golden vectors
// ---------------------------------------------------------------------------

type goldenFixture struct {
	Vectors []struct {
		Name  string `json:"name"`
		Model string `json:"model"`
		At    string `json:"at"`
		Usage struct {
			Input        int `json:"input"`
			Output       int `json:"output"`
			CacheRead    int `json:"cache_read"`
			CacheWrite5m int `json:"cache_write_5m"`
			CacheWrite1h int `json:"cache_write_1h"`
		} `json:"usage"`
		ExpectedUSD *float64 `json:"expected_usd"`
		Expected    string   `json:"expected"`
	} `json:"vectors"`
}

// ---------------------------------------------------------------------------
// History + Provenance accessors
// ---------------------------------------------------------------------------

// TestHistoryMultiInterval verifies History reconstructs the full
// interval table for a model with a price change. claude-3-5-haiku
// stepped $1/$5 -> $0.80/$4 on 2024-12-03 and then retired (closed) on
// 2026-02-19, so we expect exactly two intervals with the right rates
// and chained [from, to) bounds.
func TestHistoryMultiInterval(t *testing.T) {
	ivs, err := History("claude-3-5-haiku-20241022")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(ivs) != 2 {
		t.Fatalf("got %d intervals, want 2: %+v", len(ivs), ivs)
	}

	// First interval: 2024-11-04 .. 2024-12-03 at $1/$5.
	if !ivs[0].From.Equal(mustParseDate(t, "2024-11-04")) {
		t.Errorf("interval[0].From = %v, want 2024-11-04", ivs[0].From)
	}
	if ivs[0].To == nil || !ivs[0].To.Equal(mustParseDate(t, "2024-12-03")) {
		t.Errorf("interval[0].To = %v, want 2024-12-03", ivs[0].To)
	}
	if ivs[0].Rates.InputPerMTok != 1.0 || ivs[0].Rates.OutputPerMTok != 5.0 {
		t.Errorf("interval[0] rates = %v/%v, want 1.0/5.0", ivs[0].Rates.InputPerMTok, ivs[0].Rates.OutputPerMTok)
	}

	// Second interval: 2024-12-03 .. 2026-02-19 at $0.80/$4 (then retired).
	if !ivs[1].From.Equal(mustParseDate(t, "2024-12-03")) {
		t.Errorf("interval[1].From = %v, want 2024-12-03", ivs[1].From)
	}
	if ivs[1].To == nil || !ivs[1].To.Equal(mustParseDate(t, "2026-02-19")) {
		t.Errorf("interval[1].To = %v, want 2026-02-19 (retired)", ivs[1].To)
	}
	if ivs[1].Rates.InputPerMTok != 0.8 || ivs[1].Rates.OutputPerMTok != 4.0 {
		t.Errorf("interval[1] rates = %v/%v, want 0.8/4.0", ivs[1].Rates.InputPerMTok, ivs[1].Rates.OutputPerMTok)
	}
}

// TestHistoryOpenInterval verifies that a currently-priced model with a
// single open interval yields one interval whose To is nil. Cache rates
// are derived from the input rate.
func TestHistoryOpenInterval(t *testing.T) {
	ivs, err := History("claude-opus-4-1-20250805")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(ivs) != 1 {
		t.Fatalf("got %d intervals, want 1: %+v", len(ivs), ivs)
	}
	if ivs[0].To != nil {
		t.Errorf("open interval To = %v, want nil", ivs[0].To)
	}
	if ivs[0].Rates.InputPerMTok != 15.0 || ivs[0].Rates.OutputPerMTok != 75.0 {
		t.Errorf("rates = %v/%v, want 15.0/75.0", ivs[0].Rates.InputPerMTok, ivs[0].Rates.OutputPerMTok)
	}
	if ivs[0].Rates.CacheReadPerMTok != 1.5 {
		t.Errorf("cache read = %v, want 1.5 (0.1 x 15)", ivs[0].Rates.CacheReadPerMTok)
	}
}

// TestHistoryAlias verifies an alias resolves to the same history as
// the canonical id.
func TestHistoryAlias(t *testing.T) {
	a, err := History("claude-opus-4-1")
	if err != nil {
		t.Fatalf("History(alias): %v", err)
	}
	c, err := History("claude-opus-4-1-20250805")
	if err != nil {
		t.Fatalf("History(canonical): %v", err)
	}
	if len(a) != len(c) {
		t.Fatalf("alias len %d != canonical len %d", len(a), len(c))
	}
	for i := range a {
		if a[i].Rates != c[i].Rates || !a[i].From.Equal(c[i].From) {
			t.Errorf("interval %d mismatch: alias %+v vs canonical %+v", i, a[i], c[i])
		}
	}
}

// TestHistoryUnknown verifies History reports ErrUnknownModel for an
// unknown id.
func TestHistoryUnknown(t *testing.T) {
	_, err := History("not-a-real-model")
	if !errors.Is(err, ErrUnknownModel) {
		t.Errorf("expected ErrUnknownModel, got %v", err)
	}
}

// TestProvenance verifies the accessor returns the generated tag and
// commit (matching the vendored PINNED_TAG).
func TestProvenance(t *testing.T) {
	tag, commit := Provenance()
	if tag == "" || commit == "" {
		t.Fatalf("Provenance returned empty: tag=%q commit=%q", tag, commit)
	}
	want, err := os.ReadFile("PINNED_TAG")
	if err != nil {
		t.Fatalf("read PINNED_TAG: %v", err)
	}
	if got := string(want); len(got) > 0 {
		// PINNED_TAG has a trailing newline; trim for comparison.
		trimmed := got
		for len(trimmed) > 0 && (trimmed[len(trimmed)-1] == '\n' || trimmed[len(trimmed)-1] == ' ') {
			trimmed = trimmed[:len(trimmed)-1]
		}
		if tag != trimmed {
			t.Errorf("Provenance tag = %q, want PINNED_TAG %q", tag, trimmed)
		}
	}
}

// TestGoldenVectors runs the shared cross-engine golden fixture. Every
// engine (this Go package, the landing TS, Goei) must reproduce
// expected_usd exactly, and surface the unknown-model error rather than
// a silent zero.
func TestGoldenVectors(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("index", "pricing-vectors.json"))
	if err != nil {
		t.Fatalf("read pricing-vectors.json: %v", err)
	}
	var fx goldenFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("parse pricing-vectors.json: %v", err)
	}
	if len(fx.Vectors) == 0 {
		t.Fatal("golden fixture has no vectors")
	}

	for _, v := range fx.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			at := mustParseDate(t, v.At)
			u := Usage{
				Input:        v.Usage.Input,
				Output:       v.Usage.Output,
				CacheRead:    v.Usage.CacheRead,
				CacheWrite5m: v.Usage.CacheWrite5m,
				CacheWrite1h: v.Usage.CacheWrite1h,
			}
			got, err := CostForModelAt(v.Model, at, u)

			if v.Expected == "unknown_model" {
				if !errors.Is(err, ErrUnknownModel) {
					t.Fatalf("expected ErrUnknownModel for %q, got err=%v cost=%v", v.Model, err, got)
				}
				return
			}
			if v.ExpectedUSD == nil {
				t.Fatalf("vector %q has neither expected_usd nor a recognised expected sentinel (%q)", v.Name, v.Expected)
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", v.Model, err)
			}
			if math.Abs(got-*v.ExpectedUSD) > epsilon {
				t.Errorf("%s: got $%v, want $%v", v.Name, got, *v.ExpectedUSD)
			}
		})
	}
}
