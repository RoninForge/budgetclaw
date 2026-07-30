package refresh

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RoninForge/budgetclaw/internal/pricing"
)

// Cross-path golden vectors.
//
// A price can reach a lookup by three routes, and they do not share a
// parser:
//
//	1. compiled in    - the vendored dataset through gen/main.go into
//	                    table_gen.go at build time
//	2. fetched        - the published bundle through parseBundle at run time
//	3. cached         - the same bundle re-read and re-verified from disk
//
// Routes 1 and 2 are two independent implementations of the same document
// shape, written months apart. That is the setup for a drift bug that no
// single-path test can see: each side stays internally consistent while
// they quietly disagree, and the symptom is a total that changes depending
// on whether a machine opted into refresh. Money, silently different.
//
// So the vectors are run through all three routes in ONE test and compared
// against the same expected figure. A divergence names all three numbers
// at once instead of showing up as an unexplained failure on whichever
// path happened to be tested.
//
// The fixture is the real published bundle and the real signature, so this
// is production bytes through the production code path.

// vectorEpsilon is float comparison slack. The vectors are exact decimal
// figures; this only absorbs binary representation, not real error.
const vectorEpsilon = 1e-9

type vectorFile struct {
	Vectors []vector `json:"vectors"`
}

type vector struct {
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
	ResolvesTo  string   `json:"resolves_to"`
}

// TestGoldenVectorsAcrossAllThreePaths is the parity guarantee.
func TestGoldenVectorsAcrossAllThreePaths(t *testing.T) {
	isolate(t)

	vectors := loadVectors(t)
	// After the fixture's dataModified, so the freshness gates pass.
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	// Route 1: whatever this binary was built with.
	pricing.RestoreBuiltIn()
	builtIn := evaluate(t, vectors)
	if src, _, _ := pricing.ActiveTable(); src != pricing.SourceBuiltIn {
		t.Fatalf("setup: source = %q, want %q", src, pricing.SourceBuiltIn)
	}

	// Route 2: the full network path, real bundle and real signature over
	// a local server.
	srv := bundleServer(t, nil)
	if _, err := refreshVia(context.Background(), srv, now); err != nil {
		t.Fatalf("fetch the live fixture: %v", err)
	}
	if src, _, _ := pricing.ActiveTable(); src != pricing.SourceFetched {
		t.Fatalf("setup: after a refresh the source is %q, want %q", src, pricing.SourceFetched)
	}
	fetched := evaluate(t, vectors)

	// Route 3: a restart. Back to the built-in table, then load from disk
	// with no network at all. The signature is re-verified on the way in.
	pricing.RestoreBuiltIn()
	if _, err := LoadCachedTable(now); err != nil {
		t.Fatalf("load the cached table: %v", err)
	}
	if src, _, _ := pricing.ActiveTable(); src != pricing.SourceFetched {
		t.Fatalf("setup: after a cache load the source is %q, want %q", src, pricing.SourceFetched)
	}
	cached := evaluate(t, vectors)

	coveredDirectly := bundleCoverage(t)

	for i, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			// Guard against a vacuous pass. If the fetched bundle does not
			// carry this model at all, the built-in floor answered and the
			// two routes were never actually compared.
			if v.Expected != "unknown_model" {
				id := resolvedID(v)
				if !coveredDirectly[id] {
					t.Fatalf("the fetched bundle does not carry %q, so routes 1 and 2 "+
						"were not compared here: the built-in floor answered both times", id)
				}
			}

			for _, r := range []struct {
				route  string
				result vectorResult
			}{
				{"compiled in", builtIn[i]},
				{"fetched", fetched[i]},
				{"cached", cached[i]},
			} {
				assertVector(t, r.route, v, r.result)
			}

			// The three must agree with each other, not merely each land
			// within epsilon of the expected figure.
			if !sameCost(builtIn[i], fetched[i]) || !sameCost(builtIn[i], cached[i]) {
				t.Errorf("the three routes disagree: compiled-in %v, fetched %v, cached %v",
					builtIn[i].cost, fetched[i].cost, cached[i].cost)
			}
		})
	}
}

type vectorResult struct {
	cost float64
	err  error
}

