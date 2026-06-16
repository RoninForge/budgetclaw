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

// TestPricingRatesEmitsCorrectShape verifies the --json output of
// pricing rates is a valid array of {model, input_per_mtok,
// output_per_mtok} objects with realistic values for a known model.
func TestPricingRatesEmitsCorrectShape(t *testing.T) {
	stdout, _, err := execCmd(t, "pricing", "rates", "--json")
	if err != nil {
		t.Fatalf("pricing rates --json: %v", err)
	}
	var rows []pricingRateRow
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("output not valid JSON: %v\nraw: %q", err, stdout)
	}
	var found bool
	for _, r := range rows {
		if r.Model == "claude-opus-4-7" {
			found = true
			if r.InputPerMTok != 5.00 || r.OutputPerMTok != 25.00 {
				t.Errorf("claude-opus-4-7 rates = %v/%v, want 5.00/25.00",
					r.InputPerMTok, r.OutputPerMTok)
			}
		}
	}
	if !found {
		t.Errorf("claude-opus-4-7 missing from rates output")
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

// TestPricingRatesSkipsRetiredModels verifies the current-rates JSON
// only contains currently-priced models. KnownModels now includes
// models the dataset retired (no current rate), and `pricing rates`
// must skip those rather than error so the external audit workflow
// keeps parsing a clean array of priced models.
func TestPricingRatesSkipsRetiredModels(t *testing.T) {
	stdout, _, err := execCmd(t, "pricing", "rates", "--json")
	if err != nil {
		t.Fatalf("pricing rates --json: %v", err)
	}
	var rows []pricingRateRow
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("output not valid JSON: %v\nraw: %q", err, stdout)
	}
	if len(rows) == 0 {
		t.Fatal("expected at least one currently-priced model")
	}
	for _, r := range rows {
		// A retired model (e.g. claude-3-opus-20240229) carries no
		// current rate and must not appear here.
		if r.Model == "claude-3-opus-20240229" {
			t.Errorf("retired model %q leaked into current rates output", r.Model)
		}
		if r.InputPerMTok <= 0 || r.OutputPerMTok <= 0 {
			t.Errorf("model %q has non-positive rate %v/%v", r.Model, r.InputPerMTok, r.OutputPerMTok)
		}
	}
}

// TestPricingHistoryTable verifies the human-readable history table for
// a model with a price change includes both rate tiers and the FROM/TO
// columns.
func TestPricingHistoryTable(t *testing.T) {
	stdout, _, err := execCmd(t, "pricing", "history", "claude-3-5-haiku-20241022")
	if err != nil {
		t.Fatalf("pricing history: %v", err)
	}
	for _, want := range []string{"FROM", "TO", "INPUT/MTOK", "2024-11-04", "2024-12-03", "$1.00", "$0.80"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("history table missing %q:\n%s", want, stdout)
		}
	}
}

// TestPricingHistoryJSON verifies --json emits a stable array of
// {from, to?, input_per_mtok, output_per_mtok} objects, with the open
// (current) interval omitting "to".
func TestPricingHistoryJSON(t *testing.T) {
	stdout, _, err := execCmd(t, "pricing", "history", "claude-opus-4-1-20250805", "--json")
	if err != nil {
		t.Fatalf("pricing history --json: %v", err)
	}
	var rows []pricingHistoryRow
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %q", err, stdout)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 interval for opus-4-1, got %d", len(rows))
	}
	if rows[0].To != "" {
		t.Errorf("open interval should omit 'to', got %q", rows[0].To)
	}
	if rows[0].InputPerMTok != 15.00 || rows[0].OutputPerMTok != 75.00 {
		t.Errorf("rates = %v/%v, want 15.00/75.00", rows[0].InputPerMTok, rows[0].OutputPerMTok)
	}
}

// TestPricingHistoryUnknownModel verifies an unknown model id returns
// an error.
func TestPricingHistoryUnknownModel(t *testing.T) {
	_, _, err := execCmd(t, "pricing", "history", "not-a-real-model")
	if err == nil {
		t.Error("expected error for unknown model")
	}
}

// TestPricingProvenance verifies the provenance command prints the
// pinned dataset tag and index commit.
func TestPricingProvenance(t *testing.T) {
	stdout, _, err := execCmd(t, "pricing", "provenance")
	if err != nil {
		t.Fatalf("pricing provenance: %v", err)
	}
	if !strings.Contains(stdout, "dataset tag:") || !strings.Contains(stdout, "index commit:") {
		t.Errorf("provenance output missing labels: %q", stdout)
	}
	if !strings.Contains(stdout, "v2026") {
		t.Errorf("provenance output should include the pinned tag: %q", stdout)
	}
}

// TestPricingProvenanceJSON verifies --json emits {tag, commit}.
func TestPricingProvenanceJSON(t *testing.T) {
	stdout, _, err := execCmd(t, "pricing", "provenance", "--json")
	if err != nil {
		t.Fatalf("pricing provenance --json: %v", err)
	}
	var row pricingProvenanceRow
	if err := json.Unmarshal([]byte(stdout), &row); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %q", err, stdout)
	}
	if row.Tag == "" || row.Commit == "" {
		t.Errorf("provenance JSON has empty fields: %+v", row)
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
