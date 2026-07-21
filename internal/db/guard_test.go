package db

import (
	"context"
	"testing"
	"time"

	"github.com/RoninForge/budgetclaw/internal/parser"
)

func guardEvent(uuid, project, branch string, ts time.Time) *parser.Event {
	return &parser.Event{
		UUID:        uuid,
		SessionID:   "s",
		Timestamp:   ts,
		CWD:         "/x/" + project,
		Project:     project,
		GitBranch:   branch,
		Model:       "claude-opus-4-1-20250805",
		ServiceTier: "standard",
	}
}

func TestGuardPendingQueueDedupDelete(t *testing.T) {
	d, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer func() { _ = d.Close() }()
	ctx := context.Background()

	inserted, err := d.QueueGuardEvent(ctx, "k1", `{"policyId":"gp_1","action":"kill"}`)
	if err != nil || !inserted {
		t.Fatalf("first queue: inserted=%v err=%v", inserted, err)
	}
	// Same dedup key is ignored (idempotent report).
	inserted2, err := d.QueueGuardEvent(ctx, "k1", `{"policyId":"gp_1","action":"kill"}`)
	if err != nil {
		t.Fatalf("second queue err: %v", err)
	}
	if inserted2 {
		t.Error("duplicate dedup key should not insert")
	}

	pend, err := d.PendingGuardEvents(ctx, 10)
	if err != nil {
		t.Fatalf("PendingGuardEvents: %v", err)
	}
	if len(pend) != 1 {
		t.Fatalf("pending = %d, want 1", len(pend))
	}

	if err := d.DeleteGuardEvents(ctx, []int64{pend[0].ID}); err != nil {
		t.Fatalf("DeleteGuardEvents: %v", err)
	}
	after, _ := d.PendingGuardEvents(ctx, 10)
	if len(after) != 0 {
		t.Errorf("after delete pending = %d, want 0", len(after))
	}
}

func TestTotalAndProjectSum(t *testing.T) {
	d, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer func() { _ = d.Close() }()
	ctx := context.Background()

	day := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	if err := d.Insert(ctx, guardEvent("u1", "app", "main", day), 10.0); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := d.Insert(ctx, guardEvent("u2", "other", "main", day), 5.0); err != nil {
		t.Fatalf("insert: %v", err)
	}

	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)

	total, err := d.TotalSum(ctx, start, end)
	if err != nil {
		t.Fatalf("TotalSum: %v", err)
	}
	if total != 15.0 {
		t.Errorf("TotalSum = %v, want 15", total)
	}

	proj, err := d.ProjectSum(ctx, "app", start, end)
	if err != nil {
		t.Fatalf("ProjectSum: %v", err)
	}
	if proj != 10.0 {
		t.Errorf("ProjectSum(app) = %v, want 10", proj)
	}

	// A month with no spend sums to zero, not an error.
	mayStart := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mayEnd := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	if v, _ := d.TotalSum(ctx, mayStart, mayEnd); v != 0 {
		t.Errorf("empty month TotalSum = %v, want 0", v)
	}
}
