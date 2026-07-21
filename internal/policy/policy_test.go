package policy

import (
	"testing"

	"github.com/RoninForge/budgetclaw/internal/budget"
	"github.com/RoninForge/budgetclaw/internal/goei"
)

func wire(id, scopeType, scopeValue, enforcement, action string, cap int) goei.WirePolicy {
	w := goei.WirePolicy{ID: id, Period: "month", CapCents: cap, Enforcement: enforcement, Action: action}
	w.Scope.Type = scopeType
	w.Scope.Value = scopeValue
	return w
}

func TestFromWireAndFilters(t *testing.T) {
	pr := &goei.PolicyResponse{
		PolicyVersion: 7,
		Policies: []goei.WirePolicy{
			wire("gp_1_u2", "dev", "", "local_exact", "kill", 2500),
			wire("gp_3", "team", "", "server_aggregate", "warn", 50000),
			wire("gp_4", "project", "goei-web", "local_exact", "kill", 10000),
		},
	}
	b := BundleFromResponse(pr, `"etag"`, "2026-07-21T00:00:00Z")
	if b.PolicyVersion != 7 || len(b.Policies) != 3 {
		t.Fatalf("bundle = %+v", b)
	}
	if got := LocalExact(b.Policies); len(got) != 2 {
		t.Errorf("local_exact count = %d, want 2", len(got))
	}
	if got := Aggregate(b.Policies); len(got) != 1 {
		t.Errorf("aggregate count = %d, want 1", len(got))
	}
	if b.Policies[0].CapUSD() != 25.0 {
		t.Errorf("capUSD = %v, want 25", b.Policies[0].CapUSD())
	}
}

func TestMapPeriod(t *testing.T) {
	cases := map[string]budget.Period{
		"day":     budget.PeriodDaily,
		"week":    budget.PeriodWeekly,
		"month":   budget.PeriodMonthly,
		"unknown": budget.PeriodMonthly, // safe default
	}
	for in, want := range cases {
		if got := MapPeriod(in); got != want {
			t.Errorf("MapPeriod(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	// StateDir honors XDG_STATE_HOME; point it at a temp dir so the test never
	// touches the real cache.
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// A missing cache loads as an empty bundle, not an error.
	empty, err := Load()
	if err != nil {
		t.Fatalf("Load (missing): %v", err)
	}
	if len(empty.Policies) != 0 {
		t.Errorf("missing cache should be empty, got %d", len(empty.Policies))
	}

	in := &Bundle{
		PolicyVersion: 5,
		ETag:          `"g1.abc"`,
		FetchedAt:     "2026-07-21T00:00:00Z",
		Policies:      []Policy{{ID: "gp_1", ScopeType: "team", Period: "month", CapCents: 50000, Enforcement: "server_aggregate", Action: "warn"}},
	}
	if err := Save(in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.ETag != in.ETag || out.PolicyVersion != 5 || len(out.Policies) != 1 || out.Policies[0].ID != "gp_1" {
		t.Errorf("round trip mismatch: %+v", out)
	}
}
