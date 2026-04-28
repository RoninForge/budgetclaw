package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPricingListLines verifies the human-readable mode prints one
// model per line and includes the flagship Opus tier.
func TestPricingListLines(t *testing.T) {
	stdout, _, err := execCmd(t, "pricing", "list")
	if err != nil {
		t.Fatalf("pricing list: %v", err)
	}
	if !strings.Contains(stdout, "claude-opus-4-7") {
		t.Errorf("pricing list output missing claude-opus-4-7: %q", stdout)
	}
	// One line per model, no trailing comma artifacts from JSON
	// encoding leaking through.
	if strings.Contains(stdout, "[") || strings.Contains(stdout, "\"") {
		t.Errorf("pricing list (no --json) should not emit JSON syntax: %q", stdout)
	}
}

// TestPricingListJSON verifies the --json flag emits a valid array
// usable by the pricing-audit GitHub Action.
func TestPricingListJSON(t *testing.T) {
	stdout, _, err := execCmd(t, "pricing", "list", "--json")
	if err != nil {
		t.Fatalf("pricing list --json: %v", err)
	}
	var got []string
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("output not valid JSON: %v\nraw: %q", err, stdout)
	}
	found := false
	for _, m := range got {
		if m == "claude-opus-4-7" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("claude-opus-4-7 missing from JSON output: %v", got)
	}
}

// TestPricingDiagnoseEmptyDir confirms a fresh install (no JSONL
// files) reports cleanly without error.
func TestPricingDiagnoseEmptyDir(t *testing.T) {
	dir := t.TempDir()
	stdout, _, err := execCmd(t, "pricing", "diagnose", "--dir", dir)
	if err != nil {
		t.Fatalf("diagnose on empty dir: %v", err)
	}
	if !strings.Contains(stdout, "No assistant events") {
		t.Errorf("expected no-events message, got %q", stdout)
	}
}

// TestPricingDiagnoseFlagsMissingModel writes a JSONL fixture with
// one priceable event (claude-opus-4-7) and one unpriceable event
// (some-future-model), then asserts diagnose flags only the second.
func TestPricingDiagnoseFlagsMissingModel(t *testing.T) {
	dir := t.TempDir()
	fixture := strings.Join([]string{
		`{"type":"assistant","uuid":"u1","sessionId":"s1","timestamp":"2026-04-28T00:00:00Z","cwd":"/tmp/proj","gitBranch":"main","message":{"model":"claude-opus-4-7","usage":{"input_tokens":10,"output_tokens":5}}}`,
		`{"type":"assistant","uuid":"u2","sessionId":"s1","timestamp":"2026-04-28T00:00:01Z","cwd":"/tmp/proj","gitBranch":"main","message":{"model":"some-future-model","usage":{"input_tokens":10,"output_tokens":5}}}`,
		`{"type":"user","uuid":"u3"}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := execCmd(t, "pricing", "diagnose", "--dir", dir)
	// diagnose returns a non-nil error when missing models found,
	// to drive a non-zero exit code.
	if err == nil {
		t.Fatalf("expected error for missing model, got nil. stdout=%q", stdout)
	}
	if !strings.Contains(stdout, "MISSING") {
		t.Errorf("stdout missing MISSING marker: %q", stdout)
	}
	if !strings.Contains(stdout, "some-future-model") {
		t.Errorf("stdout should name the unknown model: %q", stdout)
	}
	if !strings.Contains(stdout, "claude-opus-4-7") {
		t.Errorf("stdout should also list the priced model: %q", stdout)
	}
}

// TestPricingDiagnoseJSONShape verifies --json emits a stable array
// of {model, events, priced} objects.
func TestPricingDiagnoseJSONShape(t *testing.T) {
	dir := t.TempDir()
	fixture := `{"type":"assistant","uuid":"u1","sessionId":"s1","timestamp":"2026-04-28T00:00:00Z","cwd":"/tmp/proj","gitBranch":"main","message":{"model":"claude-opus-4-7","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := execCmd(t, "pricing", "diagnose", "--dir", dir, "--json")
	if err != nil {
		t.Fatalf("diagnose --json: %v", err)
	}
	var rows []pricingDiagnoseRow
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %q", err, stdout)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].Model != "claude-opus-4-7" || rows[0].Events != 1 || !rows[0].Priced {
		t.Errorf("unexpected row: %+v", rows[0])
	}
}
