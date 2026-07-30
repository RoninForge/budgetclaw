package pricing

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func ptr(t time.Time) *time.Time { return &t }

// restore puts the built-in table back after a test that installs one,
// so tests cannot leak state into each other through the package global.
func restore(t *testing.T) {
	t.Helper()
	t.Cleanup(RestoreBuiltIn)
}

// TestBuiltInIsActiveByDefault pins the default posture: a binary that
// never fetches anything prices from its compiled-in table.
func TestBuiltInIsActiveByDefault(t *testing.T) {
	RestoreBuiltIn()
	src, tag, dataDate := ActiveTable()
	if src != SourceBuiltIn {
		t.Errorf("source = %q, want %q", src, SourceBuiltIn)
	}
	if tag == "" || dataDate == "" {
		t.Errorf("built-in table should report its provenance, got tag %q dataDate %q", tag, dataDate)
	}
}

// TestInstallOverlaysPrices is the core of the feature: a verified table
// changes what a lookup returns, including for a model the binary never
// knew.
func TestInstallOverlaysPrices(t *testing.T) {
	restore(t)

	const future = "claude-opus-99"
	if _, err := RatesForAt(future, day(2026, 8, 1)); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("setup: %q should be unknown to the built-in table, got %v", future, err)
	}

	err := Install(ExternalTable{
		Tag:      "v2026.08.01-abc1234",
		DataDate: "2026-08-01",
		Models: []ExternalModel{{
			ID:     future,
			Input:  []ExternalInterval{{From: day(2026, 7, 1), Price: 7}},
			Output: []ExternalInterval{{From: day(2026, 7, 1), Price: 35}},
		}},
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	r, err := RatesForAt(future, day(2026, 8, 1))
	if err != nil {
		t.Fatalf("the overlaid model should now price: %v", err)
	}
	if r.InputPerMTok != 7 || r.OutputPerMTok != 35 {
		t.Errorf("rates = %+v, want input 7 output 35", r)
	}
	// Cache rates are derived, so they must follow the new input rate.
	// Compared with a tolerance because 7 * 0.10 is not exactly 0.7 in
	// binary floating point.
	if got, want := r.CacheReadPerMTok, 0.7; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("CacheReadPerMTok = %v, want %v (0.1 x input)", got, want)
	}

	src, tag, dataDate := ActiveTable()
	if src != SourceFetched || tag != "v2026.08.01-abc1234" || dataDate != "2026-08-01" {
		t.Errorf("ActiveTable() = %q %q %q", src, tag, dataDate)
	}
	if DatasetDate() != "2026-08-01" {
		t.Errorf("DatasetDate() = %q", DatasetDate())
	}
}

