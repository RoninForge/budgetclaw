package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RoninForge/budgetclaw/internal/budget"
	"github.com/RoninForge/budgetclaw/internal/policy"
)

// The guard evaluator sums spend for the current period using the pipeline
// clock, so tests pin Now to the sample event's own month.
var guardWhen = time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)

func devPolicy(action string, capCents int) policy.Policy {
	return policy.Policy{
		ID: "gp_1_u2", ScopeType: "dev", ScopeValue: "", Period: "month",
		CapCents: capCents, Enforcement: "local_exact", Action: action, SetBy: "boss@x.com",
	}
}

func pendingCount(t *testing.T, p *Pipeline) int {
	t.Helper()
	pend, err := p.DB.PendingGuardEvents(context.Background(), 10)
	if err != nil {
		t.Fatalf("PendingGuardEvents: %v", err)
	}
	return len(pend)
}

func TestGuardKillsWhenDeveloperOverCap(t *testing.T) {
	fk := &fakeKiller{byCwd: map[string][]int{"/home/u/app": {1}}}
	p := buildPipeline(t, &budget.Config{Timezone: time.UTC}, nil, fk)
	p.Now = func() time.Time { return guardWhen }
	p.Machine = "test-mbp"
	p.SetGuardPolicies([]policy.Policy{devPolicy("kill", 1000)}) // $10 cap, event is $15

	if err := p.Handle(context.Background(), sampleEvent("u1", "app", "main"), "/src"); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if fk.killCalls == 0 {
		t.Error("expected the runaway to be killed")
	}
	lk, _ := p.Enforcer.Locks.IsLocked("app", "main")
	if lk == nil {
		t.Fatal("expected a lock for app/main")
	}
	if lk.PolicyID != "gp_1_u2" {
		t.Errorf("lock PolicyID = %q, want gp_1_u2", lk.PolicyID)
	}
	pend, _ := p.DB.PendingGuardEvents(context.Background(), 10)
	if len(pend) != 1 {
		t.Fatalf("audit events = %d, want 1", len(pend))
	}
	if !strings.Contains(pend[0].JSON, `"action":"kill"`) || !strings.Contains(pend[0].JSON, `"policyId":"gp_1_u2"`) {
		t.Errorf("audit event json = %s", pend[0].JSON)
	}
}

func TestGuardWarnsWithoutKilling(t *testing.T) {
	fk := &fakeKiller{byCwd: map[string][]int{"/home/u/app": {1}}}
	p := buildPipeline(t, &budget.Config{Timezone: time.UTC}, nil, fk)
	p.Now = func() time.Time { return guardWhen }
	p.SetGuardPolicies([]policy.Policy{devPolicy("warn", 1000)})

	if err := p.Handle(context.Background(), sampleEvent("u1", "app", "main"), "/src"); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if fk.killCalls != 0 {
		t.Error("warn policy must not kill")
	}
	if lk, _ := p.Enforcer.Locks.IsLocked("app", "main"); lk != nil {
		t.Error("warn policy must not write a lock")
	}
	if n := pendingCount(t, p); n != 1 {
		t.Errorf("audit events = %d, want 1", n)
	}
}

func TestGuardDoesNothingUnderCap(t *testing.T) {
	p := buildPipeline(t, &budget.Config{Timezone: time.UTC}, nil, nil)
	p.Now = func() time.Time { return guardWhen }
	p.SetGuardPolicies([]policy.Policy{devPolicy("kill", 5000)}) // $50 cap, event is $15

	if err := p.Handle(context.Background(), sampleEvent("u1", "app", "main"), "/src"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if n := pendingCount(t, p); n != 0 {
		t.Errorf("under cap should produce no audit events, got %d", n)
	}
}

func TestGuardProjectCapIgnoresOtherProjects(t *testing.T) {
	fk := &fakeKiller{byCwd: map[string][]int{"/home/u/app": {1}}}
	p := buildPipeline(t, &budget.Config{Timezone: time.UTC}, nil, fk)
	p.Now = func() time.Time { return guardWhen }
	// A cap on project "billing", but the runaway is in "app".
	p.SetGuardPolicies([]policy.Policy{{
		ID: "gp_9", ScopeType: "project", ScopeValue: "billing", Period: "month",
		CapCents: 500, Enforcement: "local_exact", Action: "kill",
	}})

	if err := p.Handle(context.Background(), sampleEvent("u1", "app", "main"), "/src"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fk.killCalls != 0 {
		t.Error("a cap on another project must not kill this one")
	}
	if n := pendingCount(t, p); n != 0 {
		t.Errorf("no audit event expected, got %d", n)
	}
}

func TestGuardNotifiesOncePerPeriod(t *testing.T) {
	fk := &fakeKiller{byCwd: map[string][]int{"/home/u/app": {1}}}
	p := buildPipeline(t, &budget.Config{Timezone: time.UTC}, nil, fk)
	p.Now = func() time.Time { return guardWhen }
	p.SetGuardPolicies([]policy.Policy{devPolicy("warn", 1000)})

	// Two events in the same period: the audit event is deduped to one.
	_ = p.Handle(context.Background(), sampleEvent("u1", "app", "main"), "/src")
	_ = p.Handle(context.Background(), sampleEvent("u2", "app", "feature"), "/src")
	if n := pendingCount(t, p); n != 1 {
		t.Errorf("audit events = %d, want 1 (deduped per period)", n)
	}
}
