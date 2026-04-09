package enforcer

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// fakeKiller is an in-memory Killer for tests. It records every
// Kill call and lets tests preload the cwd → pids map that
// FindByCWD will consult.
type fakeKiller struct {
	byCwd    map[string][]int
	findErr  error
	killErr  error
	killed   [][]int // every Kill call is recorded
}

func (f *fakeKiller) FindByCWD(_ context.Context, cwd string) ([]int, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.byCwd[cwd], nil
}

func (f *fakeKiller) Kill(_ context.Context, pids []int) ([]int, error) {
	f.killed = append(f.killed, pids)
	if f.killErr != nil {
		return nil, f.killErr
	}
	return pids, nil
}

// newTestStore creates a lock store in a temp directory. The
// directory is cleaned up automatically by t.Cleanup.
func newTestStore(t *testing.T) *LockStore {
	t.Helper()
	s, err := NewLockStoreAt(t.TempDir())
	if err != nil {
		t.Fatalf("NewLockStoreAt: %v", err)
	}
	return s
}

// TestLockStoreAcquireAndIsLocked verifies the basic write/read
// cycle, including the LockedAt default behavior.
func TestLockStoreAcquireAndIsLocked(t *testing.T) {
	s := newTestStore(t)

	lk := Lock{
		Project:    "myapp",
		Branch:     "main",
		Period:     "daily",
		Reason:     "daily $5 cap breached at $5.25",
		CapUSD:     5.00,
		CurrentUSD: 5.25,
	}
	if err := s.Acquire(lk); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	got, err := s.IsLocked("myapp", "main")
	if err != nil {
		t.Fatalf("IsLocked: %v", err)
	}
	if got == nil {
		t.Fatal("expected lock, got nil")
	}
	if got.Reason != lk.Reason {
		t.Errorf("Reason = %q, want %q", got.Reason, lk.Reason)
	}
	if got.CurrentUSD != 5.25 {
		t.Errorf("CurrentUSD = %v, want 5.25", got.CurrentUSD)
	}
	if got.LockedAt.IsZero() {
		t.Error("LockedAt should have been set to time.Now()")
	}
}

