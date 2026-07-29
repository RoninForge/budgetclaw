package reconcile

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/RoninForge/budgetclaw/internal/db"
	"github.com/RoninForge/budgetclaw/internal/parser"
	"github.com/RoninForge/budgetclaw/internal/pricing"
)

// knownModel is a model the vendored pricing table can price at the
// timestamps used below. Picked from the real table so these tests
// exercise the genuine pricing path rather than a stub.
const knownModel = "claude-opus-4-8"

// unpriceableModel is not in the table and never will be.
const unpriceableModel = "model-that-does-not-exist"

func newStore(t *testing.T) *db.DB {
	t.Helper()
	store, err := db.OpenMemory()
	if err != nil {
		t.Fatalf("open memory db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func event(uuid, model string, ts time.Time) *parser.Event {
	return &parser.Event{
		UUID:                  uuid,
		SessionID:             "s1",
		Timestamp:             ts,
		CWD:                   "/tmp/app",
		Project:               "app",
		GitBranch:             "main",
		Model:                 model,
		ServiceTier:           "standard",
		InputTokens:           1_000_000,
		OutputTokens:          0,
		CacheReadTokens:       0,
		CacheCreation5mTokens: 0,
		CacheCreation1hTokens: 0,
	}
}

// day is inside the known model's priced interval.
var day = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// TestRunPricesBacklog is the headline behavior: events stored without a
// cost gain one, with no user action and without reading any log file.
func TestRunPricesBacklog(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	for _, id := range []string{"a", "b", "c"} {
		if err := store.InsertUnpriced(ctx, event(id, knownModel, day)); err != nil {
			t.Fatal(err)
		}
	}

	// Confirm the starting state really is a zero-dollar backlog.
	before, err := store.RollupForDay(ctx, "app", "main", day)
	if err != nil {
		t.Fatal(err)
	}
	if before.CostUSD != 0 || before.UnpricedCount != 3 {
		t.Fatalf("setup: want cost 0 unpriced 3, got cost %v unpriced %d",
			before.CostUSD, before.UnpricedCount)
	}

	res, err := Run(ctx, store, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Repriced != 3 {
		t.Errorf("Repriced = %d, want 3", res.Repriced)
	}
	if res.Recovered <= 0 {
		t.Errorf("Recovered = %v, want a positive dollar figure", res.Recovered)
	}

	after, err := store.RollupForDay(ctx, "app", "main", day)
	if err != nil {
		t.Fatal(err)
	}
	if after.UnpricedCount != 0 {
		t.Errorf("UnpricedCount = %d, want 0", after.UnpricedCount)
	}
	if after.CostUSD != res.Recovered {
		t.Errorf("rollup cost %v does not match reported recovery %v", after.CostUSD, res.Recovered)
	}
	// Tokens and event_count were already counted at insert; reconcile
	// must not touch them.
	if after.EventCount != 3 || after.InputTokens != 3_000_000 {
		t.Errorf("reconcile disturbed counts: %+v", after)
	}
	assertInvariant(t, store)
}

// TestRunMatchesIngestPricing pins the guarantee that a reconciled event
// is indistinguishable from one priced on arrival. Same model, same
// timestamp, same tokens: the two paths must agree exactly.
func TestRunMatchesIngestPricing(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	// What the ingest path would have computed.
	want, err := pricing.CostForModelAt(knownModel, day, pricing.Usage{Input: 1_000_000})
	if err != nil {
		t.Fatalf("pricing the reference event: %v", err)
	}

	if err := store.InsertUnpriced(ctx, event("only", knownModel, day)); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, store, false); err != nil {
		t.Fatal(err)
	}

	got, err := store.RollupForDay(ctx, "app", "main", day)
	if err != nil {
		t.Fatal(err)
	}
	if got.CostUSD != want {
		t.Errorf("reconciled cost %v, ingest would have produced %v", got.CostUSD, want)
	}
}

// TestRunIsIdempotent is the anti-double-count test. Running the pass
// repeatedly must not add the recovered dollars more than once.
func TestRunIsIdempotent(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	if err := store.InsertUnpriced(ctx, event("a", knownModel, day)); err != nil {
		t.Fatal(err)
	}

	first, err := Run(ctx, store, false)
	if err != nil {
		t.Fatal(err)
	}
	afterFirst, err := store.RollupForDay(ctx, "app", "main", day)
	if err != nil {
		t.Fatal(err)
	}

	// Force past the watermark so the pass genuinely re-runs rather than
	// short-circuiting, which is the harder case to get right.
	for i := 0; i < 3; i++ {
		res, err := Run(ctx, store, true)
		if err != nil {
			t.Fatal(err)
		}
		if res.Repriced != 0 {
			t.Errorf("re-run %d repriced %d events, want 0", i, res.Repriced)
		}
	}

	afterRepeats, err := store.RollupForDay(ctx, "app", "main", day)
	if err != nil {
		t.Fatal(err)
	}
	if afterFirst.CostUSD != afterRepeats.CostUSD {
		t.Errorf("cost drifted across repeated passes: %v then %v",
			afterFirst.CostUSD, afterRepeats.CostUSD)
	}
	if first.Repriced != 1 {
		t.Errorf("first pass repriced %d, want 1", first.Repriced)
	}
	assertInvariant(t, store)
}

// TestRunConcurrentPassesDoNotDoubleCount covers watch and status racing
// on the same database. The guarded update means the loser applies no
// rollup delta at all.
func TestRunConcurrentPassesDoNotDoubleCount(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	for i := 0; i < 25; i++ {
		if err := store.InsertUnpriced(ctx, event(string(rune('a'+i)), knownModel, day)); err != nil {
			t.Fatal(err)
		}
	}

	single, err := pricing.CostForModelAt(knownModel, day, pricing.Usage{Input: 1_000_000})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	repriced := make([]int, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			res, rerr := Run(ctx, store, true)
			if rerr != nil {
				t.Errorf("concurrent Run: %v", rerr)
				return
			}
			repriced[slot] = res.Repriced
		}(i)
	}
	wg.Wait()

	total := 0
	for _, n := range repriced {
		total += n
	}
	if total != 25 {
		t.Errorf("concurrent passes repriced %d events in total, want exactly 25", total)
	}

	got, err := store.RollupForDay(ctx, "app", "main", day)
	if err != nil {
		t.Fatal(err)
	}
	if want := single * 25; !nearly(got.CostUSD, want) {
		t.Errorf("rollup cost %v, want %v (no double counting)", got.CostUSD, want)
	}
	if got.UnpricedCount != 0 {
		t.Errorf("UnpricedCount = %d, want 0", got.UnpricedCount)
	}
	assertInvariant(t, store)
}

// TestRunLeavesStillUnpriceableAlone verifies the pass terminates and
// stays honest when a model simply cannot be priced. The rows must
// survive untouched rather than being dropped or given a made-up cost.
func TestRunLeavesStillUnpriceableAlone(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	for _, id := range []string{"x", "y"} {
		if err := store.InsertUnpriced(ctx, event(id, unpriceableModel, day)); err != nil {
			t.Fatal(err)
		}
	}

	res, err := Run(ctx, store, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Repriced != 0 {
		t.Errorf("Repriced = %d, want 0", res.Repriced)
	}
	if res.Remaining != 2 {
		t.Errorf("Remaining = %d, want 2", res.Remaining)
	}

	r, err := store.RollupForDay(ctx, "app", "main", day)
	if err != nil {
		t.Fatal(err)
	}
	if r.UnpricedCount != 2 || r.CostUSD != 0 {
		t.Errorf("want the backlog intact at zero cost, got unpriced %d cost %v",
			r.UnpricedCount, r.CostUSD)
	}
	assertInvariant(t, store)
}

// TestRunSkipsWhenPricingUnchanged checks the watermark short-circuit:
// once a pass has completed against a given table, an unpriceable row
// must not be rescanned until the table changes.
func TestRunSkipsWhenPricingUnchanged(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	if err := store.InsertUnpriced(ctx, event("x", unpriceableModel, day)); err != nil {
		t.Fatal(err)
	}

	// First pass records the watermark.
	if _, err := Run(ctx, store, false); err != nil {
		t.Fatal(err)
	}
	mark, err := store.MetaGet(ctx, watermarkKey)
	if err != nil {
		t.Fatal(err)
	}
	if mark == "" {
		t.Fatal("watermark not recorded after a completed pass")
	}
	if mark != identity() {
		t.Errorf("watermark = %q, want the current pricing identity %q", mark, identity())
	}

	// Second pass short-circuits. Remaining is left at zero because the
	// pass returns before counting, which is the intended cheap path.
	res, err := Run(ctx, store, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Repriced != 0 || res.Remaining != 0 {
		t.Errorf("expected a short-circuited pass, got %+v", res)
	}
}

// TestRunNoBacklogIsFree confirms the healthy path does nothing at all,
// including not writing a watermark, so an untouched database stays
// untouched.
func TestRunNoBacklogIsFree(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	if err := store.Insert(ctx, event("ok", knownModel, day), 2.50); err != nil {
		t.Fatal(err)
	}

	res, err := Run(ctx, store, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Any() || res.Repriced != 0 {
		t.Errorf("expected a no-op on a fully priced database, got %+v", res)
	}
	mark, err := store.MetaGet(ctx, watermarkKey)
	if err != nil {
		t.Fatal(err)
	}
	if mark != "" {
		t.Errorf("watermark written on a database with no backlog: %q", mark)
	}

	r, err := store.RollupForDay(ctx, "app", "main", day)
	if err != nil {
		t.Fatal(err)
	}
	if r.CostUSD != 2.50 {
		t.Errorf("existing cost disturbed: %v", r.CostUSD)
	}
}

// TestSummaryWording checks the user-facing line, including that it is
// empty when there is nothing to report.
func TestSummaryWording(t *testing.T) {
	if got := (Result{}).Summary(); got != "" {
		t.Errorf("empty result summary = %q, want empty", got)
	}
	one := Result{Repriced: 1530, Recovered: 105.45, Models: []string{"claude-opus-5"}}
	want := "repriced 1530 previously unpriced event(s): $105.45 recovered (claude-opus-5)"
	if got := one.Summary(); got != want {
		t.Errorf("Summary() = %q, want %q", got, want)
	}
	two := Result{Repriced: 5, Recovered: 1, Models: []string{"a", "b", "c"}}
	if got := two.Summary(); got != "repriced 5 previously unpriced event(s): $1.00 recovered (a and 2 more)" {
		t.Errorf("multi-model Summary() = %q", got)
	}
}

func nearly(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

// assertInvariant is the same bookkeeping check the db package makes:
// the unpriced row count must equal the rolled-up counter. Reconcile
// updates both, so a bug here would silently desynchronize status.
func assertInvariant(t *testing.T, store *db.DB) {
	t.Helper()
	ctx := context.Background()

	models, err := store.UnpricedByModel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rowCount := 0
	for _, m := range models {
		rowCount += m.EventCount
	}

	// Sum the rollup counters through the public range query.
	sum, err := store.RollupSum(ctx, "app", "main",
		day.Add(-72*time.Hour), day.Add(72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rowCount != sum.UnpricedCount {
		t.Errorf("unpriced drift: %d event rows, %d counted in rollups", rowCount, sum.UnpricedCount)
	}
}
