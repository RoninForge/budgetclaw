package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RoninForge/budgetclaw/internal/budget"
	"github.com/RoninForge/budgetclaw/internal/db"
	"github.com/RoninForge/budgetclaw/internal/enforcer"
	"github.com/RoninForge/budgetclaw/internal/ntfy"
	"github.com/RoninForge/budgetclaw/internal/parser"
)

// fakeKiller is the same shape as enforcer's internal test fake
// but duplicated here because the enforcer fake is test-only and
// not exported. Records every call so tests can assert that the
// pipeline asks for the right process set.
type fakeKiller struct {
	byCwd      map[string][]int
	killed     [][]int
	findCalls  int32
	killCalls  int32
}

func (f *fakeKiller) FindByCWD(_ context.Context, cwd string) ([]int, error) {
	atomic.AddInt32(&f.findCalls, 1)
	return f.byCwd[cwd], nil
}

func (f *fakeKiller) Kill(_ context.Context, pids []int) ([]int, error) {
	atomic.AddInt32(&f.killCalls, 1)
	f.killed = append(f.killed, pids)
	return pids, nil
}

// newNtfyRecorder returns a ntfy.Client pointed at an httptest
// server whose handler records every call. Tests assert on the
// records to verify warn/kill dispatch without running the real
// retry loop (we use zero backoff).
func newNtfyRecorder(t *testing.T, status int) (*ntfy.Client, *[]http.Header) {
	t.Helper()
	var hdrs []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdrs = append(hdrs, r.Header.Clone())
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return ntfy.New(ntfy.Options{
		Server:      srv.URL,
		Topic:       "test",
		BackoffFunc: func(int) time.Duration { return 0 },
		MaxRetries:  1,
	}), &hdrs
}

// buildPipeline constructs a Pipeline wired with in-memory db,
// temp-dir lock store, fake killer, and the provided config +
// notifier. Used by every test.
func buildPipeline(t *testing.T, cfg *budget.Config, notifier *ntfy.Client, fk *fakeKiller) *Pipeline {
	t.Helper()

	store, err := db.OpenMemory()
	if err != nil {
		t.Fatalf("db.OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ls, err := enforcer.NewLockStoreAt(t.TempDir())
	if err != nil {
		t.Fatalf("NewLockStoreAt: %v", err)
	}

	if fk == nil {
		fk = &fakeKiller{byCwd: map[string][]int{}}
	}
	enf := &enforcer.Enforcer{Locks: ls, Killer: fk}

	if notifier == nil {
		notifier = ntfy.New(ntfy.Options{}) // noop
	}

	return &Pipeline{
		Config:   cfg,
		DB:       store,
		Enforcer: enf,
		Notifier: notifier,
	}
}

// sampleEvent returns a parser.Event with the given identity.
// Token counts are fixed so cost math is predictable.
func sampleEvent(uuid, project, branch string) *parser.Event {
	return &parser.Event{
		UUID:                  uuid,
		SessionID:             "s1",
		Timestamp:             time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC),
		CWD:                   "/home/u/" + project,
		Project:               project,
		GitBranch:             branch,
		Model:                 "claude-opus-4-6",
		ServiceTier:           "standard",
		InputTokens:           1_000_000, // $15 at opus rates
		OutputTokens:          0,
		CacheReadTokens:       0,
		CacheCreation5mTokens: 0,
		CacheCreation1hTokens: 0,
	}
}

// TestHandleNilEvent is a defensive no-op test: a nil event must
// not crash the pipeline. The watcher should never pass nil but
// belt-and-suspenders is cheap.
func TestHandleNilEvent(t *testing.T) {
	p := buildPipeline(t, nil, nil, nil)
	if err := p.Handle(context.Background(), nil, "/path"); err != nil {
		t.Errorf("nil event should be no-op, got %v", err)
	}
}

// TestHandleMissingDependencies surfaces configuration errors
// instead of panicking on nil-field access.
func TestHandleMissingDependencies(t *testing.T) {
	p := &Pipeline{}
	err := p.Handle(context.Background(), sampleEvent("u1", "app", "main"), "")
	if err == nil {
		t.Error("expected error on missing dependencies")
	}
}

// TestHandleUnknownModelDroppedSilently verifies that an event
// for a model we don't have pricing for is logged and skipped
// rather than halting the watcher. No db insert, no verdict.
func TestHandleUnknownModelDroppedSilently(t *testing.T) {
	p := buildPipeline(t, nil, nil, nil)

	e := sampleEvent("u1", "app", "main")
	e.Model = "gpt-5-xl"

	if err := p.Handle(context.Background(), e, ""); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Verify nothing was inserted.
	rows, _ := p.DB.StatusByProject(context.Background(),
		time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 9, 23, 59, 59, 0, time.UTC))
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}
}

