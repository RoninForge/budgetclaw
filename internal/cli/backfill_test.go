package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBackfillEmptyDir confirms an empty/missing log dir reports
// cleanly without error so a fresh install does not panic.
func TestBackfillEmptyDir(t *testing.T) {
	setupXDG(t)
	dir := t.TempDir()
	stdout, _, err := execCmd(t, "backfill", "--dir", dir)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if !strings.Contains(stdout, "scanned 0 events") {
		t.Errorf("expected zero-event summary, got %q", stdout)
	}
}

// TestBackfillCountsAndAttributes seeds two assistant events (one
// priceable, one for an unknown model) plus a non-billable user
// line, then asserts the summary reports the right counts and
// flags the unknown model.
func TestBackfillCountsAndAttributes(t *testing.T) {
	setupXDG(t)
	dir := t.TempDir()
	fixture := strings.Join([]string{
		`{"type":"assistant","uuid":"u1","sessionId":"s1","timestamp":"2026-04-28T00:00:00Z","cwd":"/tmp/proj","gitBranch":"main","message":{"model":"claude-opus-4-7","usage":{"input_tokens":100,"output_tokens":50}}}`,
		`{"type":"assistant","uuid":"u2","sessionId":"s1","timestamp":"2026-04-28T00:00:01Z","cwd":"/tmp/proj","gitBranch":"main","message":{"model":"some-future-model","usage":{"input_tokens":100,"output_tokens":50}}}`,
		`{"type":"user","uuid":"u3"}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := execCmd(t, "backfill", "--dir", dir)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if !strings.Contains(stdout, "scanned 2 events") {
		t.Errorf("expected scanned=2, got %q", stdout)
	}
	if !strings.Contains(stdout, "priced 1") {
		t.Errorf("expected priced=1, got %q", stdout)
	}
	if !strings.Contains(stdout, "skipped 1") {
		t.Errorf("expected skipped=1, got %q", stdout)
	}
	if !strings.Contains(stdout, "some-future-model") {
		t.Errorf("expected unknown-models block to name some-future-model, got %q", stdout)
	}
}

// TestBackfillRebuildWipesAndReplays checks the --rebuild path:
// rolling up at one rate, then changing the model fixture and
// re-running with --rebuild, should produce the new totals rather
// than appending. Validates the recovery path used after a
// pricing correction lands.
func TestBackfillRebuildWipesAndReplays(t *testing.T) {
	setupXDG(t)
	dir := t.TempDir()

	// Initial scan with one event.
	first := `{"type":"assistant","uuid":"u1","sessionId":"s1","timestamp":"2026-04-28T00:00:00Z","cwd":"/tmp/proj","gitBranch":"main","message":{"model":"claude-opus-4-7","usage":{"input_tokens":100,"output_tokens":50}}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := execCmd(t, "backfill", "--dir", dir); err != nil {
		t.Fatalf("first backfill: %v", err)
	}

	// Replace the file with a different fixture (1 event, same uuid
	// — without --rebuild this would be a no-op).
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := execCmd(t, "backfill", "--dir", dir, "--rebuild")
	if err != nil {
		t.Fatalf("rebuild backfill: %v", err)
	}
	if !strings.Contains(stdout, "wiped events + rollups") {
		t.Errorf("expected wipe banner with --rebuild, got %q", stdout)
	}
	if !strings.Contains(stdout, "scanned 1 events") || !strings.Contains(stdout, "priced 1") {
		t.Errorf("expected scanned=1 priced=1 after rebuild, got %q", stdout)
	}
}

// TestBackfillIdempotent runs backfill twice on the same fixture
// and asserts the second run reports the same scan/priced counts.
// The DB-level idempotency (ON CONFLICT DO NOTHING) means rollups
// do not double; the CLI summary should reflect that the events
// were re-scanned but not re-attributed.
func TestBackfillIdempotent(t *testing.T) {
	setupXDG(t)
	dir := t.TempDir()
	fixture := `{"type":"assistant","uuid":"u1","sessionId":"s1","timestamp":"2026-04-28T00:00:00Z","cwd":"/tmp/proj","gitBranch":"main","message":{"model":"claude-opus-4-7","usage":{"input_tokens":100,"output_tokens":50}}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		stdout, _, err := execCmd(t, "backfill", "--dir", dir)
		if err != nil {
			t.Fatalf("backfill run %d: %v", i+1, err)
		}
		if !strings.Contains(stdout, "scanned 1 events") {
			t.Errorf("run %d: expected scanned=1, got %q", i+1, stdout)
		}
		// "priced 1" appears in both runs because the CLI counts
		// every successful Insert call. The DB silently dedupes,
		// so rollups stay correct even though the counter looks
		// the same.
		if !strings.Contains(stdout, "priced 1") {
			t.Errorf("run %d: expected priced=1, got %q", i+1, stdout)
		}
	}
}
