// Package enforcer implements the two sides of budget-breach
// enforcement for budgetclaw:
//
//  1. A filesystem-backed LockStore that persists "this
//     (project, branch) is in breach" across process restarts,
//     so a user who accidentally re-launches claude cannot
//     sidestep a cap they already hit.
//
//  2. A Killer interface (with a gopsutil-backed RealKiller) that
//     finds Claude Code processes whose working directory matches
//     the breached project and sends SIGTERM to them.
//
// The two concerns live in one package because the watcher always
// wires them together: on a kill breach, write the lock AND kill;
// on every subsequent event for a locked (project, branch), re-kill.
// Splitting them into separate packages would just force callers
// to hold two things together at every call site.
//
// The package has a single external dependency (gopsutil) and does
// not import any other budgetclaw package except `paths` for XDG
// resolution. That keeps test dependencies minimal and lets
// watcher code own the wiring.
package enforcer

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RoninForge/budgetclaw/internal/paths"
)

// Lock is one active budget-breach lock. It is both written to
// disk (as JSON in the locks dir) and returned in-memory by the
// query methods.
type Lock struct {
	Project    string    `json:"project"`
	Branch     string    `json:"branch"`
	Period     string    `json:"period"` // "daily" | "weekly" | "monthly"
	Reason     string    `json:"reason"`
	CapUSD     float64   `json:"cap_usd"`
	CurrentUSD float64   `json:"current_usd"`
	LockedAt   time.Time `json:"locked_at"`
	ExpiresAt  time.Time `json:"expires_at"` // auto-unlock at this time
}

// Expired reports whether the lock's period has rolled over and
// the lock should be treated as released. Callers are responsible
// for actually deleting the file (see LockStore.Prune).
//
// A zero ExpiresAt means "no auto-expire" — the lock only clears
// on explicit Release.
func (l Lock) Expired(now time.Time) bool {
	if l.ExpiresAt.IsZero() {
		return false
	}
	return now.After(l.ExpiresAt)
}

// LockStore is a filesystem-backed set of active locks, one file
// per (project, branch) under $XDG_DATA_HOME/budgetclaw/locks/.
//
// The store is safe to use from a single process. Multi-process
// coordination is handled by the filesystem (atomic rename on
// write, ENOENT on read of a released lock). The budgetclaw
// watcher is single-process by design; two watchers racing on the
// same lock directory is not a supported configuration.
type LockStore struct {
	dir string
}

// NewLockStore creates a store at the default XDG location and
// ensures the directory exists.
func NewLockStore() (*LockStore, error) {
	dir, err := paths.DataDir()
	if err != nil {
		return nil, fmt.Errorf("resolve data dir: %w", err)
	}
	return NewLockStoreAt(filepath.Join(dir, "locks"))
}

// NewLockStoreAt creates a store at an explicit directory. Used
// by tests that want an isolated temp dir.
func NewLockStoreAt(dir string) (*LockStore, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create lock dir %s: %w", dir, err)
	}
	return &LockStore{dir: dir}, nil
}

// Dir returns the on-disk directory. Used by `budgetclaw locks path`
// and similar diagnostic commands.
func (s *LockStore) Dir() string { return s.dir }

// Acquire writes a lock file for (Project, Branch). If one already
// exists, it is overwritten — the latest breach metadata is what
// matters. If LockedAt is zero, it is set to time.Now().UTC().
//
// The write is atomic via the classic temp-file-plus-rename dance
// so a crash mid-write cannot leave a half-written JSON file on
// disk (a half-written file would cause IsLocked to return a
// corrupted-lock error).
func (s *LockStore) Acquire(l Lock) error {
	if l.Project == "" || l.Branch == "" {
		return errors.New("lock requires non-empty project and branch")
	}
	if l.LockedAt.IsZero() {
		l.LockedAt = time.Now().UTC()
	}

	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal lock: %w", err)
	}

	final := filepath.Join(s.dir, lockFilename(l.Project, l.Branch))
	tmp := final + ".tmp"

	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write temp lock: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename lock: %w", err)
	}
	return nil
}

// Release removes the lock for (project, branch). Releasing a
// non-existent lock is not an error — the desired end state
// (no lock) is achieved either way.
func (s *LockStore) Release(project, branch string) error {
	path := filepath.Join(s.dir, lockFilename(project, branch))
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// IsLocked returns the active Lock for (project, branch), or
// (nil, nil) if no lock exists. A corrupt lock file (invalid JSON)
// produces an error — the caller should probably delete the file
// and treat it as unlocked, but we surface the error so
// debugging is possible.
func (s *LockStore) IsLocked(project, branch string) (*Lock, error) {
	path := filepath.Join(s.dir, lockFilename(project, branch))
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lk Lock
	if err := json.Unmarshal(data, &lk); err != nil {
		return nil, fmt.Errorf("corrupt lock file %s: %w", path, err)
	}
	return &lk, nil
}

// List returns every active lock file. Unreadable or corrupt
// entries are silently skipped — a single bad file should not
// break `budgetclaw locks list`. Returned locks are in filesystem
// order (no sort guarantee).
func (s *LockStore) List() ([]Lock, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var out []Lock
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var lk Lock
		if err := json.Unmarshal(data, &lk); err != nil {
			continue
		}
		out = append(out, lk)
	}
	return out, nil
}

// Prune removes every lock whose ExpiresAt has passed relative to
// `now`. Returns the number of locks removed. Useful as a
// lightweight cleanup run by the watcher on startup or on a timer.
func (s *LockStore) Prune(now time.Time) (int, error) {
	all, err := s.List()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, lk := range all {
		if !lk.Expired(now) {
			continue
		}
		if err := s.Release(lk.Project, lk.Branch); err != nil {
			return count, fmt.Errorf("release expired lock for %s/%s: %w",
				lk.Project, lk.Branch, err)
		}
		count++
	}
	return count, nil
}

// lockFilename returns a deterministic filesystem-safe filename
// for a (project, branch) pair. Both components are URL-escaped
// so a branch like "feature/login" becomes "feature%2Flogin" in
// the filename. Separator is "__" (two underscores), which
// url.PathEscape will not produce for normal branch or project
// names.
func lockFilename(project, branch string) string {
	return url.PathEscape(project) + "__" + url.PathEscape(branch) + ".lock"
}