// TestHandleUnknownModelDedupesPerRun feeds three events with the
// same unknown model and one with a different unknown model, then
// asserts the pipeline tracks one count per model rather than one
// per event. The verbatim WARN-flood reproduced in production
// (claude-opus-4-7 before the fix) is what motivated this dedupe.
func TestHandleUnknownModelDedupesPerRun(t *testing.T) {
	p := buildPipeline(t, nil, nil, nil)

	for i, model := range []string{"gpt-5-xl", "gpt-5-xl", "gpt-5-xl", "future-claude-99"} {
		e := sampleEvent("u-"+string(rune('a'+i)), "app", "main")
		e.Model = model
		if err := p.Handle(context.Background(), e, ""); err != nil {
			t.Fatalf("Handle %d: %v", i, err)
		}
	}

	got := p.UnknownModels()
	if got["gpt-5-xl"] != 3 {
		t.Errorf("gpt-5-xl count = %d, want 3", got["gpt-5-xl"])
	}
	if got["future-claude-99"] != 1 {
		t.Errorf("future-claude-99 count = %d, want 1", got["future-claude-99"])
	}
	if len(got) != 2 {
		t.Errorf("expected 2 distinct unknown models, got %d: %v", len(got), got)
	}
}

// TestHandleNoConfigInsertsButDoesNotEvaluate verifies that a
// missing (nil) budget.Config still stores the event — the CLI's
// `status` command must work even before any rules exist.
func TestHandleNoConfigInsertsButDoesNotEvaluate(t *testing.T) {
	p := buildPipeline(t, nil, nil, nil)

	e := sampleEvent("u1", "app", "main")
	if err := p.Handle(context.Background(), e, ""); err != nil {
		t.Fatal(err)
	}

	rows, _ := p.DB.StatusByProject(context.Background(),
		time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 9, 23, 59, 59, 0, time.UTC))
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].CostUSD != 15.00 {
		t.Errorf("CostUSD = %v, want 15.00", rows[0].CostUSD)
	}
}

// TestHandleBelowCapNoBreach exercises the happy path where a
// rule exists, the event is recorded, but spend is under the cap.
// No notification, no lock.
func TestHandleBelowCapNoBreach(t *testing.T) {
	cfg := &budget.Config{
		Timezone: time.UTC,
		Rules: []budget.Rule{{
			Project: "*", Branch: "*",
			Period: budget.PeriodDaily,
			CapUSD: 100.00,
			Action: budget.ActionWarn,
		}},
	}
	notifier, hdrs := newNtfyRecorder(t, 200)
	p := buildPipeline(t, cfg, notifier, nil)

	e := sampleEvent("u1", "app", "main") // costs $15
	if err := p.Handle(context.Background(), e, ""); err != nil {
		t.Fatal(err)
	}

	if len(*hdrs) != 0 {
		t.Errorf("expected 0 ntfy calls, got %d", len(*hdrs))
	}
}

// TestHandleWarnBreach verifies that a breached warn rule fires
// a SendWarn with correct title/priority headers.
func TestHandleWarnBreach(t *testing.T) {
	cfg := &budget.Config{
		Timezone: time.UTC,
		Rules: []budget.Rule{{
			Project: "*", Branch: "*",
			Period: budget.PeriodDaily,
			CapUSD: 5.00, // $15 event will breach
			Action: budget.ActionWarn,
		}},
	}
	notifier, hdrs := newNtfyRecorder(t, 200)
	fk := &fakeKiller{byCwd: map[string][]int{}}
	p := buildPipeline(t, cfg, notifier, fk)

	if err := p.Handle(context.Background(), sampleEvent("u1", "app", "main"), ""); err != nil {
		t.Fatal(err)
	}

	if len(*hdrs) != 1 {
		t.Fatalf("expected 1 ntfy call, got %d", len(*hdrs))
	}
	if got := (*hdrs)[0].Get("Priority"); got != "4" {
		t.Errorf("Priority = %q, want 4 (warn)", got)
	}
	if got := (*hdrs)[0].Get("Tags"); got != "warning" {
		t.Errorf("Tags = %q", got)
	}

	// Warn never kills.
	if atomic.LoadInt32(&fk.killCalls) != 0 {
		t.Error("warn should not have invoked the killer")
	}
}

// TestHandleKillBreach verifies the full kill path:
// 1. SendKill fires
// 2. Lock file is written with PeriodEnd as ExpiresAt
// 3. Killer.Kill is called with matching PIDs
func TestHandleKillBreach(t *testing.T) {
	cfg := &budget.Config{
		Timezone: time.UTC,
		Rules: []budget.Rule{{
			Project: "*", Branch: "*",
			Period: budget.PeriodDaily,
			CapUSD: 5.00,
			Action: budget.ActionKill,
		}},
	}
	notifier, hdrs := newNtfyRecorder(t, 200)
	fk := &fakeKiller{byCwd: map[string][]int{
		"/home/u/app": {5001, 5002},
	}}
	p := buildPipeline(t, cfg, notifier, fk)

	if err := p.Handle(context.Background(), sampleEvent("u1", "app", "main"), ""); err != nil {
		t.Fatal(err)
	}

	// ntfy call with kill priority + tags
	if len(*hdrs) != 1 {
		t.Fatalf("expected 1 ntfy call, got %d", len(*hdrs))
	}
	if got := (*hdrs)[0].Get("Priority"); got != "5" {
		t.Errorf("Priority = %q, want 5 (kill)", got)
	}

	// Lock written
	lk, _ := p.Enforcer.Locks.IsLocked("app", "main")
	if lk == nil {
		t.Fatal("expected lock to be written")
	}
	if lk.CapUSD != 5.00 || lk.CurrentUSD < 15.00 {
		t.Errorf("lock content wrong: %+v", lk)
	}
	if lk.ExpiresAt.IsZero() {
		t.Error("ExpiresAt not set from period end")
	}

	// Killer called with both PIDs
	if len(fk.killed) != 1 {
		t.Fatalf("expected 1 Kill call, got %d", len(fk.killed))
	}
	if len(fk.killed[0]) != 2 {
		t.Errorf("killed pids = %v", fk.killed[0])
	}
}