// TestLockStoreIsLockedNotExist returns (nil, nil) for unknown locks.
func TestLockStoreIsLockedNotExist(t *testing.T) {
	s := newTestStore(t)
	got, err := s.IsLocked("ghost", "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

// TestLockStoreRelease removes a lock, then a second Release is
// a no-op (no error on missing).
func TestLockStoreRelease(t *testing.T) {
	s := newTestStore(t)
	lk := Lock{Project: "myapp", Branch: "main"}

	if err := s.Acquire(lk); err != nil {
		t.Fatal(err)
	}
	if err := s.Release("myapp", "main"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := s.Release("myapp", "main"); err != nil {
		t.Errorf("second Release should be no-op, got %v", err)
	}

	got, _ := s.IsLocked("myapp", "main")
	if got != nil {
		t.Errorf("expected nil after release, got %+v", got)
	}
}

// TestLockStoreBranchWithSlashes verifies URL-escape preserves
// "feature/login" as a legal lockfile name.
func TestLockStoreBranchWithSlashes(t *testing.T) {
	s := newTestStore(t)
	lk := Lock{Project: "myapp", Branch: "feature/login"}
	if err := s.Acquire(lk); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	got, err := s.IsLocked("myapp", "feature/login")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Branch != "feature/login" {
		t.Errorf("expected feature/login lock, got %+v", got)
	}
}

// TestLockStoreAcquireOverwrites verifies that re-acquiring the
// same (project, branch) updates the lock rather than duplicating.
func TestLockStoreAcquireOverwrites(t *testing.T) {
	s := newTestStore(t)
	lk := Lock{Project: "myapp", Branch: "main", CurrentUSD: 5.00}
	if err := s.Acquire(lk); err != nil {
		t.Fatal(err)
	}

	lk.CurrentUSD = 5.50
	if err := s.Acquire(lk); err != nil {
		t.Fatal(err)
	}

	got, _ := s.IsLocked("myapp", "main")
	if got.CurrentUSD != 5.50 {
		t.Errorf("expected overwrite to 5.50, got %v", got.CurrentUSD)
	}
}

// TestLockStoreAcquireEmptyFields rejects lock writes with missing
// identity fields — otherwise we'd get a filename like "__.lock"
// that could collide across unrelated callers.
func TestLockStoreAcquireEmptyFields(t *testing.T) {
	s := newTestStore(t)
	if err := s.Acquire(Lock{Branch: "main"}); err == nil {
		t.Error("expected error on empty project")
	}
	if err := s.Acquire(Lock{Project: "x"}); err == nil {
		t.Error("expected error on empty branch")
	}
}

// TestLockStoreList returns every active lock and skips garbage
// files in the directory.
func TestLockStoreList(t *testing.T) {
	s := newTestStore(t)

	for i, bn := range []string{"main", "feature/x", "rebirth"} {
		lk := Lock{
			Project:    "myapp",
			Branch:     bn,
			CurrentUSD: float64(i + 1),
		}
		if err := s.Acquire(lk); err != nil {
			t.Fatal(err)
		}
	}

	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("expected 3 locks, got %d: %+v", len(list), list)
	}
}

// TestLockStorePruneRemovesExpired verifies auto-cleanup of
// locks whose period has rolled over.
func TestLockStorePruneRemovesExpired(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	// Two locks: one expired, one still active.
	if err := s.Acquire(Lock{
		Project: "old", Branch: "main",
		ExpiresAt: now.Add(-1 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire(Lock{
		Project: "current", Branch: "main",
		ExpiresAt: now.Add(1 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	removed, err := s.Prune(now)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}

	if lk, _ := s.IsLocked("old", "main"); lk != nil {
		t.Error("expired lock should have been pruned")
	}
	if lk, _ := s.IsLocked("current", "main"); lk == nil {
		t.Error("active lock should have survived Prune")
	}
}

// TestLockExpired exercises the Expired helper in isolation.
func TestLockExpired(t *testing.T) {
	now := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		expires time.Time
		want    bool
	}{
		{"zero means never expires", time.Time{}, false},
		{"future is not expired", now.Add(1 * time.Hour), false},
		{"past is expired", now.Add(-1 * time.Hour), true},
		{"exactly now is not expired", now, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lk := Lock{ExpiresAt: c.expires}
			if got := lk.Expired(now); got != c.want {
				t.Errorf("Expired(%v) = %v, want %v", c.expires, got, c.want)
			}
		})
	}
}

// TestLockStoreCorruptFile surfaces parse errors for a corrupt
// JSON lock file but does not crash the process.
func TestLockStoreCorruptFile(t *testing.T) {
	s := newTestStore(t)

	// Write an invalid lock file by hand.
	path := s.Dir() + "/" + lockFilename("myapp", "main")
	if err := writeString(path, "{not-json"); err != nil {
		t.Fatal(err)
	}

	_, err := s.IsLocked("myapp", "main")
	if err == nil {
		t.Error("expected error for corrupt lock file")
	}
}

// TestCwdMatches covers the prefix-match semantics used by the
// RealKiller. Exercised here because it's package-level and we
// want confidence before we trust it with real SIGTERMs.
func TestCwdMatches(t *testing.T) {
	cases := []struct {
		procCwd  string
		eventCwd string
		want     bool
	}{
		{"/home/u/myapp", "/home/u/myapp", true},
		{"/home/u/myapp/cmd", "/home/u/myapp", true},
		{"/home/u/myapp/cmd/claude", "/home/u/myapp", true},
		{"/home/u/other", "/home/u/myapp", false},
		{"/home/u/myapp-other", "/home/u/myapp", false}, // prefix but not subdir
		{"/home/u/myapp/", "/home/u/myapp", true},
		{"/", "/home/u/myapp", false},
	}
	for _, c := range cases {
		got := cwdMatches(c.procCwd, c.eventCwd)
		if got != c.want {
			t.Errorf("cwdMatches(%q, %q) = %v, want %v",
				c.procCwd, c.eventCwd, got, c.want)
		}
	}
}

// TestEnforcerHandleBreach wires a LockStore (real, temp dir) and
// a fakeKiller and verifies the happy-path sequence: lock is
// persisted, then processes are killed.
func TestEnforcerHandleBreach(t *testing.T) {
	ls := newTestStore(t)
	fk := &fakeKiller{byCwd: map[string][]int{
		"/home/u/myapp": {1001, 1002},
	}}
	e := &Enforcer{Locks: ls, Killer: fk}

	lock := Lock{
		Project: "myapp", Branch: "main",
		Period: "daily", Reason: "daily $5 breach",
		CapUSD: 5, CurrentUSD: 5.10,
	}

	killed, err := e.HandleBreach(context.Background(), lock, "/home/u/myapp")
	if err != nil {
		t.Fatalf("HandleBreach: %v", err)
	}
	if len(killed) != 2 {
		t.Errorf("killed = %v, want [1001 1002]", killed)
	}

	// Lock must be persisted.
	lk, _ := ls.IsLocked("myapp", "main")
	if lk == nil {
		t.Fatal("expected lock to be written")
	}
	if lk.CurrentUSD != 5.10 {
		t.Errorf("CurrentUSD = %v", lk.CurrentUSD)
	}

	// Kill must have happened exactly once.
	if len(fk.killed) != 1 {
		t.Errorf("expected 1 Kill call, got %d", len(fk.killed))
	}
}

// TestEnforcerHandleBreachNoProcesses verifies that a lock is
// still written even if no claude processes are currently running
// in the cwd. The lock catches the next relaunch.
func TestEnforcerHandleBreachNoProcesses(t *testing.T) {
	ls := newTestStore(t)
	fk := &fakeKiller{byCwd: map[string][]int{}}
	e := &Enforcer{Locks: ls, Killer: fk}

	killed, err := e.HandleBreach(context.Background(),
		Lock{Project: "myapp", Branch: "main"}, "/home/u/myapp")
	if err != nil {
		t.Fatalf("HandleBreach: %v", err)
	}
	if len(killed) != 0 {
		t.Errorf("killed = %v, want []", killed)
	}
	if lk, _ := ls.IsLocked("myapp", "main"); lk == nil {
		t.Error("lock should still be written")
	}
}

// TestEnforcerCheckLockedNotLocked returns (nil, nil, nil) for
// the common "nothing locked, proceed normally" case.
func TestEnforcerCheckLockedNotLocked(t *testing.T) {
	ls := newTestStore(t)
	fk := &fakeKiller{}
	e := &Enforcer{Locks: ls, Killer: fk}

	lk, killed, err := e.CheckLocked(context.Background(),
		"myapp", "main", "/home/u/myapp", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if lk != nil || killed != nil {
		t.Errorf("expected all-nil, got lk=%+v killed=%v", lk, killed)
	}
	if len(fk.killed) != 0 {
		t.Error("fake killer should not have been invoked")
	}
}

// TestEnforcerCheckLockedActive re-kills a running process when
// the lock is still in force.
func TestEnforcerCheckLockedActive(t *testing.T) {
	ls := newTestStore(t)
	now := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)

	err := ls.Acquire(Lock{
		Project: "myapp", Branch: "main",
		ExpiresAt: now.Add(1 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	fk := &fakeKiller{byCwd: map[string][]int{
		"/home/u/myapp": {2001},
	}}
	e := &Enforcer{Locks: ls, Killer: fk}

	lk, killed, err := e.CheckLocked(context.Background(),
		"myapp", "main", "/home/u/myapp", now)
	if err != nil {
		t.Fatal(err)
	}
	if lk == nil {
		t.Fatal("expected active lock to be returned")
	}
	if len(killed) != 1 || killed[0] != 2001 {
		t.Errorf("killed = %v, want [2001]", killed)
	}
}

// TestEnforcerCheckLockedExpired auto-releases a lock whose
// period has rolled over and returns "not locked".
func TestEnforcerCheckLockedExpired(t *testing.T) {
	ls := newTestStore(t)
	now := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	err := ls.Acquire(Lock{
		Project: "myapp", Branch: "main",
		ExpiresAt: now.Add(-1 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	fk := &fakeKiller{byCwd: map[string][]int{
		"/home/u/myapp": {3001},
	}}
	e := &Enforcer{Locks: ls, Killer: fk}

	lk, killed, err := e.CheckLocked(context.Background(),
		"myapp", "main", "/home/u/myapp", now)
	if err != nil {
		t.Fatal(err)
	}
	if lk != nil {
		t.Errorf("expected expired → nil, got %+v", lk)
	}
	if killed != nil {
		t.Error("expired lock should not trigger kill")
	}
	// Lock file must have been released.
	if got, _ := ls.IsLocked("myapp", "main"); got != nil {
		t.Error("expired lock should have been released from disk")
	}
}

// TestEnforcerCheckLockedFindError surfaces the underlying
// Killer.FindByCWD error instead of silently swallowing it.
func TestEnforcerCheckLockedFindError(t *testing.T) {
	ls := newTestStore(t)
	now := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	if err := ls.Acquire(Lock{
		Project: "myapp", Branch: "main",
		ExpiresAt: now.Add(1 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("proc-lookup failed")
	fk := &fakeKiller{findErr: sentinel}
	e := &Enforcer{Locks: ls, Killer: fk}

	_, _, err := e.CheckLocked(context.Background(), "myapp", "main", "/home/u/myapp", now)
	if err == nil || !errors.Is(err, sentinel) {
		t.Errorf("expected wrapped sentinel, got %v", err)
	}
}

// TestEnforcerNilReceiver protects callers from constructing an
// Enforcer with missing fields.
func TestEnforcerNilReceiver(t *testing.T) {
	var e *Enforcer
	if _, err := e.HandleBreach(context.Background(), Lock{}, ""); err == nil {
		t.Error("expected error on nil enforcer")
	}
	if _, _, err := e.CheckLocked(context.Background(), "p", "b", "/", time.Now()); err == nil {
		t.Error("expected error on nil enforcer")
	}
}

// writeString is a tiny test helper that writes a string to a file.
func writeString(path, s string) error {
	return os.WriteFile(path, []byte(s), 0o644)
}

// TestNewRealKillerNonNil is a trivial smoke test for the
// constructor so the happy path is covered.
func TestNewRealKillerNonNil(t *testing.T) {
	if NewRealKiller() == nil {
		t.Error("NewRealKiller returned nil")
	}
}

// TestNewEnforcerDefaultXDG verifies the XDG-backed constructor
// produces a fully wired Enforcer when XDG_DATA_HOME points at a
// temp dir. We override XDG so the test never touches the user's
// real lock directory.
func TestNewEnforcerDefaultXDG(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	e, err := NewEnforcer()
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	if e == nil || e.Locks == nil || e.Killer == nil {
		t.Errorf("incomplete enforcer: %+v", e)
	}
}

// TestRealKillerKillsSpawnedProcess exercises the real SIGTERM
// path without touching any claude processes: we spawn our own
// `sleep 60`, SIGTERM it, and verify it exits. POSIX-only.
func TestRealKillerKillsSpawnedProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals only")
	}

	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}

	// Safety net: SIGKILL and reap if the test fails before
	// normal cleanup.
	done := false
	t.Cleanup(func() {
		if !done {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	k := NewRealKiller()
	killed, err := k.Kill(context.Background(), []int{cmd.Process.Pid})
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if len(killed) != 1 || killed[0] != cmd.Process.Pid {
		t.Errorf("killed = %v, want [%d]", killed, cmd.Process.Pid)
	}

	// Wait for the signaled process to actually exit.
	waitErr := cmd.Wait()
	done = true

	// SIGTERM causes Wait to return a non-nil *exec.ExitError whose
	// underlying WaitStatus reports Signal() == SIGTERM. Anything
	// else means our Kill did not land.
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("expected *exec.ExitError, got %T: %v", waitErr, waitErr)
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Errorf("expected SIGTERM exit, got %v", waitErr)
	}
}

// TestRealKillerKillMissingPID verifies that signaling a
// nonexistent PID returns an aggregated error (not a panic).
func TestRealKillerKillMissingPID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals only")
	}

	k := NewRealKiller()
	_, err := k.Kill(context.Background(), []int{999_999_999})
	if err == nil {
		t.Error("expected error for nonexistent PID")
	}
}

// fakeSource is a deterministic processSource for testing
// FindByCWD without touching the real host process table.
type fakeSource struct {
	procs []processInfo
	err   error
}

func (f fakeSource) Processes(_ context.Context) ([]processInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.procs, nil
}

// TestRealKillerFindByCWDWithFakeSource is the unit test for
// FindByCWD's filtering logic. It injects a fake process list
// and verifies:
//  1. Only processes whose name is in processNames are considered
//  2. Processes with empty cwd are skipped
//  3. Exact and subdir cwd matches are both returned
//  4. Non-matching cwds are excluded
func TestRealKillerFindByCWDWithFakeSource(t *testing.T) {
	k := &RealKiller{source: fakeSource{procs: []processInfo{
		// Exact cwd match — should return.
		{PID: 100, Name: "claude", CWD: "/home/u/myapp"},
		// Subdir match — should return.
		{PID: 101, Name: "claude", CWD: "/home/u/myapp/cmd"},
		// Different project — should not return.
		{PID: 102, Name: "claude", CWD: "/home/u/other"},
		// Wrong name — should not return.
		{PID: 103, Name: "bash", CWD: "/home/u/myapp"},
		// Empty cwd — should be skipped.
		{PID: 104, Name: "claude", CWD: ""},
		// Prefix but not subdir — should not return.
		{PID: 105, Name: "claude", CWD: "/home/u/myapp-other"},
	}}}

	pids, err := k.FindByCWD(context.Background(), "/home/u/myapp")
	if err != nil {
		t.Fatalf("FindByCWD: %v", err)
	}

	wantSet := map[int]bool{100: true, 101: true}
	if len(pids) != len(wantSet) {
		t.Fatalf("got %d PIDs, want %d: %v", len(pids), len(wantSet), pids)
	}
	for _, p := range pids {
		if !wantSet[p] {
			t.Errorf("unexpected PID %d in result", p)
		}
	}
}

// TestRealKillerFindByCWDSourceError surfaces errors from the
// underlying process source.
func TestRealKillerFindByCWDSourceError(t *testing.T) {
	sentinel := errors.New("kernel mood swing")
	k := &RealKiller{source: fakeSource{err: sentinel}}
	_, err := k.FindByCWD(context.Background(), "/")
	if !errors.Is(err, sentinel) {
		t.Errorf("expected wrapped sentinel, got %v", err)
	}
}

// TestGopsutilSourceSmoke exercises the real gopsutil-backed
// source with no assertions beyond "it doesn't error and returns
// at least one process". This verifies our gopsutil wiring on
// the host without depending on any specific process being
// present.
func TestGopsutilSourceSmoke(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("not exercising Windows process table here")
	}
	procs, err := gopsutilSource{}.Processes(context.Background())
	if err != nil {
		t.Fatalf("gopsutilSource.Processes: %v", err)
	}
	if len(procs) == 0 {
		t.Error("expected at least one process on the host")
	}
}
