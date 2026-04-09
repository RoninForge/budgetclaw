package budget

import (
	"context"
	"errors"
	"testing"
	"time"
)

// staticSpend returns the same value for every call. Useful for
// tests that only care about one-rule evaluation.
func staticSpend(v float64) SpendFunc {
	return func(_ context.Context, _, _ string, _, _ time.Time) (float64, error) {
		return v, nil
	}
}

// errSpend returns a fixed error for every call.
func errSpend(err error) SpendFunc {
	return func(_ context.Context, _, _ string, _, _ time.Time) (float64, error) {
		return 0, err
	}
}

// TestEvaluateNilConfig is a defensive test: nil config → empty verdicts, no panic.
func TestEvaluateNilConfig(t *testing.T) {
	v, err := Evaluate(context.Background(), nil, "myapp", "main", time.Now(), staticSpend(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Errorf("expected 0 verdicts, got %d", len(v))
	}
}

// TestEvaluateEmptyConfig likewise: a config with zero rules
// produces zero verdicts.
func TestEvaluateEmptyConfig(t *testing.T) {
	cfg := &Config{Timezone: time.UTC}
	v, err := Evaluate(context.Background(), cfg, "myapp", "main", time.Now(), staticSpend(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Errorf("expected 0 verdicts, got %d", len(v))
	}
}

// TestEvaluateNoMatch returns empty verdicts when rules exist but
// none match.
func TestEvaluateNoMatch(t *testing.T) {
	cfg := &Config{
		Timezone: time.UTC,
		Rules: []Rule{
			{Project: "other", Branch: "main", Period: PeriodDaily, CapUSD: 5, Action: ActionWarn},
		},
	}
	v, err := Evaluate(context.Background(), cfg, "myapp", "main", time.Now(), staticSpend(10))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Errorf("expected 0 verdicts, got %d", len(v))
	}
}

// TestEvaluateSingleMatchNoBreach verifies a rule matches but
// current spend is below the cap.
func TestEvaluateSingleMatchNoBreach(t *testing.T) {
	cfg := &Config{
		Timezone: time.UTC,
		Rules: []Rule{
			{Project: "myapp", Branch: "main", Period: PeriodDaily, CapUSD: 10, Action: ActionWarn},
		},
	}
	v, err := Evaluate(context.Background(), cfg, "myapp", "main", time.Now(), staticSpend(5))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 1 {
		t.Fatalf("expected 1 verdict, got %d", len(v))
	}
	if v[0].Breach {
		t.Error("should not breach: 5 < 10")
	}
	if v[0].CurrentUSD != 5.0 {
		t.Errorf("CurrentUSD = %v", v[0].CurrentUSD)
	}
	if v[0].CapUSD != 10.0 {
		t.Errorf("CapUSD = %v", v[0].CapUSD)
	}
}

// TestEvaluateSingleMatchBreach verifies a breach is detected.
func TestEvaluateSingleMatchBreach(t *testing.T) {
	cfg := &Config{
		Timezone: time.UTC,
		Rules: []Rule{
			{Project: "myapp", Branch: "main", Period: PeriodDaily, CapUSD: 10, Action: ActionKill},
		},
	}
	v, err := Evaluate(context.Background(), cfg, "myapp", "main", time.Now(), staticSpend(10.01))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 1 || !v[0].Breach {
		t.Errorf("expected breach verdict, got %+v", v)
	}
}

// TestEvaluateStrictGreaterThan documents that exactly-at-cap is
// NOT a breach. 10.00 on a $10 cap is borderline: allowed.
func TestEvaluateStrictGreaterThan(t *testing.T) {
	cfg := &Config{
		Timezone: time.UTC,
		Rules: []Rule{
			{Project: "myapp", Branch: "main", Period: PeriodDaily, CapUSD: 10, Action: ActionWarn},
		},
	}
	v, err := Evaluate(context.Background(), cfg, "myapp", "main", time.Now(), staticSpend(10.00))
	if err != nil {
		t.Fatal(err)
	}
	if v[0].Breach {
		t.Error("10.00 on $10 cap should NOT breach (strict >)")
	}
}

// TestEvaluateMultipleRulesOrderedBySpecificity inserts three
// matching rules and verifies the response is ordered most-specific
// first with one verdict per rule.
func TestEvaluateMultipleRulesOrderedBySpecificity(t *testing.T) {
	cfg := &Config{
		Timezone: time.UTC,
		Rules: []Rule{
			{Project: "*", Branch: "*", Period: PeriodDaily, CapUSD: 100, Action: ActionWarn},
			{Project: "myapp", Branch: "main", Period: PeriodDaily, CapUSD: 10, Action: ActionKill},
			{Project: "myapp", Branch: "*", Period: PeriodDaily, CapUSD: 50, Action: ActionWarn},
		},
	}
	v, err := Evaluate(context.Background(), cfg, "myapp", "main", time.Now(), staticSpend(15))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 3 {
		t.Fatalf("expected 3 verdicts, got %d", len(v))
	}

	// Order: (myapp, main) score 3, (myapp, *) score 2, (*, *) score 0.
	caps := []float64{10, 50, 100}
	for i, want := range caps {
		if v[i].CapUSD != want {
			t.Errorf("verdict[%d].CapUSD = %v, want %v", i, v[i].CapUSD, want)
		}
	}

	// Breach flags: 15 > 10 only
	if !v[0].Breach || v[1].Breach || v[2].Breach {
		t.Errorf("breach flags = %v, %v, %v; want true false false",
			v[0].Breach, v[1].Breach, v[2].Breach)
	}
}

// TestEvaluateSpendErrorPropagates ensures a SpendFunc error is
// returned unchanged (wrapped with rule context) and partial
// verdicts are discarded.
func TestEvaluateSpendErrorPropagates(t *testing.T) {
	cfg := &Config{
		Timezone: time.UTC,
		Rules: []Rule{
			{Project: "myapp", Branch: "main", Period: PeriodDaily, CapUSD: 10, Action: ActionWarn},
		},
	}
	sentinel := errors.New("db unavailable")
	_, err := Evaluate(context.Background(), cfg, "myapp", "main", time.Now(), errSpend(sentinel))
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected wrapped sentinel, got %v", err)
	}
}

