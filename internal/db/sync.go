package db

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// SyncAggregate is one (project, branch, model, day) cost-and-token
// aggregate derived from the events table, shaped for pushing to an
// external dashboard. Unlike the rollups table it carries the model
// dimension, which dashboards use for per-model breakdowns.
type SyncAggregate struct {
	Project            string
	GitBranch          string
	Model              string
	Day                string // YYYY-MM-DD (UTC)
	CostUSD            float64
	InputTokens        int
	OutputTokens       int
	CacheReadTokens    int
	CacheWrite5mTokens int
	CacheWrite1hTokens int
}

// SyncAggregates returns per-(project, branch, model, day) aggregates
// for every event at or after since.
//
// Days are UTC calendar days, bucketed in Go from each event's
// timestamp rather than in SQL. The events.ts column is stored in Go's
// native time format ("2006-01-02 15:04:05.999 -0700 MST"), which
// SQLite's strftime/date functions cannot parse, so any SQL day
// bucketing would silently return NULL. Scanning into time.Time and
// formatting in Go is the only correct path here.
//
// The result is ordered by day, then project, branch, model for
// deterministic output and stable chunking downstream.
func (d *DB) SyncAggregates(ctx context.Context, since time.Time) ([]SyncAggregate, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT project, git_branch, model, ts,
		       cost_usd, input_tokens, output_tokens,
		       cache_read_tokens, cache_write_5m_tokens, cache_write_1h_tokens
		FROM events
		WHERE ts >= ?
	`, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type key struct{ project, branch, model, day string }
	agg := make(map[key]*SyncAggregate)

	for rows.Next() {
		var (
			project, branch, model string
			ts                     time.Time
			cost                   float64
			in, out, cr, cw5, cw1  int
		)
		if err := rows.Scan(
			&project, &branch, &model, &ts,
			&cost, &in, &out, &cr, &cw5, &cw1,
		); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}

		day := ts.UTC().Format(dayFormat)
		k := key{project, branch, model, day}
		a := agg[k]
		if a == nil {
			a = &SyncAggregate{Project: project, GitBranch: branch, Model: model, Day: day}
			agg[k] = a
		}
		a.CostUSD += cost
		a.InputTokens += in
		a.OutputTokens += out
		a.CacheReadTokens += cr
		a.CacheWrite5mTokens += cw5
		a.CacheWrite1hTokens += cw1
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}

	out := make([]SyncAggregate, 0, len(agg))
	for _, a := range agg {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day < out[j].Day
		}
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		if out[i].GitBranch != out[j].GitBranch {
			return out[i].GitBranch < out[j].GitBranch
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}

// RepoSpendKey is one (project, branch) that had spend in the sync window, plus the
// most recent working directory it was seen in, so the git-metadata collector can
// locate the repository. project and branch are the exact keys spend records join on.
type RepoSpendKey struct {
	Project string
	Branch  string
	CWD     string
}

// SpendRepoKeys returns one row per (project, git_branch) with events since `since`,
// carrying the latest cwd for that pair. It powers the opt-in cost-per-PR collector,
// which resolves each cwd to a git repo and reads branch/PR metadata locally. SQLite
// returns the cwd from the MAX(ts) row for each group.
func (d *DB) SpendRepoKeys(ctx context.Context, since time.Time) ([]RepoSpendKey, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT project, git_branch, cwd, MAX(ts)
		FROM events
		WHERE ts >= ? AND cwd != ''
		GROUP BY project, git_branch
	`, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("query spend repo keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RepoSpendKey
	for rows.Next() {
		var project, branch, cwd string
		var ts time.Time
		if err := rows.Scan(&project, &branch, &cwd, &ts); err != nil {
			return nil, fmt.Errorf("scan spend repo key: %w", err)
		}
		out = append(out, RepoSpendKey{Project: project, Branch: branch, CWD: cwd})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate spend repo keys: %w", err)
	}
	return out, nil
}
