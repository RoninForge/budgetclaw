// Package db persists parsed budgetclaw events and their per-day
// rollups in a local SQLite database.
//
// Two tables:
//
//	events   one row per (uuid) billable assistant message.
//	         UUID is the primary key and dedupe guarantee: re-inserting
//	         the same event is a no-op. Frozen historical fact.
//	rollups  one row per (project, git_branch, day) aggregate.
//	         Updated atomically with the event insert so the budget
//	         evaluator can do O(1) reads for cap checks.
//
// SQLite is opened with WAL journal mode on file-backed databases,
// enabling the CLI to read while the watcher writes from a separate
// process. The driver is modernc.org/sqlite (pure Go, no cgo) so
// the static-binary pledge holds.
//
// Costs are passed in from the caller at insert time and stored as
// historical fact. The db package has no dependency on the pricing
// table. A future Anthropic rate change will not retroactively
// re-price old events.
//
// Day boundaries in rollups are UTC. Budget evaluators that need
// local-timezone semantics should use RollupSum over a UTC time
// range computed from the user's tz, not the day string directly.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"

	"github.com/RoninForge/budgetclaw/internal/parser"
	"github.com/RoninForge/budgetclaw/internal/paths"
)

// schema is the full DDL applied on Open. Every statement is
// idempotent so an existing database is upgraded in place. When we
// need to evolve the schema we'll add a migrations table and move
// this constant into a first-version file.
const schema = `
CREATE TABLE IF NOT EXISTS events (
	uuid                      TEXT    PRIMARY KEY,
	session_id                TEXT    NOT NULL,
	ts                        DATETIME NOT NULL,
	cwd                       TEXT    NOT NULL,
	project                   TEXT    NOT NULL,
	git_branch                TEXT    NOT NULL,
	model                     TEXT    NOT NULL,
	service_tier              TEXT    NOT NULL,
	input_tokens              INTEGER NOT NULL,
	output_tokens             INTEGER NOT NULL,
	cache_read_tokens         INTEGER NOT NULL,
	cache_write_5m_tokens     INTEGER NOT NULL,
	cache_write_1h_tokens     INTEGER NOT NULL,
	cost_usd                  REAL    NOT NULL,
	inserted_at               DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_events_project_branch_ts
	ON events(project, git_branch, ts);
CREATE INDEX IF NOT EXISTS idx_events_ts          ON events(ts);
CREATE INDEX IF NOT EXISTS idx_events_session_id  ON events(session_id);

CREATE TABLE IF NOT EXISTS rollups (
	project                   TEXT    NOT NULL,
	git_branch                TEXT    NOT NULL,
	day                       TEXT    NOT NULL,
	event_count               INTEGER NOT NULL DEFAULT 0,
	input_tokens              INTEGER NOT NULL DEFAULT 0,
	output_tokens             INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens         INTEGER NOT NULL DEFAULT 0,
	cache_write_5m_tokens     INTEGER NOT NULL DEFAULT 0,
	cache_write_1h_tokens     INTEGER NOT NULL DEFAULT 0,
	cost_usd                  REAL    NOT NULL DEFAULT 0,
	updated_at                DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (project, git_branch, day)
);

CREATE INDEX IF NOT EXISTS idx_rollups_day ON rollups(day);
`

// dayFormat is the string format used for the rollups.day column.
// Keeping it as plain YYYY-MM-DD makes BETWEEN range queries trivial
// and aligns with ISO 8601.
const dayFormat = "2006-01-02"

// DB wraps a *sql.DB with budgetclaw-specific methods.
type DB struct {
	sql *sql.DB
}

// Rollup is one (project, branch, day) aggregate. For range-sum
// queries, Day is empty because the result spans multiple days.
type Rollup struct {
	Project            string
	GitBranch          string
	Day                string
	EventCount         int
	InputTokens        int
	OutputTokens       int
	CacheReadTokens    int
	CacheWrite5mTokens int
	CacheWrite1hTokens int
	CostUSD            float64
}

