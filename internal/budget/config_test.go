package budget

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestParseEmpty decodes a zero-byte config. An empty file is a
// valid, no-rules configuration — not an error. The watcher can
// start with no rules; it just never breaches anything.
func TestParseEmpty(t *testing.T) {
	cfg, err := Parse([]byte{})
	if err != nil {
		t.Fatalf("empty Parse: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if len(cfg.Rules) != 0 {
		t.Errorf("expected 0 rules, got %d", len(cfg.Rules))
	}
	if cfg.Timezone.String() != "UTC" {
		t.Errorf("default timezone should be UTC, got %s", cfg.Timezone)
	}
}

// TestParseFullFixture loads the testdata sample and verifies every
// section was decoded correctly.
func TestParseFullFixture(t *testing.T) {
	cfg, err := LoadFile("testdata/sample.toml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	if cfg.Timezone.String() != "Asia/Bangkok" {
		t.Errorf("timezone = %s, want Asia/Bangkok", cfg.Timezone)
	}
	if cfg.NtfyServer != "https://push.example.com" {
		t.Errorf("NtfyServer = %q", cfg.NtfyServer)
	}
	if cfg.NtfyTopic != "test-topic-aaaa" {
		t.Errorf("NtfyTopic = %q", cfg.NtfyTopic)
	}
	if cfg.NtfyMinCostUSD != 0.25 {
		t.Errorf("NtfyMinCostUSD = %v", cfg.NtfyMinCostUSD)
	}
	if len(cfg.Rules) != 3 {
		t.Fatalf("expected 3 rules, got %d", len(cfg.Rules))
	}

	// Rule 0: global warn
	r0 := cfg.Rules[0]
	if r0.Project != "*" || r0.Branch != "*" || r0.Period != PeriodDaily ||
		r0.CapUSD != 20.00 || r0.Action != ActionWarn {
		t.Errorf("rule[0] = %+v", r0)
	}

	// Rule 1: myapp main, daily $5, kill
	r1 := cfg.Rules[1]
	if r1.Project != "myapp" || r1.Branch != "main" || r1.Period != PeriodDaily ||
		r1.CapUSD != 5.00 || r1.Action != ActionKill {
		t.Errorf("rule[1] = %+v", r1)
	}

	// Rule 2: myapp feature/*, weekly $3, kill
	r2 := cfg.Rules[2]
	if r2.Project != "myapp" || r2.Branch != "feature/*" || r2.Period != PeriodWeekly ||
		r2.CapUSD != 3.00 || r2.Action != ActionKill {
		t.Errorf("rule[2] = %+v", r2)
	}
}

// TestParseDefaultsApplied proves that missing optional fields
// default sensibly: period→daily, action→warn, empty strings→"*".
func TestParseDefaultsApplied(t *testing.T) {
	src := `
[[limit]]
cap_usd = 1.00
`
	cfg, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 1 {
		t.Fatal("expected 1 rule")
	}
	r := cfg.Rules[0]
	if r.Project != "*" {
		t.Errorf("Project default = %q, want *", r.Project)
	}
	if r.Branch != "*" {
		t.Errorf("Branch default = %q, want *", r.Branch)
	}
	if r.Period != PeriodDaily {
		t.Errorf("Period default = %s, want daily", r.Period)
	}
	if r.Action != ActionWarn {
		t.Errorf("Action default = %s, want warn", r.Action)
	}
}

// TestParseInvalidTOML wraps the parser error in ErrConfig.
func TestParseInvalidTOML(t *testing.T) {
	_, err := Parse([]byte(`not-valid-toml[[[`))
	if !errors.Is(err, ErrConfig) {
		t.Errorf("expected ErrConfig, got %v", err)
	}
}

// TestParseInvalidPeriod rejects unknown period strings.
func TestParseInvalidPeriod(t *testing.T) {
	src := `
[[limit]]
cap_usd = 1.00
period = "hourly"
`
	_, err := Parse([]byte(src))
	if !errors.Is(err, ErrConfig) {
		t.Errorf("expected ErrConfig, got %v", err)
	}
}

// TestParseInvalidAction rejects unknown action strings.
func TestParseInvalidAction(t *testing.T) {
	src := `
[[limit]]
cap_usd = 1.00
action = "nuke"
`
	_, err := Parse([]byte(src))
	if !errors.Is(err, ErrConfig) {
		t.Errorf("expected ErrConfig, got %v", err)
	}
}

// TestParseInvalidTimezone rejects IANA names that time.LoadLocation
// does not know.
func TestParseInvalidTimezone(t *testing.T) {
	src := `
[general]
timezone = "Mars/Olympus_Mons"
`
	_, err := Parse([]byte(src))
	if !errors.Is(err, ErrConfig) {
		t.Errorf("expected ErrConfig, got %v", err)
	}
}

// TestParseNegativeCap rejects nonsensical cap values.
func TestParseNegativeCap(t *testing.T) {
	src := `
[[limit]]
cap_usd = -1.00
`
	_, err := Parse([]byte(src))
	if !errors.Is(err, ErrConfig) {
		t.Errorf("expected ErrConfig, got %v", err)
	}
}

// TestParseZeroCap accepts zero — a "warn at any spend" rule.
func TestParseZeroCap(t *testing.T) {
	src := `
[[limit]]
cap_usd = 0.00
`
	cfg, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("zero cap should be valid: %v", err)
	}
	if cfg.Rules[0].CapUSD != 0.00 {
		t.Errorf("CapUSD = %v", cfg.Rules[0].CapUSD)
	}
}