// TestInstallKeepsBuiltInAsFloor is the safety property. A bundle that
// omits a model must not make already-recorded spend unpriceable, which
// also denies a hostile bundle the ability to hide spend by deletion.
func TestInstallKeepsBuiltInAsFloor(t *testing.T) {
	restore(t)

	// A model the built-in table does know.
	const known = "claude-opus-4-8"
	before, err := RatesForAt(known, day(2026, 6, 1))
	if err != nil {
		t.Fatalf("setup: %q should be known: %v", known, err)
	}

	// Install a table that mentions only an unrelated model.
	if err := Install(ExternalTable{
		Tag:      "v2026.08.01-abc1234",
		DataDate: "2026-08-01",
		Models: []ExternalModel{{
			ID:     "some-other-model",
			Input:  []ExternalInterval{{From: day(2026, 7, 1), Price: 1}},
			Output: []ExternalInterval{{From: day(2026, 7, 1), Price: 2}},
		}},
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	after, err := RatesForAt(known, day(2026, 6, 1))
	if err != nil {
		t.Fatalf("%q must still price after an unrelated overlay: %v", known, err)
	}
	if after != before {
		t.Errorf("built-in rates changed: before %+v, after %+v", before, after)
	}
}

// TestInstallReplacesAModelWholesale checks the other half of the
// overlay rule: a fetched model's series replaces the built-in one
// entirely, so an upstream correction wins rather than merging.
func TestInstallReplacesAModelWholesale(t *testing.T) {
	restore(t)

	const known = "claude-opus-4-8"
	if err := Install(ExternalTable{
		Tag:      "v2026.08.01-abc1234",
		DataDate: "2026-08-01",
		Models: []ExternalModel{{
			ID:     known,
			Input:  []ExternalInterval{{From: day(2020, 1, 1), Price: 99}},
			Output: []ExternalInterval{{From: day(2020, 1, 1), Price: 199}},
		}},
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	r, err := RatesForAt(known, day(2026, 6, 1))
	if err != nil {
		t.Fatal(err)
	}
	if r.InputPerMTok != 99 {
		t.Errorf("InputPerMTok = %v, want the overlaid 99", r.InputPerMTok)
	}
}

// TestInstallResolvesAliases verifies an overlaid model is reachable
// through its aliases, which is how Claude Code's short undated ids
// resolve.
func TestInstallResolvesAliases(t *testing.T) {
	restore(t)

	if err := Install(ExternalTable{
		Tag:      "v1",
		DataDate: "2026-08-01",
		Models: []ExternalModel{{
			ID:      "claude-future-9-20260801",
			Aliases: []string{"claude-future-9"},
			Input:   []ExternalInterval{{From: day(2026, 8, 1), Price: 3}},
			Output:  []ExternalInterval{{From: day(2026, 8, 1), Price: 15}},
		}},
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Via the alias, and via the alias plus the "[1m]" display suffix.
	for _, id := range []string{"claude-future-9", "claude-future-9[1m]"} {
		if _, err := RatesForAt(id, day(2026, 8, 2)); err != nil {
			t.Errorf("%q should resolve: %v", id, err)
		}
	}
}

// TestInstallRejectsStructurallyBadTables covers every invariant priceAt
// relies on. A violation would not crash, it would silently return the
// wrong rate, which is the exact failure this package exists to prevent.
func TestInstallRejectsStructurallyBadTables(t *testing.T) {
	restore(t)

	ok := []ExternalInterval{{From: day(2026, 1, 1), Price: 5}}

	cases := map[string]ExternalTable{
		"no models": {Tag: "v1"},
		"model with no id": {Tag: "v1", Models: []ExternalModel{
			{Input: ok, Output: ok},
		}},
		"no input intervals": {Tag: "v1", Models: []ExternalModel{
			{ID: "m", Output: ok},
		}},
		"no output intervals": {Tag: "v1", Models: []ExternalModel{
			{ID: "m", Input: ok},
		}},
		"zero price": {Tag: "v1", Models: []ExternalModel{
			{ID: "m", Input: []ExternalInterval{{From: day(2026, 1, 1), Price: 0}}, Output: ok},
		}},
		"negative price": {Tag: "v1", Models: []ExternalModel{
			{ID: "m", Input: []ExternalInterval{{From: day(2026, 1, 1), Price: -5}}, Output: ok},
		}},
		"missing from date": {Tag: "v1", Models: []ExternalModel{
			{ID: "m", Input: []ExternalInterval{{Price: 5}}, Output: ok},
		}},
		"interval ends before it starts": {Tag: "v1", Models: []ExternalModel{
			{ID: "m", Input: []ExternalInterval{
				{From: day(2026, 6, 1), To: ptr(day(2026, 1, 1)), Price: 5},
			}, Output: ok},
		}},
		"overlapping intervals": {Tag: "v1", Models: []ExternalModel{
			{ID: "m", Input: []ExternalInterval{
				{From: day(2026, 1, 1), To: ptr(day(2026, 6, 1)), Price: 5},
				{From: day(2026, 3, 1), Price: 6}, // starts before the previous ends
			}, Output: ok},
		}},
		"interval after an open one": {Tag: "v1", Models: []ExternalModel{
			{ID: "m", Input: []ExternalInterval{
				{From: day(2026, 1, 1), Price: 5}, // open
				{From: day(2026, 6, 1), Price: 6}, // unreachable
			}, Output: ok},
		}},
	}

	for name, tbl := range cases {
		t.Run(name, func(t *testing.T) {
			RestoreBuiltIn()
			err := Install(tbl)
			if err == nil {
				t.Fatal("a structurally invalid table was installed")
			}
			if !errors.Is(err, ErrInvalidTable) {
				t.Errorf("error should wrap ErrInvalidTable, got %v", err)
			}
			// A rejected install must leave the previous table in force.
			if src, _, _ := ActiveTable(); src != SourceBuiltIn {
				t.Errorf("after a rejected install the source is %q, want %q", src, SourceBuiltIn)
			}
		})
	}
}

// TestRestoreBuiltIn covers the safety valve.
func TestRestoreBuiltIn(t *testing.T) {
	restore(t)

	if err := Install(ExternalTable{
		Tag: "v1", DataDate: "2026-08-01",
		Models: []ExternalModel{{
			ID:     "m",
			Input:  []ExternalInterval{{From: day(2026, 1, 1), Price: 5}},
			Output: []ExternalInterval{{From: day(2026, 1, 1), Price: 10}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if src, _, _ := ActiveTable(); src != SourceFetched {
		t.Fatalf("setup: source = %q", src)
	}

	RestoreBuiltIn()
	if src, _, _ := ActiveTable(); src != SourceBuiltIn {
		t.Errorf("source = %q after RestoreBuiltIn, want %q", src, SourceBuiltIn)
	}
	if _, err := RatesForAt("m", day(2026, 2, 1)); !errors.Is(err, ErrUnknownModel) {
		t.Errorf("the overlaid model should be gone, got %v", err)
	}
}

// TestConcurrentReadsDuringSwap runs pricing lookups while tables are
// swapped underneath. The daemon prices events continuously while a
// background refresh may install a new table, so a torn read here would
// be a real, hard-to-reproduce bug. Run with -race.
func TestConcurrentReadsDuringSwap(t *testing.T) {
	restore(t)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Readers: price a known model over and over.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					// Either rate is acceptable; a torn table is not.
					if _, err := RatesForAt("claude-opus-4-8", day(2026, 6, 1)); err != nil {
						t.Errorf("lookup failed mid-swap: %v", err)
						return
					}
				}
			}
		}()
	}

	// Writer: swap between two valid tables.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			price := float64(10 + i%5)
			_ = Install(ExternalTable{
				Tag: "v1", DataDate: "2026-08-01",
				Models: []ExternalModel{{
					ID:     "claude-opus-4-8",
					Input:  []ExternalInterval{{From: day(2020, 1, 1), Price: price}},
					Output: []ExternalInterval{{From: day(2020, 1, 1), Price: price * 5}},
				}},
			})
		}
		close(stop)
	}()

	wg.Wait()
}
