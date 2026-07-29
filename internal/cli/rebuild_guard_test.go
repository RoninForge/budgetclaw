package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RoninForge/budgetclaw/internal/db"
	"github.com/RoninForge/budgetclaw/internal/parser"
)

// `backfill --rebuild` wipes events and rollups and replays from the
// logs. Claude Code prunes those after roughly a month while the
// database keeps everything, so a rebuild can silently destroy spend
// that cannot be recovered from anywhere. These tests pin the guard.
//
// Measured on a real machine 2026-07-27: the database held rollups back
// to 05-13 while the logs only reached 06-26. A rebuild would have
// discarded six weeks.

// logLine renders one billable assistant JSONL line on a given day.
func logLine(uuid, day string) string {
	return `{"type":"assistant","uuid":"` + uuid + `","sessionId":"s1","timestamp":"` +
		day + `T09:00:00Z","cwd":"/tmp/proj","gitBranch":"main","message":{"id":"m-` + uuid +
		`","model":"claude-opus-4-8","usage":{"input_tokens":100,"output_tokens":50}}}`
}

// writeLogs writes a single session file containing one event per day.
func writeLogs(t *testing.T, dir string, days ...string) {
	t.Helper()
	lines := make([]string, 0, len(days))
	for i, d := range days {
		lines = append(lines, logLine(string(rune('a'+i)), d))
	}
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"),
		[]byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedDBDay inserts one priced event on the given day, so the database
// holds history reaching back that far.
func seedDBDay(t *testing.T, day string) {
	t.Helper()
	ts, err := time.Parse("2006-01-02", day)
	if err != nil {
		t.Fatal(err)
	}
	store, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	e := &parser.Event{
		UUID: "seed-" + day, SessionID: "seed", Timestamp: ts,
		CWD: "/tmp/proj", Project: "proj", GitBranch: "main",
		Model: "claude-opus-4-8", ServiceTier: "standard",
		InputTokens: 100, OutputTokens: 50,
	}
	if err := store.Insert(context.Background(), e, 1.00); err != nil {
		t.Fatal(err)
	}
}

// TestRebuildRefusedWhenLogsCannotReplayHistory is the incident, as a
// test: the database reaches further back than the logs, so a rebuild
// would destroy the difference.
func TestRebuildRefusedWhenLogsCannotReplayHistory(t *testing.T) {
	setupXDG(t)
	dir := t.TempDir()

	seedDBDay(t, "2026-05-13")
	writeLogs(t, dir, "2026-06-26", "2026-06-27")

	stdout, _, err := execCmd(t, "backfill", "--rebuild", "--dir", dir)
	if err == nil {
		t.Fatalf("expected --rebuild to be refused, got success. stdout: %q", stdout)
	}

	msg := err.Error()
	for _, want := range []string{
		"refusing --rebuild",
		"2026-05-13", // oldest in database
		"2026-06-26", // oldest in logs
		"44 days",    // 05-13 to 06-26
		"--rebuild --force",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message missing %q:\n%s", want, msg)
		}
	}

	// Crucially, the database must be untouched by a refused rebuild.
	assertDBStillHas(t, "2026-05-13")
}