// TestLoadFileNotExist returns the os error unchanged so callers
// can distinguish "config missing" from "config malformed".
func TestLoadFileNotExist(t *testing.T) {
	_, err := LoadFile(filepath.Join(t.TempDir(), "nope.toml"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got %v", err)
	}
}

// TestRuleMatchesExact covers the common case.
func TestRuleMatchesExact(t *testing.T) {
	r := Rule{Project: "myapp", Branch: "main"}
	if !r.Matches("myapp", "main") {
		t.Error("exact match should succeed")
	}
	if r.Matches("other", "main") {
		t.Error("different project should not match")
	}
	if r.Matches("myapp", "other") {
		t.Error("different branch should not match")
	}
}

// TestRuleMatchesWildcard documents the "*" short-circuit.
// path.Match("*", "feature/login") returns false (slash), which is
// why we special-case "*" before calling path.Match.
func TestRuleMatchesWildcard(t *testing.T) {
	r := Rule{Project: "*", Branch: "*"}
	cases := [][2]string{
		{"anything", "main"},
		{"myapp", "feature/login"},
		{"", ""},
		{"weird/name", "also/weird"},
	}
	for _, c := range cases {
		if !r.Matches(c[0], c[1]) {
			t.Errorf("wildcard should match (%q, %q)", c[0], c[1])
		}
	}
}

// TestRuleMatchesGlob covers path.Match-style patterns.
func TestRuleMatchesGlob(t *testing.T) {
	r := Rule{Project: "app*", Branch: "feature/*"}

	if !r.Matches("app1", "feature/login") {
		t.Error("app1 / feature/login should match")
	}
	if !r.Matches("app-beta", "feature/x") {
		t.Error("app-beta / feature/x should match")
	}
	// path.Match treats "*" as "no /", so deeper nested branches do
	// not match "feature/*" — document this behavior.
	if r.Matches("app1", "feature/nested/thing") {
		t.Error("nested branch should NOT match feature/*")
	}
	if r.Matches("app1", "main") {
		t.Error("main should NOT match feature/*")
	}
	if r.Matches("other", "feature/x") {
		t.Error("other project should NOT match app*")
	}
}

// TestRuleSpecificity locks in the scoring. The evaluator uses this
// to order matching rules; a bug here would break user expectations.
func TestRuleSpecificity(t *testing.T) {
	cases := []struct {
		rule Rule
		want int
	}{
		{Rule{Project: "myapp", Branch: "main"}, 3},
		{Rule{Project: "myapp", Branch: "*"}, 2},
		{Rule{Project: "*", Branch: "main"}, 1},
		{Rule{Project: "*", Branch: "*"}, 0},
		{Rule{Project: "", Branch: ""}, 0},
		{Rule{Project: "feature/*", Branch: "feature/*"}, 3},
	}
	for _, c := range cases {
		got := c.rule.Specificity()
		if got != c.want {
			t.Errorf("%+v specificity = %d, want %d", c.rule, got, c.want)
		}
	}
}

// TestConfigMatchOrdering verifies that Match returns the most
// specific rule first, with config order as the stable tiebreaker.
func TestConfigMatchOrdering(t *testing.T) {
	cfg := &Config{
		Rules: []Rule{
			{Project: "*", Branch: "*", CapUSD: 100},  // score 0
			{Project: "myapp", Branch: "*", CapUSD: 50}, // score 2
			{Project: "myapp", Branch: "main", CapUSD: 10}, // score 3
			{Project: "*", Branch: "main", CapUSD: 30}, // score 1
		},
	}
	got := cfg.Match("myapp", "main")
	if len(got) != 4 {
		t.Fatalf("expected 4 matches, got %d", len(got))
	}
	// Expected order: score 3, 2, 1, 0
	want := []float64{10, 50, 30, 100}
	for i, r := range got {
		if r.CapUSD != want[i] {
			t.Errorf("match[%d].CapUSD = %v, want %v", i, r.CapUSD, want[i])
		}
	}
}

// TestConfigMatchNoRules returns nil for an empty config.
func TestConfigMatchNoRules(t *testing.T) {
	cfg := &Config{}
	if got := cfg.Match("myapp", "main"); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

// TestConfigMatchNilReceiver is a safety check: calling Match on
// a nil *Config should not panic. The watcher may Evaluate() before
// a config is loaded.
func TestConfigMatchNilReceiver(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil receiver panicked: %v", r)
		}
	}()
	var cfg *Config
	if got := cfg.Match("any", "any"); got != nil {
		t.Errorf("expected nil from nil receiver, got %+v", got)
	}
}

// TestPeriodString locks in the canonical names (used in error
// messages and potentially for serialization).
func TestPeriodString(t *testing.T) {
	cases := map[Period]string{
		PeriodDaily:   "daily",
		PeriodWeekly:  "weekly",
		PeriodMonthly: "monthly",
		Period(999):   "unknown",
	}
	for p, want := range cases {
		if got := p.String(); got != want {
			t.Errorf("Period(%d).String() = %q, want %q", p, got, want)
		}
	}
}

func TestActionString(t *testing.T) {
	cases := map[Action]string{
		ActionWarn: "warn",
		ActionKill: "kill",
		Action(99): "unknown",
	}
	for a, want := range cases {
		if got := a.String(); got != want {
			t.Errorf("Action(%d).String() = %q, want %q", a, got, want)
		}
	}
}
