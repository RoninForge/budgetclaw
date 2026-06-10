package db

import (
	"context"
	"testing"
	"time"

	"github.com/RoninForge/budgetclaw/internal/parser"
)

// syncEvent builds a minimal event for sync-aggregation tests. Cost is
// passed separately to DB.Insert, so it is not a field here.
func syncEvent(uuid, project, branch, model string, ts time.Time, in, out int) *parser.Event {
	return &parser.Event{
		UUID:                  uuid,
		SessionID:             "s",
		Timestamp:             ts,
		CWD:                   "/tmp/" + project,
		Project:               project,
		GitBranch:             branch,
		Model:                 model,
		ServiceTier:           "standard",
		InputTokens:           in,
		OutputTokens:          out,
		CacheReadTokens:       0,
		CacheCreation5mTokens: 0,
		CacheCreation1hTokens: 0,
	}
}

func TestSyncAggregatesGroupsByProjectBranchModelDay(t *testing.T) {
	d, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	ctx := context.Background()

	day1 := time.Date(2026, 6, 10, 8, 0, 0, 0, time.UTC)
	day1later := time.Date(2026, 6, 10, 20, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 6, 11, 8, 0, 0, 0, time.UTC)

	// Two events same (project, branch, model, day): must collapse into one
	// aggregate with summed cost and tokens.
	if err := d.Insert(ctx, syncEvent("u1", "app", "main", "opus", day1, 100, 50), 0.10); err != nil {
		t.Fatal(err)
	}
	if err := d.Insert(ctx, syncEvent("u2", "app", "main", "opus", day1later, 200, 80), 0.20); err != nil {
		t.Fatal(err)
	}
	// Different model, same day: separate aggregate.
	if err := d.Insert(ctx, syncEvent("u3", "app", "main", "sonnet", day1, 300, 90), 0.05); err != nil {
		t.Fatal(err)
	}
	// Different day: separate aggregate.
	if err := d.Insert(ctx, syncEvent("u4", "app", "main", "opus", day2, 10, 5), 0.01); err != nil {
		t.Fatal(err)
	}

	aggs, err := d.SyncAggregates(ctx, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(aggs) != 3 {
		t.Fatalf("got %d aggregates, want 3: %+v", len(aggs), aggs)
	}

	// Sorted by day, project, branch, model. First should be day1 opus collapsed.
	var opusDay1 *SyncAggregate
	for i := range aggs {
		if aggs[i].Day == "2026-06-10" && aggs[i].Model == "opus" {
			opusDay1 = &aggs[i]
		}
	}
	if opusDay1 == nil {
		t.Fatal("missing day1 opus aggregate")
	}
	if opusDay1.CostUSD < 0.299 || opusDay1.CostUSD > 0.301 {
		t.Errorf("collapsed cost = %v, want ~0.30", opusDay1.CostUSD)
	}
	if opusDay1.InputTokens != 300 {
		t.Errorf("collapsed input tokens = %d, want 300", opusDay1.InputTokens)
	}
	if opusDay1.OutputTokens != 130 {
		t.Errorf("collapsed output tokens = %d, want 130", opusDay1.OutputTokens)
	}
}

func TestSyncAggregatesRespectsSince(t *testing.T) {
	d, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	ctx := context.Background()

	old := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	if err := d.Insert(ctx, syncEvent("old", "app", "main", "opus", old, 1, 1), 0.01); err != nil {
		t.Fatal(err)
	}
	if err := d.Insert(ctx, syncEvent("new", "app", "main", "opus", recent, 1, 1), 0.01); err != nil {
		t.Fatal(err)
	}

	aggs, err := d.SyncAggregates(ctx, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(aggs) != 1 || aggs[0].Day != "2026-06-10" {
		t.Fatalf("since filter failed, got %+v", aggs)
	}
}

func TestSyncAggregatesEmpty(t *testing.T) {
	d, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	aggs, err := d.SyncAggregates(context.Background(), time.Now().AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	if len(aggs) != 0 {
		t.Errorf("expected no aggregates, got %d", len(aggs))
	}
}
