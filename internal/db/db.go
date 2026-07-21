// Package db persists parsed budgetclaw events and their per-day
// rollups in a local SQLite database.
//
// Two tables:
//
//	events   one row per billable assistant API response.
//	         Claude Code writes the same response on multiple JSONL
//	         lines (one per tool-result roundtrip), each with its own
//	         line uuid but a shared (message_id, request_id) pair and
//	         the same response usage. We dedupe on that pair so the
//	         response is counted once, matching ccusage. Lines with no
//	         message_id (older Claude Code schemas) fall back to uuid
//	         dedupe. A later line for the same response REPLACEs the
//	         stored row, so the most complete usage wins and
//	         re-processing stays idempotent.
//	rollups  one row per (project, git_branch, day) aggregate.
//	         Updated atomically with the event insert so the budget
//	         evaluator can do O(1) reads for cap checks. A replace
//	         updates the rollup by the delta (new minus old) so a
//	         redundant line never double-counts.
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
	"strings"
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
	message_id                TEXT    NOT NULL DEFAULT '',
	request_id                TEXT    NOT NULL DEFAULT '',
	inserted_at               DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_events_project_branch_ts
	ON events(project, git_branch, ts);
CREATE INDEX IF NOT EXISTS idx_events_ts          ON events(ts);
CREATE INDEX IF NOT EXISTS idx_events_session_id  ON events(session_id);

-- Response dedupe: Claude Code writes one API response across several
-- JSONL lines (different uuid, same message_id + request_id). The
-- partial index makes (message_id, request_id) the uniqueness key
-- whenever message_id is present, leaving older message_id-less rows
-- on the uuid primary key. Insert upserts against this index.
CREATE UNIQUE INDEX IF NOT EXISTS idx_events_message_request
	ON events(message_id, request_id) WHERE message_id != '';

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

-- Guard Mode audit queue: enforcement events (a remote policy warned or
-- killed a runaway) wait here until the next sync ships them to Goei, then
-- are deleted. dedup_key makes queueing idempotent so re-firing the same
-- breach in the same period does not pile up duplicate rows.
CREATE TABLE IF NOT EXISTS guard_pending (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	dedup_key   TEXT    NOT NULL DEFAULT '',
	event_json  TEXT    NOT NULL,
	created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_guard_pending_dedup
	ON guard_pending(dedup_key) WHERE dedup_key != '';
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
	// Column migrations run before the schema DDL so the unique index
	// in schema can reference message_id/request_id on a database
	// created by an older binary.
	if err := migrate(sqlDB); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	if _, err := sqlDB.Exec(schema); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	return &DB{sql: sqlDB}, nil
}

