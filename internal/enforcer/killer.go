package enforcer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// Killer finds and terminates Claude Code processes by working
// directory. The interface exists so tests can inject a fake
// implementation instead of enumerating real processes and sending
// real signals.
type Killer interface {
	// FindByCWD returns the PIDs of every Claude process whose
	// working directory equals or is under `cwd`. Returns an empty
	// slice (not an error) when no matches are found.
	FindByCWD(ctx context.Context, cwd string) ([]int, error)

	// Kill sends SIGTERM to the given PIDs. Returns the list of
	// PIDs successfully signaled and an error describing any
	// failures. Individual failures do not abort iteration: killing
	// two of three matching processes is better than killing none.
	Kill(ctx context.Context, pids []int) ([]int, error)
}

// processNames is the set of executable basenames we consider
// "Claude Code" and are willing to SIGTERM. Kept intentionally
// tight: SIGTERMing an unrelated binary that happens to have
// "claude" in its name is a serious bug.
//
// If Anthropic ships a renamed binary in a future Claude Code
// release, add its basename here.
var processNames = map[string]bool{
	"claude": true,
}

// processInfo is the minimal view of a running process we need
// for FindByCWD: the PID, the executable basename, and the
// working directory. It is a package-private normalization over
// gopsutil's richer Process type so tests can inject fakes
// without depending on gopsutil or the host process table.
type processInfo struct {
	PID  int
	Name string
	CWD  string
}

// processSource abstracts process enumeration so FindByCWD can
// be tested with an injected list. The production implementation
// (gopsutilSource) wraps gopsutil; tests use fakeSource.
type processSource interface {
	Processes(ctx context.Context) ([]processInfo, error)
}

// gopsutilSource is the real-host implementation backed by
// gopsutil. Errors on individual process lookups are treated as
// "skip" so one permission-denied entry does not hide a valid
// match elsewhere.
type gopsutilSource struct{}

func (gopsutilSource) Processes(ctx context.Context) ([]processInfo, error) {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]processInfo, 0, len(procs))
	for _, p := range procs {
		name, err := p.NameWithContext(ctx)
		if err != nil {
			continue
		}
		cwd, _ := p.CwdWithContext(ctx)
		out = append(out, processInfo{
			PID:  int(p.Pid),
			Name: name,
			CWD:  cwd,
		})
	}
	return out, nil
}

// RealKiller uses a processSource to enumerate processes and
// syscall.Kill to signal them. The source is swappable so tests
// can run without a real process table.
type RealKiller struct {
	source processSource
}

// NewRealKiller returns a production Killer backed by gopsutil.
func NewRealKiller() *RealKiller {
	return &RealKiller{source: gopsutilSource{}}
}

// FindByCWD walks the process list, filters to recognized
// Claude executable names, and returns PIDs whose cwd equals or
// is under the requested cwd. An empty cwd on a process is
// treated as "unknown" and skipped.
func (k *RealKiller) FindByCWD(ctx context.Context, cwd string) ([]int, error) {
	procs, err := k.source.Processes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}

	var matches []int
	for _, p := range procs {
		if !processNames[p.Name] {
			continue
		}
		if p.CWD == "" {
			continue
		}
		if cwdMatches(p.CWD, cwd) {
			matches = append(matches, p.PID)
		}
	}
	return matches, nil
}

// Kill sends SIGTERM to every PID in the list. We never send
// SIGKILL: SIGTERM lets Claude Code flush its session log and
// exit cleanly. Users who really need a hard kill can
// `kill -9` the PID from the lockfile reason by hand.
//
// Individual signal failures (process already exited, permission
// denied) are collected into a single aggregated error, but
// successfully-signaled PIDs are still returned in the first slice.
func (k *RealKiller) Kill(_ context.Context, pids []int) ([]int, error) {
	var killed []int
	var failures []string

	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			failures = append(failures, fmt.Sprintf("pid %d: %v", pid, err))
			continue
		}
		killed = append(killed, pid)
	}

	if len(failures) > 0 {
		return killed, fmt.Errorf("SIGTERM failures: %s", strings.Join(failures, "; "))
	}
	return killed, nil
}

// cwdMatches reports whether `procCwd` is equal to `eventCwd` or
// a subdirectory of it. Prefix matching catches the "user cd'd
// into a subdir of the project before running claude" case, which
// is common with monorepos.
func cwdMatches(procCwd, eventCwd string) bool {
	if procCwd == eventCwd {
		return true
	}
	if !strings.HasSuffix(eventCwd, "/") {
		eventCwd += "/"
	}
	return strings.HasPrefix(procCwd, eventCwd)
}

// Enforcer bundles a LockStore and a Killer so the watcher only
// has to hold one thing. Construct via NewEnforcer or, for tests,
// by setting the fields directly with a fake Killer and a
// temp-dir LockStore.
type Enforcer struct {
	Locks  *LockStore
	Killer Killer
}

// NewEnforcer constructs an Enforcer using the default XDG lock
// directory and a real gopsutil-backed killer.
func NewEnforcer() (*Enforcer, error) {
	ls, err := NewLockStore()
	if err != nil {
		return nil, err
	}
	return &Enforcer{Locks: ls, Killer: NewRealKiller()}, nil
}

// HandleBreach writes a lock for (lock.Project, lock.Branch) and
// immediately SIGTERMs any matching Claude process found in cwd.
// Returns the list of killed PIDs (possibly empty) and any error
// from the lock write or kill phase.
//
// This is the "fresh breach" path: the watcher sees an event,
// evaluates rules, finds a kill-action breach, and calls this.
func (e *Enforcer) HandleBreach(ctx context.Context, lock Lock, cwd string) ([]int, error) {
	if e == nil || e.Locks == nil || e.Killer == nil {
		return nil, errors.New("enforcer not initialized")
	}
	if err := e.Locks.Acquire(lock); err != nil {
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	pids, err := e.Killer.FindByCWD(ctx, cwd)
	if err != nil {
		return nil, fmt.Errorf("find processes: %w", err)
	}
	if len(pids) == 0 {
		return nil, nil
	}
	return e.Killer.Kill(ctx, pids)
}

// CheckLocked is the "every event" path: before the watcher runs
// full budget evaluation, it calls this to see if (project,
// branch) is already locked. Returns:
//
//	(nil,  nil, nil)            not locked, proceed normally
//	(lock, killed, nil)          locked and active, processes killed (may be empty list)
//	(nil,  nil, nil)            locked but expired — auto-released, proceed normally
//
// The auto-release happens transparently: if Lock.Expired(now) is
// true, CheckLocked calls Release and reports "not locked".
func (e *Enforcer) CheckLocked(ctx context.Context, project, branch, cwd string, now time.Time) (*Lock, []int, error) {
	if e == nil || e.Locks == nil || e.Killer == nil {
		return nil, nil, errors.New("enforcer not initialized")
	}
	lk, err := e.Locks.IsLocked(project, branch)
	if err != nil {
		return nil, nil, err
	}
	if lk == nil {
		return nil, nil, nil
	}
	if lk.Expired(now) {
		if err := e.Locks.Release(project, branch); err != nil {
			return nil, nil, fmt.Errorf("release expired lock: %w", err)
		}
		return nil, nil, nil
	}
	pids, err := e.Killer.FindByCWD(ctx, cwd)
	if err != nil {
		return lk, nil, fmt.Errorf("find processes: %w", err)
	}
	if len(pids) == 0 {
		return lk, nil, nil
	}
	killed, err := e.Killer.Kill(ctx, pids)
	return lk, killed, err
}
