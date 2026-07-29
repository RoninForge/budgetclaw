package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// The invariants below are the contract that makes a stale pricing
// table survivable. An event whose model has no rate is stored rather
// than discarded, so the tokens outlive Claude Code's roughly one month
// of JSONL retention and can be priced later from the row itself.

// TestInsertUnpricedRetainsTokensAndCostsNothing is the core guarantee:
// the row exists, keeps every token count, and contributes $0.
func TestInsertUnpricedRetainsTokensAndCostsNothing(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	day := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	e := testEvent("u1", "app", "main", day)
	e.Model = "claude-opus-5" // the model that broke on 2026-07-27
	if err := d.InsertUnpriced(ctx, e); err != nil {
		t.Fatalf("InsertUnpriced: %v", err)
	}

	r, err := d.RollupForDay(ctx, "app", "main", day)
	if err != nil {
		t.Fatalf("RollupForDay: %v", err)
	}
	if r == nil {
		t.Fatal("expected a rollup row for the unpriced event")
	}
	if r.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want 0", r.CostUSD)
	}
	if r.UnpricedCount != 1 {
		t.Errorf("UnpricedCount = %d, want 1", r.UnpricedCount)
	}
	if r.EventCount != 1 {
		t.Errorf("EventCount = %d, want 1", r.EventCount)
	}
	// Tokens are what make later recovery possible.
	if r.InputTokens != 1000 || r.OutputTokens != 500 || r.CacheReadTokens != 100 {
		t.Errorf("tokens not retained: %+v", r)
	}
}

