package budget

import (
	"context"
	"fmt"
	"time"
)

// SpendFunc returns the total USD cost for (project, branch) across
// the given time range. It is the evaluator's only dependency on
// stored state: callers inject a function that wraps db.RollupSum
// (or a test fake) and the evaluator stays pure.
//
// Both start and end are inclusive. The implementation is free to
// ignore sub-second precision; budget boundaries are day-aligned.
type SpendFunc func(ctx context.Context, project, branch string, start, end time.Time) (float64, error)

// Verdict is the result of evaluating one rule against the current
// period. The caller inspects verdicts to decide what to do:
// typically, fire the most-severe action among those where
// Breach == true.
type Verdict struct {
	Rule        Rule
	CurrentUSD  float64
	CapUSD      float64
	Breach      bool
	PeriodStart time.Time
	PeriodEnd   time.Time
}

// Evaluate computes one Verdict per rule that matches (project, branch).
// Returns them ordered most-specific first (matching Config.Match).
// An empty result means no rule applies; a nil config also yields
// nil verdicts.
//
// A rule is "in breach" when current spend is strictly greater than
// the cap. Equality is not a breach: a $10/day cap lets the user
// hit exactly $10 without firing, but the next billable event will
// push past and trigger. This is the more forgiving interpretation
// and matches what users expect from "cap at $10".
//
// If any SpendFunc call returns an error, Evaluate stops and returns
// that error wrapped with rule context. Partial verdicts are
// discarded so callers never have to reason about "half-evaluated".
func Evaluate(
	ctx context.Context,
	cfg *Config,
	project, branch string,
	now time.Time,
	spend SpendFunc,
) ([]Verdict, error) {
	matches := cfg.Match(project, branch)
	if len(matches) == 0 {
		return nil, nil
	}

	loc := time.UTC
	if cfg != nil && cfg.Timezone != nil {
		loc = cfg.Timezone
	}

	verdicts := make([]Verdict, 0, len(matches))
	for _, rule := range matches {
		start, end := PeriodBounds(rule.Period, now, loc)
		current, err := spend(ctx, project, branch, start, end)
		if err != nil {
			return nil, fmt.Errorf("query spend for rule %s %s %s: %w",
				rule.Project, rule.Branch, rule.Period, err)
		}
		verdicts = append(verdicts, Verdict{
			Rule:        rule,
			CurrentUSD:  current,
			CapUSD:      rule.CapUSD,
			Breach:      current > rule.CapUSD,
			PeriodStart: start,
			PeriodEnd:   end,
		})
	}
	return verdicts, nil
}

// PeriodBounds returns the [start, end] range covering the period
// that contains `now`. Both bounds are in the given location.
//
// start is the first second of the period.
// end is the last second of the period (23:59:59 on the final day),
// chosen instead of "start of next period" so SpendFunc's inclusive
// range query does not accidentally cover the next period's first
// day. Sub-second precision is not load-bearing.
//
// Weeks start on Monday (ISO 8601). Months use calendar boundaries.
// nil location is normalized to UTC.
func PeriodBounds(p Period, now time.Time, loc *time.Location) (time.Time, time.Time) {
	if loc == nil {
		loc = time.UTC
	}
	local := now.In(loc)
	year, month, day := local.Date()

	switch p {
	case PeriodDaily:
		start := time.Date(year, month, day, 0, 0, 0, 0, loc)
		end := start.AddDate(0, 0, 1).Add(-time.Second)
		return start, end

	case PeriodWeekly:
		// Monday-based week: Mon=0, Tue=1, ..., Sun=6.
		offset := (int(local.Weekday()) + 6) % 7
		start := time.Date(year, month, day-offset, 0, 0, 0, 0, loc)
		end := start.AddDate(0, 0, 7).Add(-time.Second)
		return start, end

	case PeriodMonthly:
		start := time.Date(year, month, 1, 0, 0, 0, 0, loc)
		end := start.AddDate(0, 1, 0).Add(-time.Second)
		return start, end
	}
	// Exhaustive switch above — this is unreachable but kept as a
	// safety net if Period gains a new variant without updating us.
	return local, local
}