// Open opens or creates the state database.
//
// If path is empty, Open resolves the default location via
// paths.StateDir() (honoring XDG_STATE_HOME) and creates any missing
// parent directories. If path is the literal string ":memory:",
// Open returns an in-memory database suitable for tests.
//
// The schema is applied on every call. Existing tables are not
// recreated thanks to IF NOT EXISTS.
func Open(path string) (*DB, error) {
	memory := path == ":memory:"

	if path == "" {
		dir, err := paths.StateDir()
		if err != nil {
			return nil, fmt.Errorf("resolve state dir: %w", err)
		}
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create state dir: %w", err)
		}
		path = filepath.Join(dir, "state.db")
	}

	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// In-memory databases ignore WAL (no disk to journal to) and
	// must use a single connection so every statement sees the
	// same state.
	if memory {
		sqlDB.SetMaxOpenConns(1)
	}

	if err := applyPragmas(sqlDB, memory); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	if _, err := sqlDB.Exec(schema); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	return &DB{sql: sqlDB}, nil
}

// OpenMemory returns an in-memory database for tests. Equivalent to
// Open(":memory:") but avoids forcing test code to know the magic
// string.
func OpenMemory() (*DB, error) { return Open(":memory:") }

// Close closes the underlying connection.
func (d *DB) Close() error { return d.sql.Close() }

// applyPragmas sets the pragmas we rely on. WAL and synchronous are
// skipped for in-memory databases where they're meaningless.
func applyPragmas(db *sql.DB, memory bool) error {
	pragmas := []string{
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	}
	if !memory {
		pragmas = append(pragmas,
			"PRAGMA journal_mode=WAL",
			"PRAGMA synchronous=NORMAL",
		)
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return fmt.Errorf("pragma %q: %w", p, err)
		}
	}
	return nil
}