// evaluate prices every vector against whatever table is in force.
func evaluate(t *testing.T, vectors []vector) []vectorResult {
	t.Helper()
	out := make([]vectorResult, len(vectors))
	for i, v := range vectors {
		at, err := time.Parse("2006-01-02", v.At)
		if err != nil {
			t.Fatalf("vector %q has an unparseable at %q", v.Name, v.At)
		}
		cost, perr := pricing.CostForModelAt(v.Model, at.UTC(), pricing.Usage{
			Input:        v.Usage.Input,
			Output:       v.Usage.Output,
			CacheRead:    v.Usage.CacheRead,
			CacheWrite5m: v.Usage.CacheWrite5m,
			CacheWrite1h: v.Usage.CacheWrite1h,
		})
		out[i] = vectorResult{cost: cost, err: perr}
	}
	return out
}

func assertVector(t *testing.T, route string, v vector, got vectorResult) {
	t.Helper()

	// A model nobody prices must stay an error on every route. Turning
	// into a silent zero is the failure this whole guard exists to stop,
	// and a fetched table must not paper over it either.
	if v.Expected == "unknown_model" {
		if got.err == nil {
			t.Errorf("%s: %q priced at $%v, want an unknown-model error", route, v.Model, got.cost)
		}
		return
	}

	if v.ExpectedUSD == nil {
		t.Fatalf("vector %q declares neither expected_usd nor a recognised sentinel (%q)", v.Name, v.Expected)
	}
	if got.err != nil {
		t.Errorf("%s: %v", route, got.err)
		return
	}
	if math.Abs(got.cost-*v.ExpectedUSD) > vectorEpsilon {
		t.Errorf("%s: got $%v, want $%v", route, got.cost, *v.ExpectedUSD)
	}
}

func sameCost(a, b vectorResult) bool {
	if (a.err == nil) != (b.err == nil) {
		return false
	}
	return math.Abs(a.cost-b.cost) <= vectorEpsilon
}

// loadVectors reads the shared cross-engine fixture. It lives with the
// vendored dataset because it is shared with the other engines that price
// the same data, so this package reads it rather than keeping a copy that
// could drift.
func loadVectors(t *testing.T) []vector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "index", "pricing-vectors.json"))
	if err != nil {
		t.Fatalf("read pricing-vectors.json: %v", err)
	}
	var f vectorFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse pricing-vectors.json: %v", err)
	}
	if len(f.Vectors) == 0 {
		t.Fatal("the golden fixture carries no vectors")
	}
	return f.Vectors
}

// bundleCoverage is every model id the fetched bundle can answer for,
// including the aliases it carries. Used to prove a vector actually
// exercised the fetched data rather than falling through to the floor.
func bundleCoverage(t *testing.T) map[string]bool {
	t.Helper()
	bundle, _ := liveFixture(t)
	table, _, err := parseBundle(bundle)
	if err != nil {
		t.Fatalf("parse the live fixture: %v", err)
	}

	covered := make(map[string]bool, len(table.Models)*2)
	for _, m := range table.Models {
		covered[m.ID] = true
		for _, a := range m.Aliases {
			covered[a] = true
		}
	}
	return covered
}

// resolvedID is the model id a vector should be looked up under, with the
// display suffix Claude Code appends stripped off.
func resolvedID(v vector) string {
	if v.ResolvesTo != "" {
		return v.ResolvesTo
	}
	return strings.TrimSuffix(v.Model, "[1m]")
}

// TestFetchedBundleCarriesItsAliases pins a property the fetch path
// depends on and that is easy to lose upstream without noticing.
//
// Claude Code writes short undated ids for some models, so the alias map
// is what makes them priceable. The compiled-in table gets aliases from
// the vendored index; the fetched bundle has to carry its own, because
// Install only learns the ones a bundle declares. If the exporter ever
// stopped emitting them, everything here would still verify and install,
// and a machine with refresh enabled would simply stop pricing any model
// it knows only by its short name.
func TestFetchedBundleCarriesItsAliases(t *testing.T) {
	covered := bundleCoverage(t)

	// Dated canonical ids whose short form is what actually shows up in a
	// log. Each must be reachable through the bundle's own aliases.
	for alias, canonical := range map[string]string{
		"claude-haiku-4-5":  "claude-haiku-4-5-20251001",
		"claude-opus-4-5":   "claude-opus-4-5-20251101",
		"claude-sonnet-4-5": "claude-sonnet-4-5-20250929",
		"claude-opus-4-1":   "claude-opus-4-1-20250805",
	} {
		if !covered[canonical] {
			t.Errorf("the bundle no longer carries %q", canonical)
			continue
		}
		if !covered[alias] {
			t.Errorf("the bundle carries %q but not its alias %q, so a log line naming "+
				"the short form would go unpriced on a machine using fetched prices", canonical, alias)
		}
	}
}