// TestUnpricedDoesNotInflateCostSums proves an unpriced event is
// invisible to every dollar figure, so budget caps and Guard Mode keep
// behaving exactly as they did before this feature existed.
func TestUnpricedDoesNotInflateCostSums(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	day := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	if err := d.Insert(ctx, testEvent("priced", "app", "main", day), 3.50); err != nil {
		t.Fatal(err)
	}
	unp := testEvent("unpriced", "app", "main", day)
	unp.Model = "claude-opus-5"
	if err := d.InsertUnpriced(ctx, unp); err != nil {
		t.Fatal(err)
	}

	start, end := day.Add(-24*time.Hour), day.Add(24*time.Hour)

	sum, err := d.RollupSum(ctx, "app", "main", start, end)
	if err != nil {
		t.Fatal(err)
	}
	if sum.CostUSD != 3.50 {
		t.Errorf("RollupSum CostUSD = %v, want 3.50 (unpriced must add nothing)", sum.CostUSD)
	}
	if sum.UnpricedCount != 1 {
		t.Errorf("RollupSum UnpricedCount = %d, want 1", sum.UnpricedCount)
	}

	total, err := d.TotalSum(ctx, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3.50 {
		t.Errorf("TotalSum = %v, want 3.50", total)
	}

	proj, err := d.ProjectSum(ctx, "app", start, end)
	if err != nil {
		t.Fatal(err)
	}
	if proj != 3.50 {
		t.Errorf("ProjectSum = %v, want 3.50", proj)
	}
}

// TestUnpricedRollupMatchesEventCount asserts the bookkeeping invariant
// that keeps status honest: the number of unpriced event rows always
// equals the sum of the rollup counters, across inserts and replaces.
func TestUnpricedRollupMatchesEventCount(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	day := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	for _, id := range []string{"a", "b", "c"} {
		e := testEvent(id, "app", "main", day)
		e.Model = "claude-opus-5"
		if err := d.InsertUnpriced(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Insert(ctx, testEvent("d", "app", "main", day), 1.25); err != nil {
		t.Fatal(err)
	}

	assertUnpricedInvariant(t, d)
}

// TestReplaceHealsUnpricedRow covers the emergent recovery path: when a
// later line for the same response arrives and the table can now price
// it, the stored row flips to priced and the backlog counter clears.
// This is why a plain `budgetclaw backfill` recovers spend after an
// upgrade, with no separate repair step.
func TestReplaceHealsUnpricedRow(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	day := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	// First sighting: unpriceable, stored unpriced.
	e := testEvent("line-1", "app", "main", day)
	e.Model = "claude-opus-5"
	e.MessageID, e.RequestID = "msg-1", "req-1"
	if err := d.InsertUnpriced(ctx, e); err != nil {
		t.Fatal(err)
	}

	r, _ := d.RollupForDay(ctx, "app", "main", day)
	if r.UnpricedCount != 1 || r.CostUSD != 0 {
		t.Fatalf("setup: want unpriced=1 cost=0, got unpriced=%d cost=%v", r.UnpricedCount, r.CostUSD)
	}

	// Same response seen again, now priceable (a newer pricing table).
	// Different line uuid, same (message_id, request_id).
	e2 := testEvent("line-2", "app", "main", day)
	e2.Model = "claude-opus-5"
	e2.MessageID, e2.RequestID = "msg-1", "req-1"
	if err := d.Insert(ctx, e2, 4.25); err != nil {
		t.Fatal(err)
	}

	r, _ = d.RollupForDay(ctx, "app", "main", day)
	if r.UnpricedCount != 0 {
		t.Errorf("UnpricedCount = %d, want 0 after the row was priced", r.UnpricedCount)
	}
	if r.CostUSD != 4.25 {
		t.Errorf("CostUSD = %v, want 4.25", r.CostUSD)
	}
	if r.EventCount != 1 {
		t.Errorf("EventCount = %d, want 1 (a replace is the same response)", r.EventCount)
	}
	assertUnpricedInvariant(t, d)
}

// TestHasUnpricedAndUnpricedByModel covers the two queries status uses
// to decide whether to say anything at all.
func TestHasUnpricedAndUnpricedByModel(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	day := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	// Healthy database: status must stay silent.
	if err := d.Insert(ctx, testEvent("ok", "app", "main", day), 1.00); err != nil {
		t.Fatal(err)
	}
	has, err := d.HasUnpriced(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("HasUnpriced = true on a fully priced database")
	}

	// Two models, different volumes, so ordering is observable.
	for _, id := range []string{"o1", "o2"} {
		e := testEvent(id, "app", "main", day)
		e.Model = "claude-opus-5"
		if err := d.InsertUnpriced(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	later := testEvent("f1", "app", "main", day.Add(time.Hour))
	later.Model = "future-model"
	if err := d.InsertUnpriced(ctx, later); err != nil {
		t.Fatal(err)
	}

	if has, err = d.HasUnpriced(ctx); err != nil || !has {
		t.Fatalf("HasUnpriced = %v (err %v), want true", has, err)
	}

	models, err := d.UnpricedByModel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 unpriced models, got %d", len(models))
	}
	// Ordered by event count descending.
	if models[0].Model != "claude-opus-5" || models[0].EventCount != 2 {
		t.Errorf("first = %+v, want claude-opus-5 with 2 events", models[0])
	}
	// 1000+500+100+200+50 per event, two events.
	if want := 2 * 1850; models[0].Tokens != want {
		t.Errorf("Tokens = %d, want %d", models[0].Tokens, want)
	}
	if models[0].FirstSeen != "2026-07-27" {
		t.Errorf("FirstSeen = %q, want 2026-07-27", models[0].FirstSeen)
	}
}

// TestMigrationFromPreUnpricedSchema is the upgrade-safety test: a
// database created by a binary that predates these columns must open,
// gain the columns, and treat every existing row as priced. A regression
// here would silently mark a user's entire history as unpriced.
func TestMigrationFromPreUnpricedSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// Build the pre-migration shape by hand: the events and rollups
	// tables as they were before priced/unpriced_count existed.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE events (
			uuid TEXT PRIMARY KEY, session_id TEXT NOT NULL, ts DATETIME NOT NULL,
			cwd TEXT NOT NULL, project TEXT NOT NULL, git_branch TEXT NOT NULL,
			model TEXT NOT NULL, service_tier TEXT NOT NULL,
			input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL,
			cache_read_tokens INTEGER NOT NULL, cache_write_5m_tokens INTEGER NOT NULL,
			cache_write_1h_tokens INTEGER NOT NULL, cost_usd REAL NOT NULL,
			inserted_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE rollups (
			project TEXT NOT NULL, git_branch TEXT NOT NULL, day TEXT NOT NULL,
			event_count INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0,
			cache_write_5m_tokens INTEGER NOT NULL DEFAULT 0,
			cache_write_1h_tokens INTEGER NOT NULL DEFAULT 0,
			cost_usd REAL NOT NULL DEFAULT 0,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (project, git_branch, day)
		);
		INSERT INTO events (uuid, session_id, ts, cwd, project, git_branch,
			model, service_tier, input_tokens, output_tokens,
			cache_read_tokens, cache_write_5m_tokens, cache_write_1h_tokens, cost_usd)
		VALUES ('legacy','s1','2026-05-13 10:00:00+00:00','/tmp','app','main',
			'claude-opus-4-8','standard',1000,500,0,0,0,2.75);
		INSERT INTO rollups (project, git_branch, day, event_count,
			input_tokens, output_tokens, cost_usd)
		VALUES ('app','main','2026-05-13',1,1000,500,2.75);
	`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// Open through the real path, which runs migrate() then the schema.
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a pre-migration database: %v", err)
	}
	defer func() { _ = d.Close() }()
	ctx := context.Background()

	// The pre-existing row must count as priced: it only exists because
	// pricing succeeded under the old binary.
	has, err := d.HasUnpriced(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("legacy rows were marked unpriced by the migration")
	}

	day := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)
	r, err := d.RollupForDay(ctx, "app", "main", day)
	if err != nil {
		t.Fatal(err)
	}
	if r == nil {
		t.Fatal("legacy rollup lost during migration")
	}
	if r.CostUSD != 2.75 || r.UnpricedCount != 0 {
		t.Errorf("legacy rollup = cost %v unpriced %d, want 2.75 and 0", r.CostUSD, r.UnpricedCount)
	}

	// And the migrated database still accepts both kinds of write.
	e := testEvent("new", "app", "main", day.Add(36*time.Hour))
	e.Model = "claude-opus-5"
	if err := d.InsertUnpriced(ctx, e); err != nil {
		t.Fatalf("InsertUnpriced after migration: %v", err)
	}
	assertUnpricedInvariant(t, d)

	// Opening twice must be a no-op, since Open runs on every command.
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = d2.Close() }()
	assertUnpricedInvariant(t, d2)
}

// assertUnpricedInvariant checks the two totals that must always agree:
// stored cost matches rolled-up cost, and the unpriced row count matches
// the rolled-up counter. Drift in either means status is lying.
func assertUnpricedInvariant(t *testing.T, d *DB) {
	t.Helper()

	var eventCost, rollupCost float64
	if err := d.sql.QueryRow(`SELECT COALESCE(SUM(cost_usd),0) FROM events`).Scan(&eventCost); err != nil {
		t.Fatal(err)
	}
	if err := d.sql.QueryRow(`SELECT COALESCE(SUM(cost_usd),0) FROM rollups`).Scan(&rollupCost); err != nil {
		t.Fatal(err)
	}
	if eventCost != rollupCost {
		t.Errorf("cost drift: events sum %v, rollups sum %v", eventCost, rollupCost)
	}

	var unpricedRows, unpricedCounter int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM events WHERE priced = 0`).Scan(&unpricedRows); err != nil {
		t.Fatal(err)
	}
	if err := d.sql.QueryRow(`SELECT COALESCE(SUM(unpriced_count),0) FROM rollups`).Scan(&unpricedCounter); err != nil {
		t.Fatal(err)
	}
	if unpricedRows != unpricedCounter {
		t.Errorf("unpriced drift: %d event rows, %d counted in rollups", unpricedRows, unpricedCounter)
	}
}