// TestHandleAlreadyLockedReKills verifies the "silent relaunch"
// defense: a second event arriving while a lock is active causes
// Kill to run again without creating a duplicate lock and without
// re-evaluating budgets.
func TestHandleAlreadyLockedReKills(t *testing.T) {
	cfg := &budget.Config{
		Timezone: time.UTC,
		Rules: []budget.Rule{{
			Project: "*", Branch: "*",
			Period: budget.PeriodDaily,
			CapUSD: 5.00,
			Action: budget.ActionKill,
		}},
	}
	notifier, hdrs := newNtfyRecorder(t, 200)
	fk := &fakeKiller{byCwd: map[string][]int{
		"/home/u/app": {9001},
	}}
	p := buildPipeline(t, cfg, notifier, fk)

	// Pin the clock to the same day as sampleEvent (2026-04-09)
	// so the lock's ExpiresAt (end of that day) is still in the
	// future. Without this, running the test on a later date
	// causes CheckLocked to auto-release the expired lock, and
	// the second event re-evaluates instead of short-circuiting.
	p.Now = func() time.Time {
		return time.Date(2026, 4, 9, 14, 0, 0, 0, time.UTC)
	}

	// First event: triggers the initial kill breach.
	if err := p.Handle(context.Background(), sampleEvent("u1", "app", "main"), ""); err != nil {
		t.Fatal(err)
	}
	// Second event: should re-kill via the lock path, NOT via a
	// fresh verdict dispatch.
	if err := p.Handle(context.Background(), sampleEvent("u2", "app", "main"), ""); err != nil {
		t.Fatal(err)
	}

	// Killer called at least twice (once per event).
	if atomic.LoadInt32(&fk.killCalls) < 2 {
		t.Errorf("expected >= 2 Kill calls, got %d", fk.killCalls)
	}

	// Only ONE ntfy notification: the second event found the lock
	// and bailed before dispatch, so no second kill message.
	if len(*hdrs) != 1 {
		t.Errorf("expected 1 ntfy call, got %d", len(*hdrs))
	}
}

// TestHandleIdempotentOnDuplicateEvent verifies that the same
// UUID arriving twice (e.g. from a restart that re-scans the
// file) produces exactly one rollup row and does not re-fire
// notifications.
func TestHandleIdempotentOnDuplicateEvent(t *testing.T) {
	cfg := &budget.Config{
		Timezone: time.UTC,
		Rules: []budget.Rule{{
			Project: "*", Branch: "*",
			Period: budget.PeriodDaily,
			CapUSD: 100.00,
			Action: budget.ActionWarn,
		}},
	}
	notifier, _ := newNtfyRecorder(t, 200)
	p := buildPipeline(t, cfg, notifier, nil)

	e := sampleEvent("dup", "app", "main") // $15
	_ = p.Handle(context.Background(), e, "")
	_ = p.Handle(context.Background(), e, "")
	_ = p.Handle(context.Background(), e, "")

	rows, _ := p.DB.StatusByProject(context.Background(),
		time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 9, 23, 59, 59, 0, time.UTC))
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after 3 duplicate inserts, got %d", len(rows))
	}
	if rows[0].CostUSD != 15.00 {
		t.Errorf("CostUSD = %v, want 15.00 (idempotency violated)", rows[0].CostUSD)
	}
}

// TestHandleNtfyFailureIsNotFatal verifies that a ntfy server
// that returns 500 does not abort the handler. The event is
// still recorded, the lock is still written.
func TestHandleNtfyFailureIsNotFatal(t *testing.T) {
	cfg := &budget.Config{
		Timezone: time.UTC,
		Rules: []budget.Rule{{
			Project: "*", Branch: "*",
			Period: budget.PeriodDaily,
			CapUSD: 5.00,
			Action: budget.ActionKill,
		}},
	}
	notifier, _ := newNtfyRecorder(t, 500) // server always 500
	fk := &fakeKiller{byCwd: map[string][]int{"/home/u/app": {1}}}
	p := buildPipeline(t, cfg, notifier, fk)

	if err := p.Handle(context.Background(), sampleEvent("u1", "app", "main"), ""); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Verify side-effects still happened despite ntfy failure.
	if lk, _ := p.Enforcer.Locks.IsLocked("app", "main"); lk == nil {
		t.Error("lock should still be written on ntfy failure")
	}
	if len(fk.killed) == 0 {
		t.Error("kill should still happen on ntfy failure")
	}
}