// TestEvaluateUsesConfigTimezone verifies that the timezone from
// the config is threaded through period bounds. Concretely: a daily
// period for "now = 2026-04-09 03:00 UTC" in Asia/Bangkok (UTC+7)
// should cover the local day 2026-04-09, which in UTC is
// 2026-04-08 17:00 -> 2026-04-09 16:59:59.
func TestEvaluateUsesConfigTimezone(t *testing.T) {
	bkk, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Timezone: bkk,
		Rules: []Rule{
			{Project: "*", Branch: "*", Period: PeriodDaily, CapUSD: 100, Action: ActionWarn},
		},
	}

	now := time.Date(2026, 4, 9, 3, 0, 0, 0, time.UTC) // 10:00 Bangkok, April 9

	// Capture the start/end the evaluator passes to spend.
	var gotStart, gotEnd time.Time
	spy := func(_ context.Context, _, _ string, start, end time.Time) (float64, error) {
		gotStart = start
		gotEnd = end
		return 0, nil
	}

	if _, err := Evaluate(context.Background(), cfg, "myapp", "main", now, spy); err != nil {
		t.Fatal(err)
	}

	// Local day = April 9 Bangkok = April 8 17:00 UTC -> April 9 16:59:59 UTC.
	wantStart := time.Date(2026, 4, 9, 0, 0, 0, 0, bkk)
	wantEnd := wantStart.AddDate(0, 0, 1).Add(-time.Second)
	if !gotStart.Equal(wantStart) {
		t.Errorf("start = %v, want %v", gotStart, wantStart)
	}
	if !gotEnd.Equal(wantEnd) {
		t.Errorf("end = %v, want %v", gotEnd, wantEnd)
	}
}

// TestPeriodBoundsDaily locks in UTC daily bounds.
func TestPeriodBoundsDaily(t *testing.T) {
	now := time.Date(2026, 4, 9, 15, 30, 45, 0, time.UTC)
	start, end := PeriodBounds(PeriodDaily, now, time.UTC)

	if !start.Equal(time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("start = %v", start)
	}
	if !end.Equal(time.Date(2026, 4, 9, 23, 59, 59, 0, time.UTC)) {
		t.Errorf("end = %v", end)
	}
}

// TestPeriodBoundsWeeklyStartsOnMonday verifies that a Wednesday
// resolves to a Monday-start week.
func TestPeriodBoundsWeeklyStartsOnMonday(t *testing.T) {
	// 2026-04-09 is a Thursday.
	now := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	start, end := PeriodBounds(PeriodWeekly, now, time.UTC)

	// Monday of that week: 2026-04-06.
	wantStart := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 4, 12, 23, 59, 59, 0, time.UTC)
	if !start.Equal(wantStart) {
		t.Errorf("start = %v, want %v", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("end = %v, want %v", end, wantEnd)
	}
}

// TestPeriodBoundsWeeklyFromSunday is the edge case that trips
// naive Weekday arithmetic: Sunday should be day 7 of its week,
// not day 0 of a new week.
func TestPeriodBoundsWeeklyFromSunday(t *testing.T) {
	// 2026-04-12 is a Sunday.
	now := time.Date(2026, 4, 12, 10, 0, 0, 0, time.UTC)
	start, _ := PeriodBounds(PeriodWeekly, now, time.UTC)
	wantStart := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) {
		t.Errorf("Sunday start = %v, want %v (Monday)", start, wantStart)
	}
}

// TestPeriodBoundsMonthly covers the calendar-month boundary.
func TestPeriodBoundsMonthly(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	start, end := PeriodBounds(PeriodMonthly, now, time.UTC)

	wantStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 4, 30, 23, 59, 59, 0, time.UTC)
	if !start.Equal(wantStart) {
		t.Errorf("start = %v", start)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("end = %v", end)
	}
}

// TestPeriodBoundsMonthlyAcrossYearBoundary verifies December
// wraps correctly into the next January.
func TestPeriodBoundsMonthlyAcrossYearBoundary(t *testing.T) {
	now := time.Date(2026, 12, 15, 12, 0, 0, 0, time.UTC)
	start, end := PeriodBounds(PeriodMonthly, now, time.UTC)

	wantStart := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	if !start.Equal(wantStart) || !end.Equal(wantEnd) {
		t.Errorf("got [%v, %v]", start, end)
	}
}

// TestPeriodBoundsNilLocation falls back to UTC.
func TestPeriodBoundsNilLocation(t *testing.T) {
	now := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	start, _ := PeriodBounds(PeriodDaily, now, nil)
	if start.Location().String() != "UTC" {
		t.Errorf("expected UTC fallback, got %s", start.Location())
	}
}