// migrate brings an events table created by an older binary up to the
// current shape. CREATE TABLE IF NOT EXISTS never alters an existing
// table, so new columns are added here with idempotent ALTER TABLE
// statements (SQLite errors with "duplicate column name" if the column
// already exists, which we treat as success). A freshly created table
// already has the columns via schema, so the ALTERs are no-ops there
// too. Must run before the schema's unique index on (message_id,
// request_id) is created.
func migrate(db *sql.DB) error {
	alters := []string{
		`ALTER TABLE events ADD COLUMN message_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE events ADD COLUMN request_id TEXT NOT NULL DEFAULT ''`,
	}
	// The events table may not exist yet on a brand-new database; in
	// that case the schema DDL creates it with the columns already
	// present, so skip the ALTERs entirely.
	var name string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='events'`,
	).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect events table: %w", err)
	}
	for _, stmt := range alters {
		if _, err := db.Exec(stmt); err != nil {
			if strings.Contains(err.Error(), "duplicate column name") {
				continue // column already present, idempotent
			}
			return fmt.Errorf("migrate events: %w", err)
		}
	}
	return nil
}

// OpenMemory returns an in-memory database for tests. Equivalent to
// Open(":memory:") but avoids forcing test code to know the magic
// string.
func OpenMemory() (*DB, error) { return Open(":memory:") }

// Close closes the underlying connection.
func (d *DB) Close() error { return d.sql.Close() }

// Reset truncates the events and rollups tables so a subsequent
// backfill can re-attribute every historical event from scratch.
// Used by `budgetclaw backfill --rebuild` after a pricing-table
// correction lands: existing rollups are stuck at the old (wrong)
// rate because Insert is idempotent on uuid, so the only way to
// recompute them is to wipe and replay.
//
// The tables are truncated inside a single transaction so a crash
// mid-reset cannot leave half-empty state behind.
func (d *DB) Reset(ctx context.Context) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM rollups`); err != nil {
		return fmt.Errorf("truncate rollups: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM events`); err != nil {
		return fmt.Errorf("truncate events: %w", err)
	}
	return tx.Commit()
}

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
// a single transaction.
//
// Dedupe key:
//   - When e.MessageID is non-empty, uniqueness is (message_id,
//     request_id). Claude Code writes the same API response across
//     several JSONL lines (each with its own uuid), so this is the
//     key that counts one response once and matches ccusage.
//   - When e.MessageID is empty (older Claude Code schemas), the key
//     falls back to e.UUID, preserving the original behavior.
//
// A second line for an already-stored response REPLACEs the row:
// later lines of a streaming response can carry more complete usage,
// and replacing makes re-processing idempotent. The rollup is updated
// by the delta (new contribution minus the old row's), so a redundant
// or growing line never double-counts. When the redundant line is
// identical the delta is zero and the rollup is untouched.
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

	// Look up any row already representing this response so we can
	// compute the rollup delta and remove a stale row if its uuid
	// differs from this line's. existing.found is false on first sight.
	existing, err := lookupExisting(ctx, tx, e)
	if err != nil {
		return err
	}

	// First sighting of this response: plain insert, full rollup add.
	if !existing.found {
		if _, err := tx.ExecContext(ctx, insertEventSQL,
			e.UUID, e.SessionID, e.Timestamp.UTC(),
			e.CWD, e.Project, e.GitBranch,
			e.Model, e.ServiceTier,
			e.InputTokens, e.OutputTokens,
			e.CacheReadTokens, e.CacheCreation5mTokens, e.CacheCreation1hTokens,
			costUSD, e.MessageID, e.RequestID,
		); err != nil {
			return fmt.Errorf("insert event: %w", err)
		}
		if err := applyRollupDelta(ctx, tx, e.Project, e.GitBranch,
			e.Timestamp, 1,
			e.InputTokens, e.OutputTokens,
			e.CacheReadTokens, e.CacheCreation5mTokens, e.CacheCreation1hTokens,
			costUSD,
		); err != nil {
			return err
		}
		return tx.Commit()
	}

	// Already-stored response. Replace its row with this line's values
	// and fold the delta into the rollup. The old contribution is
	// removed from its original rollup key (project/branch/day from the
	// stored row); the new contribution is added to this line's key.
	// They are normally identical, but subtracting from the stored key
	// keeps the rollup correct even if attribution somehow shifted.
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE uuid = ?`, existing.uuid); err != nil {
		return fmt.Errorf("delete superseded event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, insertEventSQL,
		e.UUID, e.SessionID, e.Timestamp.UTC(),
		e.CWD, e.Project, e.GitBranch,
		e.Model, e.ServiceTier,
		e.InputTokens, e.OutputTokens,
		e.CacheReadTokens, e.CacheCreation5mTokens, e.CacheCreation1hTokens,
		costUSD, e.MessageID, e.RequestID,
	); err != nil {
		return fmt.Errorf("replace event: %w", err)
	}

	// Remove the old row's contribution from its rollup (event_count
	// unchanged overall, so subtract 0 here and add 0 below: a replace
	// is the same response, not a new one).
	if err := applyRollupDelta(ctx, tx, existing.project, existing.branch,
		existing.ts, 0,
		-existing.input, -existing.output,
		-existing.cacheRead, -existing.cacheWrite5m, -existing.cacheWrite1h,
		-existing.cost,
	); err != nil {
		return err
	}
	// Add the new line's contribution to its rollup.
	if err := applyRollupDelta(ctx, tx, e.Project, e.GitBranch,
		e.Timestamp, 0,
		e.InputTokens, e.OutputTokens,
		e.CacheReadTokens, e.CacheCreation5mTokens, e.CacheCreation1hTokens,
		costUSD,
	); err != nil {
		return err
	}

	return tx.Commit()
}

// insertEventSQL inserts a fully specified event row. Used by both the
// first-sighting and replace paths in Insert.
const insertEventSQL = `
	INSERT INTO events (
		uuid, session_id, ts, cwd, project, git_branch,
		model, service_tier,
		input_tokens, output_tokens,
		cache_read_tokens, cache_write_5m_tokens, cache_write_1h_tokens,
		cost_usd, message_id, request_id
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
`

// existingEvent holds the stored row that represents the same response
// as the incoming line, with the columns Insert needs to reverse its
// rollup contribution. found is false when this is the first sighting.
type existingEvent struct {
	found        bool
	uuid         string
	project      string
	branch       string
	ts           time.Time
	input        int
	output       int
	cacheRead    int
	cacheWrite5m int
	cacheWrite1h int
	cost         float64
}

// lookupExisting finds the row already representing e's response. When
// e.MessageID is set it matches on (message_id, request_id); otherwise
// it matches on uuid. Returns found=false (zero value) if no such row
// exists yet.
func lookupExisting(ctx context.Context, tx *sql.Tx, e *parser.Event) (existingEvent, error) {
	const byMessageSQL = `
		SELECT uuid, project, git_branch, ts,
		       input_tokens, output_tokens,
		       cache_read_tokens, cache_write_5m_tokens, cache_write_1h_tokens,
		       cost_usd
		FROM events WHERE message_id = ? AND request_id = ? LIMIT 1`
	const byUUIDSQL = `
		SELECT uuid, project, git_branch, ts,
		       input_tokens, output_tokens,
		       cache_read_tokens, cache_write_5m_tokens, cache_write_1h_tokens,
		       cost_usd
		FROM events WHERE uuid = ? LIMIT 1`

	query := byUUIDSQL
	args := []any{e.UUID}
	if e.MessageID != "" {
		query = byMessageSQL
		args = []any{e.MessageID, e.RequestID}
	}

	var ex existingEvent
	row := tx.QueryRowContext(ctx, query, args...)
	err := row.Scan(
		&ex.uuid, &ex.project, &ex.branch, &ex.ts,
		&ex.input, &ex.output,
		&ex.cacheRead, &ex.cacheWrite5m, &ex.cacheWrite1h,
		&ex.cost,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return existingEvent{}, nil
	}
	if err != nil {
		return existingEvent{}, fmt.Errorf("lookup existing event: %w", err)
	}
	ex.found = true
	return ex, nil
}

// applyRollupDelta folds a signed token/cost contribution into the
// rollup row for (project, branch, day-of ts). Positive values add,
// negative values subtract. countDelta tracks event_count. The row is
// created on first touch via upsert. Day is derived from ts in UTC.
func applyRollupDelta(
	ctx context.Context, tx *sql.Tx,
	project, branch string, ts time.Time, countDelta int,
	input, output, cacheRead, cacheWrite5m, cacheWrite1h int,
	cost float64,
) error {
	day := ts.UTC().Format(dayFormat)
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
		project, branch, day,
		countDelta,
		input, output,
		cacheRead, cacheWrite5m, cacheWrite1h,
		cost,
	); err != nil {
		return fmt.Errorf("upsert rollup: %w", err)
	}
	return nil
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

// TotalSum returns the summed cost across every project and branch in the
// inclusive date range. Guard Mode uses it for a per-developer (whole-machine)
// cap, where the runaway can be in any project.
func (d *DB) TotalSum(ctx context.Context, start, end time.Time) (float64, error) {
	startStr := start.UTC().Format(dayFormat)
	endStr := end.UTC().Format(dayFormat)
	row := d.sql.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cost_usd), 0)
		FROM rollups WHERE day >= ? AND day <= ?
	`, startStr, endStr)
	var v float64
	if err := row.Scan(&v); err != nil {
		return 0, fmt.Errorf("scan total sum: %w", err)
	}
	return v, nil
}

