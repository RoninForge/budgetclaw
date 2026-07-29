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
// An event the caller could not price is still stored, via
// InsertUnpriced, with cost_usd = 0 and priced = 0. No billable event is
// ever discarded, so a pricing table that has not yet learned a new
// model costs visibility, not data: the tokens are retained and can be
// priced later from the stored row. Unpriced rows contribute $0 to every
// cost sum, and rollups.unpriced_count records how many are waiting so
// callers can present affected totals as minimums.
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
	priced                    INTEGER NOT NULL DEFAULT 1,
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

-- Unpriced backlog. An event whose model had no rate at insert time is
-- stored with cost_usd = 0 and priced = 0 rather than discarded, so the
-- token counts survive and can be priced later from the row itself. The
-- partial index is empty in the healthy case, which keeps the "is there
-- a backlog?" probe free.
CREATE INDEX IF NOT EXISTS idx_events_unpriced
	ON events(model, ts) WHERE priced = 0;

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
	unpriced_count            INTEGER NOT NULL DEFAULT 0,
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

-- Small key-value state that is not user data: the reconciliation
-- watermark today, notification dedupe later. Kept in the database
-- rather than a file so it shares the same transaction and lifetime as
-- the rows it describes.
CREATE TABLE IF NOT EXISTS meta (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
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

	// UnpricedCount is how many events in this aggregate are stored but
	// not yet priced. When it is non-zero, CostUSD is a minimum: the
	// tokens are recorded but their dollar value is not known to this
	// build's pricing table.
	UnpricedCount int
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

// columnMigration is one additive column added to a table after that
// table's first release. Applied on every Open, in order.
type columnMigration struct {
	table  string
	column string
	ddl    string
}

// columnMigrations lists every column added since the original schema.
//
// The DEFAULT on each is load-bearing for existing rows. events.priced
// defaults to 1 because every row written by an older binary exists only
// because pricing succeeded, so "priced" is correct by construction;
// rollups.unpriced_count defaults to 0 for the same reason.
var columnMigrations = []columnMigration{
	{"events", "message_id", `ALTER TABLE events ADD COLUMN message_id TEXT NOT NULL DEFAULT ''`},
	{"events", "request_id", `ALTER TABLE events ADD COLUMN request_id TEXT NOT NULL DEFAULT ''`},
	{"events", "priced", `ALTER TABLE events ADD COLUMN priced INTEGER NOT NULL DEFAULT 1`},
	{"rollups", "unpriced_count", `ALTER TABLE rollups ADD COLUMN unpriced_count INTEGER NOT NULL DEFAULT 0`},
}

// migrate brings tables created by an older binary up to the current
// shape. CREATE TABLE IF NOT EXISTS never alters an existing table, so
// new columns are added here.
//
// Each migration is skipped when its table does not exist yet (a
// brand-new database gets the columns from the schema DDL) or when the
// column is already present. The "duplicate column name" tolerance is
// kept as a belt-and-braces fallback in case the pragma probe and the
// ALTER ever disagree.
//
// Must run before the schema DDL, whose indexes reference columns added
// here (message_id/request_id for the dedupe index, priced for the
// unpriced index).
//
// Downgrade safety: an older binary opening a migrated database ignores
// the extra columns, since every INSERT names its columns explicitly.
// It resumes discarding unknown-model events, but nothing is corrupted.
func migrate(db *sql.DB) error {
	for _, m := range columnMigrations {
		exists, err := tableExists(db, m.table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		has, err := hasColumn(db, m.table, m.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := db.Exec(m.ddl); err != nil {
			if strings.Contains(err.Error(), "duplicate column name") {
				continue // already present, idempotent
			}
			return fmt.Errorf("migrate %s.%s: %w", m.table, m.column, err)
		}
	}
	return nil
}

// tableExists reports whether a table has been created yet.
func tableExists(db *sql.DB, table string) (bool, error) {
	var name string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
	).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect table %s: %w", table, err)
	}
	return true, nil
}

// hasColumn reports whether table already has the named column.
func hasColumn(db *sql.DB, table, column string) (bool, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("inspect %s.%s: %w", table, column, err)
	}
	return n > 0, nil
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
//
// Use InsertUnpriced when the pricing table has no rate for the event's
// model; the row is stored either way, so no billable event is ever
// discarded.
func (d *DB) Insert(ctx context.Context, e *parser.Event, costUSD float64) error {
	return d.insert(ctx, e, costUSD, true)
}

// InsertUnpriced stores an event the pricing table could not price.
//
// The row keeps its full token counts, model id, timestamp, project and
// branch, and carries cost_usd = 0 with priced = 0. This is what makes a
// stale pricing table a reporting problem instead of data loss: the
// tokens are a permanent local fact from the moment they are seen, and a
// later release (or a fetched table) can price them from the stored row
// alone, with no dependence on Claude Code's JSONL logs, which are
// pruned after roughly a month.
//
// Dollar totals are unaffected while a row is unpriced: it contributes
// exactly $0 to every rollup, cap evaluation and Guard Mode sum. The
// rollup's event_count and token columns DO count it, because activity
// genuinely happened; only the cost is unknown. rollups.unpriced_count
// records how many such rows a (project, branch, day) holds so callers
// can report the totals as minimums.
func (d *DB) InsertUnpriced(ctx context.Context, e *parser.Event) error {
	return d.insert(ctx, e, 0, false)
}

// insert is the shared implementation behind Insert and InsertUnpriced.
func (d *DB) insert(ctx context.Context, e *parser.Event, costUSD float64, priced bool) error {
	if e == nil {
		return errors.New("nil event")
	}
	unpricedDelta := 0
	if !priced {
		unpricedDelta = 1
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
			costUSD, e.MessageID, e.RequestID, priced,
		); err != nil {
			return fmt.Errorf("insert event: %w", err)
		}
		if err := applyRollupDelta(ctx, tx, e.Project, e.GitBranch,
			e.Timestamp, 1,
			e.InputTokens, e.OutputTokens,
			e.CacheReadTokens, e.CacheCreation5mTokens, e.CacheCreation1hTokens,
			costUSD, unpricedDelta,
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
		costUSD, e.MessageID, e.RequestID, priced,
	); err != nil {
		return fmt.Errorf("replace event: %w", err)
	}

	// Remove the old row's contribution from its rollup (event_count
	// unchanged overall, so subtract 0 here and add 0 below: a replace
	// is the same response, not a new one).
	//
	// The unpriced delta follows the same subtract-old/add-new shape, so
	// replaying a previously-unpriced line with an upgraded pricing table
	// heals it: the old -1 clears the backlog counter and the new row
	// carries its real cost. That makes plain `backfill` a second,
	// independent recovery path.
	oldUnpriced := 0
	if !existing.priced {
		oldUnpriced = 1
	}
	if err := applyRollupDelta(ctx, tx, existing.project, existing.branch,
		existing.ts, 0,
		-existing.input, -existing.output,
		-existing.cacheRead, -existing.cacheWrite5m, -existing.cacheWrite1h,
		-existing.cost, -oldUnpriced,
	); err != nil {
		return err
	}
	// Add the new line's contribution to its rollup.
	if err := applyRollupDelta(ctx, tx, e.Project, e.GitBranch,
		e.Timestamp, 0,
		e.InputTokens, e.OutputTokens,
		e.CacheReadTokens, e.CacheCreation5mTokens, e.CacheCreation1hTokens,
		costUSD, unpricedDelta,
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
		cost_usd, message_id, request_id, priced
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
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
	priced       bool
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
		       cost_usd, priced
		FROM events WHERE message_id = ? AND request_id = ? LIMIT 1`
	const byUUIDSQL = `
		SELECT uuid, project, git_branch, ts,
		       input_tokens, output_tokens,
		       cache_read_tokens, cache_write_5m_tokens, cache_write_1h_tokens,
		       cost_usd, priced
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
		&ex.cost, &ex.priced,
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
// negative values subtract. countDelta tracks event_count and
// unpricedDelta tracks unpriced_count. The row is created on first
// touch via upsert. Day is derived from ts in UTC.
func applyRollupDelta(
	ctx context.Context, tx *sql.Tx,
	project, branch string, ts time.Time, countDelta int,
	input, output, cacheRead, cacheWrite5m, cacheWrite1h int,
	cost float64, unpricedDelta int,
) error {
	day := ts.UTC().Format(dayFormat)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rollups (
			project, git_branch, day,
			event_count,
			input_tokens, output_tokens,
			cache_read_tokens, cache_write_5m_tokens, cache_write_1h_tokens,
			cost_usd, unpriced_count, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,CURRENT_TIMESTAMP)
		ON CONFLICT(project, git_branch, day) DO UPDATE SET
			event_count           = event_count           + excluded.event_count,
			input_tokens          = input_tokens          + excluded.input_tokens,
			output_tokens         = output_tokens         + excluded.output_tokens,
			cache_read_tokens     = cache_read_tokens     + excluded.cache_read_tokens,
			cache_write_5m_tokens = cache_write_5m_tokens + excluded.cache_write_5m_tokens,
			cache_write_1h_tokens = cache_write_1h_tokens + excluded.cache_write_1h_tokens,
			cost_usd              = cost_usd              + excluded.cost_usd,
			unpriced_count        = unpriced_count        + excluded.unpriced_count,
			updated_at            = CURRENT_TIMESTAMP
	`,
		project, branch, day,
		countDelta,
		input, output,
		cacheRead, cacheWrite5m, cacheWrite1h,
		cost, unpricedDelta,
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
		       cost_usd, unpriced_count
		FROM rollups
		WHERE project = ? AND git_branch = ? AND day = ?
	`, project, branch, dayStr)

	var r Rollup
	err := row.Scan(
		&r.Project, &r.GitBranch, &r.Day, &r.EventCount,
		&r.InputTokens, &r.OutputTokens,
		&r.CacheReadTokens, &r.CacheWrite5mTokens, &r.CacheWrite1hTokens,
		&r.CostUSD, &r.UnpricedCount,
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
			COALESCE(SUM(cost_usd),              0),
			COALESCE(SUM(unpriced_count),        0)
		FROM rollups
		WHERE project = ? AND git_branch = ? AND day >= ? AND day <= ?
	`, project, branch, startStr, endStr)

	r := Rollup{Project: project, GitBranch: branch}
	if err := row.Scan(
		&r.EventCount,
		&r.InputTokens, &r.OutputTokens,
		&r.CacheReadTokens, &r.CacheWrite5mTokens, &r.CacheWrite1hTokens,
		&r.CostUSD, &r.UnpricedCount,
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

// UnpricedModel summarizes the stored-but-unpriced backlog for one model.
type UnpricedModel struct {
	Model      string
	EventCount int
	Tokens     int

	// FirstSeen is the UTC day of the earliest unpriced event for this
	// model, as YYYY-MM-DD. It is a string rather than a time.Time
	// because ts is stored as text ("2006-01-02 15:04:05.000 +0000 UTC")
	// and an aggregate like MIN(ts) loses the column type affinity the
	// driver needs to hand back a time.Time. The day is sliced out in
	// SQL, which is both simpler and exactly what callers display.
	FirstSeen string
}

// HasUnpriced reports whether any stored event is still unpriced.
//
// This is the cheap probe callers use before doing any unpriced-specific
// work: the partial index idx_events_unpriced is empty when everything is
// priced, so the healthy case costs one index lookup and callers can keep
// their output byte-identical to a build without this feature.
func (d *DB) HasUnpriced(ctx context.Context) (bool, error) {
	var n int
	if err := d.sql.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM events WHERE priced = 0)`,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("probe unpriced: %w", err)
	}
	return n > 0, nil
}

// UnpricedByModel returns the stored-but-unpriced backlog grouped by
// model, most events first, so the CLI can name exactly which models are
// missing from this build's pricing table and how much activity is
// waiting on them.
//
// Deliberately reports counts and tokens but never a dollar estimate:
// guessing a rate for an unpriced model would trade one dishonest number
// for another.
func (d *DB) UnpricedByModel(ctx context.Context) ([]UnpricedModel, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT model,
		       COUNT(*),
		       COALESCE(SUM(input_tokens + output_tokens + cache_read_tokens +
		                    cache_write_5m_tokens + cache_write_1h_tokens), 0),
		       substr(MIN(ts), 1, 10)
		FROM events
		WHERE priced = 0
		GROUP BY model
		ORDER BY COUNT(*) DESC, model
	`)
	if err != nil {
		return nil, fmt.Errorf("query unpriced: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UnpricedModel
	for rows.Next() {
		var m UnpricedModel
		if err := rows.Scan(&m.Model, &m.EventCount, &m.Tokens, &m.FirstSeen); err != nil {
			return nil, fmt.Errorf("scan unpriced row: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("unpriced rows: %w", err)
	}
	return out, nil
}

// OldestRollupDay returns the earliest day the database holds, as
// YYYY-MM-DD, or "" when there is no history at all.
//
// rollups is authoritative here rather than events: both are written in
// the same transaction, and both are what a rebuild would destroy.
func (d *DB) OldestRollupDay(ctx context.Context) (string, error) {
	var day sql.NullString
	if err := d.sql.QueryRowContext(ctx, `SELECT MIN(day) FROM rollups`).Scan(&day); err != nil {
		return "", fmt.Errorf("oldest rollup day: %w", err)
	}
	if !day.Valid {
		return "", nil
	}
	return day.String, nil
}

// MetaGet reads a small state value. Returns "" when the key is unset,
// which callers treat as "never run".
func (d *DB) MetaGet(ctx context.Context, key string) (string, error) {
	var v string
	err := d.sql.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("meta get %s: %w", key, err)
	}
	return v, nil
}

// MetaSet writes a small state value, overwriting any previous one.
func (d *DB) MetaSet(ctx context.Context, key, value string) error {
	if _, err := d.sql.ExecContext(ctx, `
		INSERT INTO meta (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP
	`, key, value); err != nil {
		return fmt.Errorf("meta set %s: %w", key, err)
	}
	return nil
}

// UnpricedEvent is one stored-but-unpriced row, carrying everything a
// caller needs to price it. The db package deliberately does not import
// pricing, so the caller computes the cost and hands it back via
// ApplyReprice.
type UnpricedEvent struct {
	UUID         string
	Model        string
	Timestamp    time.Time
	Project      string
	GitBranch    string
	Input        int
	Output       int
	CacheRead    int
	CacheWrite5m int
	CacheWrite1h int
}

// RepricedEvent is the result of pricing an UnpricedEvent.
type RepricedEvent struct {
	UUID      string
	Project   string
	GitBranch string
	Timestamp time.Time
	CostUSD   float64
}

// UnpricedModels returns the distinct models in the unpriced backlog.
// Served by the partial index, so it stays cheap even on a large events
// table, and returns nothing at all in the healthy case.
func (d *DB) UnpricedModels(ctx context.Context) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT DISTINCT model FROM events WHERE priced = 0 ORDER BY model`)
	if err != nil {
		return nil, fmt.Errorf("query unpriced models: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("scan unpriced model: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("unpriced model rows: %w", err)
	}
	return out, nil
}

// UnpricedBatch returns up to limit unpriced events for one model whose
// uuid sorts after afterUUID.
//
// Keyset pagination on uuid, rather than repeatedly selecting WHERE
// priced = 0, is deliberate: a row that stays unpriceable (a known model
// with no interval covering its timestamp) would otherwise be returned
// forever and the caller would never terminate.
func (d *DB) UnpricedBatch(ctx context.Context, model, afterUUID string, limit int) ([]UnpricedEvent, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT uuid, model, ts, project, git_branch,
		       input_tokens, output_tokens,
		       cache_read_tokens, cache_write_5m_tokens, cache_write_1h_tokens
		FROM events
		WHERE priced = 0 AND model = ? AND uuid > ?
		ORDER BY uuid
		LIMIT ?
	`, model, afterUUID, limit)
	if err != nil {
		return nil, fmt.Errorf("query unpriced batch: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UnpricedEvent
	for rows.Next() {
		var e UnpricedEvent
		if err := rows.Scan(
			&e.UUID, &e.Model, &e.Timestamp, &e.Project, &e.GitBranch,
			&e.Input, &e.Output,
			&e.CacheRead, &e.CacheWrite5m, &e.CacheWrite1h,
		); err != nil {
			return nil, fmt.Errorf("scan unpriced batch row: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("unpriced batch rows: %w", err)
	}
	return out, nil
}

// ApplyReprice flips a batch of events from unpriced to priced and folds
// their newly known cost into the matching rollups, in one transaction.
//
// Idempotent and safe under concurrency. Each update is guarded by
// "AND priced = 0" and the rollup delta is applied only when that update
// actually changed a row, so a second pass, or a racing writer in
// another process, applies nothing rather than double-counting. The
// tokens and event_count are already in the rollup from the original
// insert, so the delta here is cost only, plus clearing one from the
// unpriced counter.
//
// Returns how many events were repriced by this call.
func (d *DB) ApplyReprice(ctx context.Context, priced []RepricedEvent) (int, error) {
	if len(priced) == 0 {
		return 0, nil
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	applied := 0
	for _, p := range priced {
		res, err := tx.ExecContext(ctx,
			`UPDATE events SET cost_usd = ?, priced = 1 WHERE uuid = ? AND priced = 0`,
			p.CostUSD, p.UUID)
		if err != nil {
			return 0, fmt.Errorf("reprice event %s: %w", p.UUID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("reprice rows affected %s: %w", p.UUID, err)
		}
		if n != 1 {
			// Someone else priced it first. Applying the rollup delta
			// now would double-count, so skip it entirely.
			continue
		}
		if err := applyRollupDelta(ctx, tx, p.Project, p.GitBranch,
			p.Timestamp, 0,
			0, 0, 0, 0, 0,
			p.CostUSD, -1,
		); err != nil {
			return 0, err
		}
		applied++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit reprice: %w", err)
	}
	return applied, nil
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
	// #nosec G202 -- placeholders contains only "?" bind markers (one per id); the
	// ids are passed as bound args, so this is parameterized, not injectable.
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
		       SUM(cost_usd), SUM(unpriced_count)
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
			&r.CostUSD, &r.UnpricedCount,
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