// TestRebuildForceOverridesGuard verifies the escape hatch works and
// says plainly what it is discarding.
func TestRebuildForceOverridesGuard(t *testing.T) {
	setupXDG(t)
	dir := t.TempDir()

	seedDBDay(t, "2026-05-13")
	writeLogs(t, dir, "2026-06-26")

	stdout, _, err := execCmd(t, "backfill", "--rebuild", "--force", "--dir", dir)
	if err != nil {
		t.Fatalf("--force should proceed, got %v", err)
	}
	if !strings.Contains(stdout, "discarding database history before 2026-06-26") {
		t.Errorf("expected an explicit discard warning, got %q", stdout)
	}
	if !strings.Contains(stdout, "wiped events + rollups") {
		t.Errorf("expected the wipe to happen under --force, got %q", stdout)
	}

	// The old day is gone, which is what the user asked for.
	store, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	oldest, err := store.OldestRollupDay(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if oldest != "2026-06-26" {
		t.Errorf("oldest day after forced rebuild = %q, want 2026-06-26", oldest)
	}
}

// TestRebuildAllowedWhenLogsCoverHistory checks the safe case proceeds
// and reports that the check actually ran.
func TestRebuildAllowedWhenLogsCoverHistory(t *testing.T) {
	setupXDG(t)
	dir := t.TempDir()

	seedDBDay(t, "2026-06-27")
	writeLogs(t, dir, "2026-06-26", "2026-06-27")

	stdout, _, err := execCmd(t, "backfill", "--rebuild", "--dir", dir)
	if err != nil {
		t.Fatalf("rebuild should be allowed when logs cover history: %v", err)
	}
	if !strings.Contains(stdout, "Safe to replay") {
		t.Errorf("expected the coverage check to report, got %q", stdout)
	}
	if !strings.Contains(stdout, "wiped events + rollups") {
		t.Errorf("expected the wipe to proceed, got %q", stdout)
	}
}

// TestRebuildOnEmptyDatabaseIsAllowed covers a fresh install: there is
// no history to lose, so the guard must not get in the way.
func TestRebuildOnEmptyDatabaseIsAllowed(t *testing.T) {
	setupXDG(t)
	dir := t.TempDir()
	writeLogs(t, dir, "2026-06-26")

	if _, _, err := execCmd(t, "backfill", "--rebuild", "--dir", dir); err != nil {
		t.Fatalf("rebuild on an empty database should be allowed: %v", err)
	}
}

// TestRebuildRefusedWhenLogsHoldNoEvents covers the worst case: a
// rebuild that would wipe everything and replay nothing.
func TestRebuildRefusedWhenLogsHoldNoEvents(t *testing.T) {
	setupXDG(t)
	dir := t.TempDir()

	seedDBDay(t, "2026-05-13")
	// A file with no billable lines at all.
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"),
		[]byte(`{"type":"user","uuid":"u1"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := execCmd(t, "backfill", "--rebuild", "--dir", dir)
	if err == nil {
		t.Fatal("expected refusal when the logs hold no billable events")
	}
	if !strings.Contains(err.Error(), "none found") {
		t.Errorf("expected the message to say no log events were found, got:\n%s", err)
	}
	assertDBStillHas(t, "2026-05-13")
}

// TestRebuildMissingLogDirDoesNotWipe covers the ordering bug this
// change also fixes: the log directory is opened before anything
// destructive, so a rebuild pointed at a missing directory can no
// longer wipe the database and then find nothing to replay.
func TestRebuildMissingLogDirDoesNotWipe(t *testing.T) {
	setupXDG(t)
	seedDBDay(t, "2026-05-13")

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	stdout, _, err := execCmd(t, "backfill", "--rebuild", "--dir", missing)
	if err != nil {
		t.Fatalf("missing dir should report cleanly, got %v", err)
	}
	if !strings.Contains(stdout, "nothing to backfill") {
		t.Errorf("expected the missing-directory notice, got %q", stdout)
	}
	if strings.Contains(stdout, "wiped") {
		t.Error("a missing log directory must never trigger the wipe")
	}
	assertDBStillHas(t, "2026-05-13")
}

// TestForceWithoutRebuildIsRejected keeps the flag honest: --force has
// no meaning on its own and should not look like it does something.
func TestForceWithoutRebuildIsRejected(t *testing.T) {
	setupXDG(t)
	dir := t.TempDir()

	_, _, err := execCmd(t, "backfill", "--force", "--dir", dir)
	if err == nil {
		t.Fatal("expected --force without --rebuild to be rejected")
	}
	if !strings.Contains(err.Error(), "only applies to --rebuild") {
		t.Errorf("unexpected error: %v", err)
	}
}

// assertDBStillHas fails when the database no longer reaches back to
// the given day, which is the thing the guard exists to prevent.
func assertDBStillHas(t *testing.T, day string) {
	t.Helper()
	store, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	oldest, err := store.OldestRollupDay(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if oldest != day {
		t.Errorf("database history changed: oldest day is %q, want %q", oldest, day)
	}
}