// Insert stores a billable event and updates its rollup row inside
// a single transaction. Idempotent on e.UUID: re-inserting the same
// event is a no-op (the rollup is not double-counted).
//
// costUSD is passed in so the db package stays independent of the
// pricing table. Callers should compute it via pricing.CostForModel
// before calling Insert.
func (d *DB) Insert(ctx context.Context, e *parser.Event, costUSD float64) error {
	if e == nil {
		return errors.New("nil event")
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op if Commit succeeded

	res, err := tx.ExecContext(ctx, `
		INSERT INTO events (
			uuid, session_id, ts, cwd, project, git_branch,
			model, service_tier,
			input_tokens, output_tokens,
			cache_read_tokens, cache_write_5m_tokens, cache_write_1h_tokens,
			cost_usd
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(uuid) DO NOTHING
	`,
		e.UUID, e.SessionID, e.Timestamp.UTC(),
		e.CWD, e.Project, e.GitBranch,
		e.Model, e.ServiceTier,
		e.InputTokens, e.OutputTokens,
		e.CacheReadTokens, e.CacheCreation5mTokens, e.CacheCreation1hTokens,
		costUSD,
	)
	if err != nil {
		return fmt.Errorf("insert event: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		// Duplicate UUID: event already stored, rollup already reflects it.
		return tx.Commit()
	}

	day := e.Timestamp.UTC().Format(dayFormat)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rollups (
			project, git_branch, day,
			event_count,
			input_tokens, output_tokens,
			cache_read_tokens, cache_write_5m_tokens, cache_write_1h_tokens,
			cost_usd, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,CURRENT_TIMESTAMP)
		ON CONFLICT(project, git_branch, day) DO UPDATE SET
			event_count           = event_count           + excluded.event_count,
			input_tokens          = input_tokens          + excluded.input_tokens,
			output_tokens         = output_tokens         + excluded.output_tokens,
			cache_read_tokens     = cache_read_tokens     + excluded.cache_read_tokens,
			cache_write_5m_tokens = cache_write_5m_tokens + excluded.cache_write_5m_tokens,
			cache_write_1h_tokens = cache_write_1h_tokens + excluded.cache_write_1h_tokens,
			cost_usd              = cost_usd              + excluded.cost_usd,
			updated_at            = CURRENT_TIMESTAMP
	`,
		e.Project, e.GitBranch, day,
		1,
		e.InputTokens, e.OutputTokens,
		e.CacheReadTokens, e.CacheCreation5mTokens, e.CacheCreation1hTokens,
		costUSD,
	); err != nil {
		return fmt.Errorf("upsert rollup: %w", err)
	}

	return tx.Commit()
}

// RollupForDay returns the rollup row for a specific (project,
// branch, day). Returns (nil, nil) if the row does not exist —
// "nothing spent today" is a valid state, not an error.
func (d *DB) RollupForDay(ctx context.Context, project, branch string, day time.Time) (*Rollup, error) {
	dayStr := day.UTC().Format(dayFormat)

	row := d.sql.QueryRowContext(ctx, `
		SELECT project, git_branch, day, event_count,
		       input_tokens, output_tokens,
		       cache_read_tokens, cache_write_5m_tokens, cache_write_1h_tokens,
		       cost_usd
		FROM rollups
		WHERE project = ? AND git_branch = ? AND day = ?
	`, project, branch, dayStr)

	var r Rollup
	err := row.Scan(
		&r.Project, &r.GitBranch, &r.Day, &r.EventCount,
		&r.InputTokens, &r.OutputTokens,
		&r.CacheReadTokens, &r.CacheWrite5mTokens, &r.CacheWrite1hTokens,
		&r.CostUSD,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan rollup: %w", err)
	}
	return &r, nil
}

// RollupSum returns the sum across a date range for a single
// (project, branch). Range is inclusive on both ends. Returned
// Rollup has empty Day and (Project, GitBranch) copied from args so
// callers don't need to re-thread them.
//
// Used by the budget evaluator for weekly/monthly caps, and by the
// CLI for "status --period=week" output.
func (d *DB) RollupSum(ctx context.Context, project, branch string, start, end time.Time) (*Rollup, error) {
	startStr := start.UTC().Format(dayFormat)
	endStr := end.UTC().Format(dayFormat)

	row := d.sql.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(event_count),           0),
			COALESCE(SUM(input_tokens),          0),
			COALESCE(SUM(output_tokens),         0),
			COALESCE(SUM(cache_read_tokens),     0),
			COALESCE(SUM(cache_write_5m_tokens), 0),
			COALESCE(SUM(cache_write_1h_tokens), 0),
			COALESCE(SUM(cost_usd),              0)
		FROM rollups
		WHERE project = ? AND git_branch = ? AND day >= ? AND day <= ?
	`, project, branch, startStr, endStr)

	r := Rollup{Project: project, GitBranch: branch}
	if err := row.Scan(
		&r.EventCount,
		&r.InputTokens, &r.OutputTokens,
		&r.CacheReadTokens, &r.CacheWrite5mTokens, &r.CacheWrite1hTokens,
		&r.CostUSD,
	); err != nil {
		return nil, fmt.Errorf("scan rollup sum: %w", err)
	}
	return &r, nil
}

// StatusByProject returns rollup totals grouped by (project, branch)
// across a date range. Ordered by project then branch for
// deterministic CLI output.
//
// Empty result is not an error — a user with nothing tracked yet
// gets a nil slice and no rows.
func (d *DB) StatusByProject(ctx context.Context, start, end time.Time) ([]Rollup, error) {
	startStr := start.UTC().Format(dayFormat)
	endStr := end.UTC().Format(dayFormat)

	rows, err := d.sql.QueryContext(ctx, `
		SELECT project, git_branch,
		       SUM(event_count),
		       SUM(input_tokens), SUM(output_tokens),
		       SUM(cache_read_tokens),
		       SUM(cache_write_5m_tokens), SUM(cache_write_1h_tokens),
		       SUM(cost_usd)
		FROM rollups
		WHERE day >= ? AND day <= ?
		GROUP BY project, git_branch
		ORDER BY project, git_branch
	`, startStr, endStr)
	if err != nil {
		return nil, fmt.Errorf("status query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Rollup
	for rows.Next() {
		var r Rollup
		if err := rows.Scan(
			&r.Project, &r.GitBranch,
			&r.EventCount,
			&r.InputTokens, &r.OutputTokens,
			&r.CacheReadTokens,
			&r.CacheWrite5mTokens, &r.CacheWrite1hTokens,
			&r.CostUSD,
		); err != nil {
			return nil, fmt.Errorf("scan status row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("status rows: %w", err)
	}
	return out, nil
}
