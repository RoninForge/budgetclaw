package db

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/RoninForge/budgetclaw/internal/parser"
)

const epsilon = 1e-9

// newTestDB opens an in-memory database and registers cleanup.
// Every test that needs a fresh db starts with this.
func newTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// testEvent builds a parser.Event with realistic token counts. Tests
// override the uuid/project/branch/timestamp fields they care about
// and rely on the defaults for the rest.
func testEvent(uuid, project, branch string, ts time.Time) *parser.Event {
	return &parser.Event{
		UUID:                  uuid,
		SessionID:             "test-session",
		Timestamp:             ts,
		CWD:                   "/home/user/" + project,
		Project:               project,
		GitBranch:             branch,
		Model:                 "claude-opus-4-6",
		ServiceTier:           "standard",
		InputTokens:           1000,
		OutputTokens:          500,
		CacheReadTokens:       100,
		CacheCreation5mTokens: 200,
		CacheCreation1hTokens: 50,
	}
}

// TestOpenMemory verifies a fresh in-memory db is usable for
// queries against the empty schema.
func TestOpenMemory(t *testing.T) {
	d := newTestDB(t)

	// An empty database should report zero rollups.
	rows, err := d.StatusByProject(context.Background(),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("StatusByProject on empty db: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}
}

// TestInsertEventWritesBothTables proves that a single Insert
// atomically creates an event row and a matching rollup row.
func TestInsertEventWritesBothTables(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	ts := time.Date(2026, 4, 9, 12, 34, 56, 0, time.UTC)
	e := testEvent("uuid-1", "myapp", "main", ts)

	if err := d.Insert(ctx, e, 0.50); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Event row
	var count int
	if err := d.sql.QueryRow("SELECT COUNT(*) FROM events WHERE uuid = ?", "uuid-1").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("events row count = %d, want 1", count)
	}

	// Rollup row
	r, err := d.RollupForDay(ctx, "myapp", "main", ts)
	if err != nil {
		t.Fatal(err)
	}
	if r == nil {
		t.Fatal("rollup row missing after insert")
	}
	if r.EventCount != 1 {
		t.Errorf("EventCount = %d, want 1", r.EventCount)
	}
	if r.InputTokens != 1000 {
		t.Errorf("InputTokens = %d, want 1000", r.InputTokens)
	}
	if r.OutputTokens != 500 {
		t.Errorf("OutputTokens = %d, want 500", r.OutputTokens)
	}
	if r.CacheReadTokens != 100 {
		t.Errorf("CacheReadTokens = %d, want 100", r.CacheReadTokens)
	}
	if r.CacheWrite5mTokens != 200 {
		t.Errorf("CacheWrite5mTokens = %d, want 200", r.CacheWrite5mTokens)
	}
	if r.CacheWrite1hTokens != 50 {
		t.Errorf("CacheWrite1hTokens = %d, want 50", r.CacheWrite1hTokens)
	}
	if math.Abs(r.CostUSD-0.50) > epsilon {
		t.Errorf("CostUSD = %v, want 0.50", r.CostUSD)
	}
	if r.Day != "2026-04-09" {
		t.Errorf("Day = %q, want 2026-04-09", r.Day)
	}
}

// TestInsertIdempotent guarantees that the same event inserted
// multiple times never double-counts in the rollup.
func TestInsertIdempotent(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	ts := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	e := testEvent("uuid-dup", "myapp", "main", ts)

	for i := 0; i < 5; i++ {
		if err := d.Insert(ctx, e, 0.25); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}

	// events: exactly 1 row
	var count int
	if err := d.sql.QueryRow("SELECT COUNT(*) FROM events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("after 5 inserts: events row count = %d, want 1", count)
	}

	// rollups: EventCount=1, CostUSD=0.25 (not 1.25)
	r, err := d.RollupForDay(ctx, "myapp", "main", ts)
	if err != nil {
		t.Fatal(err)
	}
	if r == nil {
		t.Fatal("rollup missing")
	}
	if r.EventCount != 1 {
		t.Errorf("EventCount = %d, want 1 (idempotency violated)", r.EventCount)
	}
	if math.Abs(r.CostUSD-0.25) > epsilon {
		t.Errorf("CostUSD = %v, want 0.25 (idempotency violated)", r.CostUSD)
	}
}

// TestMultipleEventsSameDayRollUp verifies aggregation correctness
// when many distinct events share a (project, branch, day).
func TestMultipleEventsSameDayRollUp(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	day := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	costs := []float64{0.25, 0.50, 0.75}

	for i, c := range costs {
		e := testEvent(fmt.Sprintf("e%d", i), "myapp", "main", day.Add(time.Duration(i)*time.Minute))
		if err := d.Insert(ctx, e, c); err != nil {
			t.Fatal(err)
		}
	}

	r, err := d.RollupForDay(ctx, "myapp", "main", day)
	if err != nil {
		t.Fatal(err)
	}
	if r.EventCount != 3 {
		t.Errorf("EventCount = %d, want 3", r.EventCount)
	}
	if r.InputTokens != 3000 {
		t.Errorf("InputTokens = %d, want 3000", r.InputTokens)
	}
	if r.OutputTokens != 1500 {
		t.Errorf("OutputTokens = %d, want 1500", r.OutputTokens)
	}
	if math.Abs(r.CostUSD-1.50) > epsilon {
		t.Errorf("CostUSD = %v, want 1.50", r.CostUSD)
	}
}

// TestDifferentDaysCreateSeparateRollups verifies that a UTC day
// boundary crossing produces distinct rollup rows.
func TestDifferentDaysCreateSeparateRollups(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	day1 := time.Date(2026, 4, 9, 23, 30, 0, 0, time.UTC)
	day2 := time.Date(2026, 4, 10, 0, 30, 0, 0, time.UTC)

	if err := d.Insert(ctx, testEvent("e1", "myapp", "main", day1), 0.10); err != nil {
		t.Fatal(err)
	}
	if err := d.Insert(ctx, testEvent("e2", "myapp", "main", day2), 0.20); err != nil {
		t.Fatal(err)
	}

	r1, err := d.RollupForDay(ctx, "myapp", "main", day1)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := d.RollupForDay(ctx, "myapp", "main", day2)
	if err != nil {
		t.Fatal(err)
	}

	if r1 == nil || r2 == nil {
		t.Fatal("expected two rollup rows")
	}
	if r1.Day == r2.Day {
		t.Errorf("both rollups have day=%q; should differ", r1.Day)
	}
	if r1.EventCount != 1 || r2.EventCount != 1 {
		t.Errorf("counts: %d, %d", r1.EventCount, r2.EventCount)
	}
}

// TestRollupForDayMissing returns (nil, nil) for unknown rows.
func TestRollupForDayMissing(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	r, err := d.RollupForDay(ctx, "nothing", "nowhere", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r != nil {
		t.Errorf("expected nil rollup, got %+v", r)
	}
}

// TestRollupSumAcrossRange inserts 5 days of rollups and sums a
// 3-day subrange.
func TestRollupSumAcrossRange(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		e := testEvent(fmt.Sprintf("e%d", i), "myapp", "main", base.AddDate(0, 0, i))
		if err := d.Insert(ctx, e, 1.00); err != nil {
			t.Fatal(err)
		}
	}

	// Sum days 1..3 inclusive (3 events).
	start := base.AddDate(0, 0, 1)
	end := base.AddDate(0, 0, 3)

	r, err := d.RollupSum(ctx, "myapp", "main", start, end)
	if err != nil {
		t.Fatal(err)
	}
	if r.EventCount != 3 {
		t.Errorf("EventCount = %d, want 3", r.EventCount)
	}
	if math.Abs(r.CostUSD-3.00) > epsilon {
		t.Errorf("CostUSD = %v, want 3.00", r.CostUSD)
	}
	if r.Project != "myapp" || r.GitBranch != "main" {
		t.Errorf("project/branch not threaded: %q/%q", r.Project, r.GitBranch)
	}
	if r.Day != "" {
		t.Errorf("range sum Day should be empty, got %q", r.Day)
	}
}

// TestRollupSumEmptyRange returns a zero Rollup (not an error) when
// the range has no events.
func TestRollupSumEmptyRange(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	r, err := d.RollupSum(ctx, "myapp", "main",
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if r.EventCount != 0 || r.CostUSD != 0 {
		t.Errorf("expected zero rollup, got %+v", r)
	}
}

// TestStatusByProject returns one row per (project, branch) with
// sums over the range, ordered deterministically.
func TestStatusByProject(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	ts := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)

	inserts := []struct {
		uuid    string
		project string
		branch  string
		cost    float64
	}{
		{"e1", "app1", "main", 1.00},
		{"e2", "app1", "feature/x", 2.00},
		{"e3", "app2", "main", 3.00},
		{"e4", "app1", "main", 0.50}, // rolls into (app1,main)
	}
	for _, i := range inserts {
		if err := d.Insert(ctx, testEvent(i.uuid, i.project, i.branch, ts), i.cost); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := d.StatusByProject(ctx, ts.AddDate(0, 0, -1), ts.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}

	want := []struct {
		project string
		branch  string
		count   int
		cost    float64
	}{
		{"app1", "feature/x", 1, 2.00},
		{"app1", "main", 2, 1.50},
		{"app2", "main", 1, 3.00},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(rows), len(want))
	}
	for i, w := range want {
		g := rows[i]
		if g.Project != w.project || g.GitBranch != w.branch {
			t.Errorf("row %d: got (%s,%s), want (%s,%s)",
				i, g.Project, g.GitBranch, w.project, w.branch)
		}
		if g.EventCount != w.count {
			t.Errorf("row %d count: got %d, want %d", i, g.EventCount, w.count)
		}
		if math.Abs(g.CostUSD-w.cost) > epsilon {
			t.Errorf("row %d cost: got %v, want %v", i, g.CostUSD, w.cost)
		}
	}
}

// TestInsertNilEventRejected guards the public API.
func TestInsertNilEventRejected(t *testing.T) {
	d := newTestDB(t)
	if err := d.Insert(context.Background(), nil, 0); err == nil {
		t.Error("expected error on nil event")
	}
}

// TestOpenFilePersistsWithWAL opens a file-backed db, inserts an
// event, closes, reopens, and verifies:
//  1. The event is still there (WAL checkpointing works).
//  2. journal_mode returns "wal" on the reopened connection.
func TestOpenFilePersistsWithWAL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var mode string
	if err := d.sql.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("expected journal_mode=wal, got %q", mode)
	}

	ts := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	if err := d.Insert(context.Background(), testEvent("persist", "myapp", "main", ts), 0.99); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	d2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = d2.Close() })

	r, err := d2.RollupForDay(context.Background(), "myapp", "main", ts)
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || r.EventCount != 1 {
		t.Errorf("persisted rollup wrong: %+v", r)
	}
	if math.Abs(r.CostUSD-0.99) > epsilon {
		t.Errorf("persisted CostUSD = %v, want 0.99", r.CostUSD)
	}
}

// TestOpenDefaultPath exercises the XDG path resolution branch.
// Uses t.Setenv to override XDG_STATE_HOME so we don't pollute the
// user's real state dir.
func TestOpenDefaultPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	d, err := Open("")
	if err != nil {
		t.Fatalf("Open empty path: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	// Verify the file was created in the XDG location.
	want := filepath.Join(dir, "budgetclaw", "state.db")
	if _, err := osStat(want); err != nil {
		t.Errorf("expected db at %s, stat err: %v", want, err)
	}
}
