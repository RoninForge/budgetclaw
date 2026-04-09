package budget

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteDefaultCreatesFile verifies a fresh init writes the
// documented default and later runs are no-ops.
func TestWriteDefaultCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.toml")

	if err := WriteDefault(path); err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Error("default config is empty")
	}

	// Parsing the default must succeed and yield at least the
	// example "*/*/daily/10/warn" rule.
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse default: %v", err)
	}
	if len(cfg.Rules) != 1 {
		t.Errorf("default rules = %d, want 1", len(cfg.Rules))
	}
}

// TestWriteDefaultIdempotent runs WriteDefault twice and verifies
// the file isn't overwritten. The canary is a custom modification
// between the two runs.
func TestWriteDefaultIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	if err := WriteDefault(path); err != nil {
		t.Fatal(err)
	}
	// Replace contents with a marker.
	if err := os.WriteFile(path, []byte("# canary\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WriteDefault(path); err != nil {
		t.Fatalf("second WriteDefault: %v", err)
	}

	data, _ := os.ReadFile(path)
	if string(data) != "# canary\n" {
		t.Error("second WriteDefault overwrote user edits")
	}
}

// TestAddLimitAppendsToEmptyFile verifies that creating a limit
// in a non-existent config file produces a valid config with
// exactly one rule.
func TestAddLimitAppendsToEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	rule := Rule{
		Project: "myapp",
		Branch:  "main",
		Period:  PeriodDaily,
		CapUSD:  5.00,
		Action:  ActionKill,
	}
	if err := AddLimit(path, rule); err != nil {
		t.Fatalf("AddLimit: %v", err)
	}

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(cfg.Rules))
	}
	r := cfg.Rules[0]
	if r.Project != "myapp" || r.Branch != "main" ||
		r.Period != PeriodDaily || r.CapUSD != 5.00 || r.Action != ActionKill {
		t.Errorf("unexpected round-trip rule: %+v", r)
	}
}

// TestAddLimitPreservesExisting verifies that appending a new
// limit does not disturb earlier rules.
func TestAddLimitPreservesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	for i, branch := range []string{"main", "feature/a", "feature/b"} {
		if err := AddLimit(path, Rule{
			Project: "myapp",
			Branch:  branch,
			Period:  PeriodDaily,
			CapUSD:  float64(i + 1),
			Action:  ActionWarn,
		}); err != nil {
			t.Fatalf("AddLimit %s: %v", branch, err)
		}
	}

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 3 {
		t.Errorf("expected 3 rules, got %d", len(cfg.Rules))
	}
	// Verify config order is preserved.
	wantBranches := []string{"main", "feature/a", "feature/b"}
	for i, want := range wantBranches {
		if cfg.Rules[i].Branch != want {
			t.Errorf("rule[%d].Branch = %q, want %q", i, cfg.Rules[i].Branch, want)
		}
	}
}

// TestRemoveLimitFoundAndGone verifies a successful removal:
// returns count=1, file no longer matches.
func TestRemoveLimitFoundAndGone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	// Seed with three rules, two of which match the selector.
	rules := []Rule{
		{Project: "myapp", Branch: "main", Period: PeriodDaily, CapUSD: 5, Action: ActionWarn},
		{Project: "other", Branch: "main", Period: PeriodDaily, CapUSD: 10, Action: ActionWarn},
		{Project: "myapp", Branch: "main", Period: PeriodDaily, CapUSD: 7, Action: ActionKill},
	}
	for _, r := range rules {
		if err := AddLimit(path, r); err != nil {
			t.Fatal(err)
		}
	}

	n, err := RemoveLimit(path, "myapp", "main", PeriodDaily)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("removed = %d, want 2", n)
	}

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 1 {
		t.Fatalf("expected 1 remaining rule, got %d", len(cfg.Rules))
	}
	if cfg.Rules[0].Project != "other" {
		t.Errorf("wrong rule kept: %+v", cfg.Rules[0])
	}
}

// TestRemoveLimitMissingFileIsNoop documents the contract: a
// remove against a file that does not exist returns (0, nil).
func TestRemoveLimitMissingFileIsNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.toml")
	n, err := RemoveLimit(path, "anything", "*", PeriodDaily)
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if n != 0 {
		t.Errorf("removed = %d, want 0", n)
	}
}

// TestRemoveLimitNormalization verifies that "" project/branch
// arguments are treated as "*", matching the parseLimit defaults.
func TestRemoveLimitNormalization(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	if err := AddLimit(path, Rule{
		Project: "*", Branch: "*",
		Period: PeriodDaily, CapUSD: 10, Action: ActionWarn,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := RemoveLimit(path, "", "", PeriodDaily)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("removed = %d, want 1", n)
	}
}

// TestRemoveLimitNoMatch returns 0 without rewriting the file.
func TestRemoveLimitNoMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	if err := AddLimit(path, Rule{
		Project: "myapp", Branch: "main",
		Period: PeriodDaily, CapUSD: 5, Action: ActionWarn,
	}); err != nil {
		t.Fatal(err)
	}

	// Snapshot before
	before, _ := os.ReadFile(path)

	n, err := RemoveLimit(path, "nonexistent", "branch", PeriodWeekly)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("removed = %d, want 0", n)
	}

	// File must be byte-identical.
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("RemoveLimit with no match modified the file")
	}
}

// TestSetNtfyConfigFirstTime creates the file with only the ntfy
// section set.
func TestSetNtfyConfigFirstTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	if err := SetNtfyConfig(path, "https://ntfy.sh", "my-secret-topic", 0.50); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NtfyServer != "https://ntfy.sh" {
		t.Errorf("server = %q", cfg.NtfyServer)
	}
	if cfg.NtfyTopic != "my-secret-topic" {
		t.Errorf("topic = %q", cfg.NtfyTopic)
	}
	if cfg.NtfyMinCostUSD != 0.50 {
		t.Errorf("min_cost_usd = %v", cfg.NtfyMinCostUSD)
	}
}

// TestSetNtfyConfigPreservesLimits ensures rewriting the alerts
// section does not drop existing [[limit]] rules.
func TestSetNtfyConfigPreservesLimits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	if err := AddLimit(path, Rule{
		Project: "myapp", Branch: "main",
		Period: PeriodDaily, CapUSD: 5, Action: ActionKill,
	}); err != nil {
		t.Fatal(err)
	}
	if err := SetNtfyConfig(path, "https://ntfy.sh", "topic", 0.25); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 1 {
		t.Errorf("limits lost: got %d rules", len(cfg.Rules))
	}
}

// TestAddLimitInvalidPath surfaces a useful filesystem error.
func TestAddLimitInvalidPath(t *testing.T) {
	// A path containing a NUL byte is rejected by os.OpenFile
	// on most platforms.
	err := AddLimit("/dev/null/nested/file.toml", Rule{
		Project: "*", Branch: "*",
		Period: PeriodDaily, CapUSD: 1, Action: ActionWarn,
	})
	if err == nil {
		t.Error("expected error for impossible path")
	}
}

// TestLoadTOMLMalformed surfaces invalid TOML as ErrConfig so
// callers can distinguish parse from I/O failures.
func TestLoadTOMLMalformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("not-valid-toml[[["), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadTOML(path)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrConfig) {
		t.Errorf("expected ErrConfig, got %v", err)
	}
}