// ProjectSum returns the summed cost for one project across all its branches
// in the inclusive date range. Guard Mode uses it for a per-project cap.
func (d *DB) ProjectSum(ctx context.Context, project string, start, end time.Time) (float64, error) {
	startStr := start.UTC().Format(dayFormat)
	endStr := end.UTC().Format(dayFormat)
	row := d.sql.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cost_usd), 0)
		FROM rollups WHERE project = ? AND day >= ? AND day <= ?
	`, project, startStr, endStr)
	var v float64
	if err := row.Scan(&v); err != nil {
		return 0, fmt.Errorf("scan project sum: %w", err)
	}
	return v, nil
}

// PendingGuardEvent is one queued audit record awaiting sync.
type PendingGuardEvent struct {
	ID   int64
	JSON string
}

// QueueGuardEvent stores an enforcement audit event (already JSON-encoded)
// for the next sync to ship. Returns whether a new row was inserted; a
// duplicate dedup_key is ignored and returns false, which callers use to
// fire a notification only on the first occurrence of a breach.
func (d *DB) QueueGuardEvent(ctx context.Context, dedupKey, eventJSON string) (bool, error) {
	res, err := d.sql.ExecContext(ctx,
		`INSERT OR IGNORE INTO guard_pending (dedup_key, event_json) VALUES (?, ?)`,
		dedupKey, eventJSON)
	if err != nil {
		return false, fmt.Errorf("queue guard event: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil
	}
	return n > 0, nil
}

// PendingGuardEvents returns up to limit queued audit events, oldest first.
func (d *DB) PendingGuardEvents(ctx context.Context, limit int) ([]PendingGuardEvent, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, event_json FROM guard_pending ORDER BY id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query guard pending: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PendingGuardEvent
	for rows.Next() {
		var e PendingGuardEvent
		if err := rows.Scan(&e.ID, &e.JSON); err != nil {
			return nil, fmt.Errorf("scan guard pending: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("guard pending rows: %w", err)
	}
	return out, nil
}

// DeleteGuardEvents removes queued audit events by id after a successful
// sync. A nil/empty slice is a no-op.
func (d *DB) DeleteGuardEvents(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	q := "DELETE FROM guard_pending WHERE id IN (" + strings.Join(placeholders, ",") + ")"
	if _, err := d.sql.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("delete guard events: %w", err)
	}
	return nil
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
