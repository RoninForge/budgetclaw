package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/RoninForge/budgetclaw/internal/db"
	"github.com/RoninForge/budgetclaw/internal/parser"
	"github.com/RoninForge/budgetclaw/internal/pricing"
	"github.com/RoninForge/budgetclaw/internal/reconcile"
)

// newBackfillCmd creates the `budgetclaw backfill` subcommand. It
// re-scans every JSONL log under the user's Claude Code projects
// directory and inserts any missing rollups into the local state
// database. Idempotent, so repeated runs are safe: the db dedupes
// each API response on (message_id, request_id), falling back to the
// line uuid for older schemas.
//
// The primary use case is recovering attribution after a release
// adds new model pricing: events the prior watcher saw but skipped
// (because the model was unknown) become attributable as soon as
// backfill is run with the new binary. Use --rebuild after upgrading
// from a binary that double-counted streamed responses (pre-dedupe),
// which wipes and replays so the historical totals are corrected.
func newBackfillCmd() *cobra.Command {
	var (
		dir     string
		rebuild bool
		force   bool
	)
	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Re-scan historical JSONL logs to seed the rollup database",
		Long: `backfill walks $HOME/.claude/projects/**/*.jsonl, prices
every assistant event, and inserts the rollups into the local
state database. Safe to run repeatedly: each API response is deduped
on (message_id, request_id) so the rollup counts it once. Claude Code
writes the same response on several lines, so this is what keeps the
totals honest.

Use after upgrading to a release that adds new model pricing -
historical events the prior watcher could not price become
attributable on the next run.

An event whose model has no rate is stored anyway, with its token
counts and a cost of zero, so nothing is lost while the pricing
table catches up. Those events are priced automatically once the
model is known.

Re-pricing is point-in-time: each event is priced at the rate that
was in effect on its own timestamp, not at today's rate, so a rebuilt
rollup reflects what the model actually cost when the event happened.

--rebuild truncates the events and rollups tables before scanning,
then replays from the logs. Its remaining use is repairing a database
written by a binary that double-counted streamed responses (pre-dedupe):
the wipe clears the old uuid-keyed rows so the rescan produces a fully
deduped dataset. A pricing correction no longer needs it, because
stored events reprice in place.

--rebuild is destructive and is refused when it would destroy history
the logs can no longer replay. Claude Code prunes its session logs
after roughly a month while this database keeps everything, so a
rebuild can silently discard months of spend. Override with --force
only if you accept losing it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if force && !rebuild {
				return errors.New("--force only applies to --rebuild")
			}
			return runBackfill(cmd.Context(), cmd.OutOrStdout(), dir, rebuild, force)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "log directory to scan (default: $HOME/.claude/projects)")
	cmd.Flags().BoolVar(&rebuild, "rebuild", false, "wipe events + rollups before scanning (destructive; repairs a pre-dedupe database)")
	cmd.Flags().BoolVar(&force, "force", false, "allow --rebuild to discard history the logs cannot replay")
	return cmd
}

// backfillStats accumulates the per-run summary returned to stdout.
type backfillStats struct {
	scanned       int            // assistant events parsed
	priced        int            // events successfully priced and stored
	unpriced      int            // events stored with no rate (tokens kept, cost unknown)
	parseErrors   int            // malformed JSONL lines we ignored
	pricingErrors int            // unexpected pricing failures (not unknown-model)
	dbErrors      int            // db insert failures (logged, then skipped)
	models        map[string]int // count per priceable model
	unknown       map[string]int // count per unpriceable model
}

func runBackfill(ctx context.Context, out io.Writer, dir string, rebuild, force bool) error {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("home dir: %w", err)
		}
		dir = filepath.Join(home, ".claude", "projects")
	}

	store, err := db.Open("")
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = store.Close() }()

	// Open the log directory before anything destructive happens. A
	// --rebuild against a missing directory would otherwise wipe the
	// database and then find nothing to replay from.
	root, err := os.OpenRoot(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(out, "No log directory at", dir, "- nothing to backfill.")
			return nil
		}
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	if rebuild {
		if err := checkRebuildSafe(ctx, out, store, root, force); err != nil {
			return err
		}
		if err := store.Reset(ctx); err != nil {
			return fmt.Errorf("reset db: %w", err)
		}
		fmt.Fprintln(out, "wiped events + rollups (rebuild mode)")
	} else {
		// Price any stored backlog from the rows themselves before
		// touching the logs. Forced, because an explicit backfill is a
		// repair request: run the pass even if the pricing table has not
		// changed since the last one. This also means a backfill recovers
		// spend whose logs Claude Code has already pruned.
		if res, rerr := reconcile.Run(ctx, store, true); rerr != nil {
			fmt.Fprintf(out, "warning: reprice pass failed: %v\n", rerr)
		} else if res.Any() {
			fmt.Fprintln(out, res.Summary())
		}
	}

	stats := backfillStats{
		models:  make(map[string]int),
		unknown: make(map[string]int),
	}

	walkErr := fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		return scanFileIntoDB(ctx, root, path, store, &stats)
	})
	if walkErr != nil {
		return fmt.Errorf("walk %s: %w", dir, walkErr)
	}

	fmt.Fprintf(out, "scanned %d events, priced %d, stored unpriced %d (unknown model)",
		stats.scanned, stats.priced, stats.unpriced)
	if stats.parseErrors > 0 {
		fmt.Fprintf(out, ", %d malformed line(s)", stats.parseErrors)
	}
	if stats.dbErrors > 0 {
		fmt.Fprintf(out, ", %d db error(s)", stats.dbErrors)
	}
	fmt.Fprintln(out)

	if len(stats.unknown) > 0 {
		// Sort for deterministic output so test golden files don't
		// flap on map iteration order.
		keys := make([]string, 0, len(stats.unknown))
		for k := range stats.unknown {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintln(out, "Unknown models (stored with tokens, cost not yet known):")
		for _, k := range keys {
			fmt.Fprintf(out, "  %s: %d events\n", k, stats.unknown[k])
		}
		fmt.Fprintln(out, "Nothing is lost. These price automatically once the pricing table")
		fmt.Fprintln(out, "learns the model. Run `budgetclaw pricing diagnose` for the breakdown.")
	}

	return nil
}

// oldestLogDay returns the earliest day a backfill could replay from the
// logs, as YYYY-MM-DD, or "" when the logs hold no billable event at all.
//
// Claude Code appends to a session file in time order, so the first
// parseable assistant event in a file bounds that file. Scanning stops at
// that line, which keeps this O(number of files) rather than O(bytes).
func oldestLogDay(root *os.Root) (string, error) {
	oldest := ""
	err := fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		day, derr := firstEventDay(root, path)
		if derr != nil {
			// An unreadable file tells us nothing about coverage. Skip
			// it rather than abort: the guard errs toward refusing.
			return nil //nolint:nilerr // deliberately tolerant, see comment
		}
		if day == "" {
			return nil
		}
		if oldest == "" || day < oldest {
			oldest = day // both are YYYY-MM-DD, so string order is date order
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return oldest, nil
}

// firstEventDay returns the UTC day of the first billable event in one
// log file, or "" if it holds none.
func firstEventDay(root *os.Root, path string) (string, error) {
	f, err := root.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		ev, perr := parser.Parse(scanner.Bytes())
		if perr != nil || ev == nil {
			continue // user lines, tool results, malformed lines
		}
		return ev.Timestamp.UTC().Format("2006-01-02"), nil
	}
	return "", scanner.Err()
}

// checkRebuildSafe refuses a --rebuild that would destroy history the
// logs can no longer replay.
//
// --rebuild wipes events and rollups and replays from the logs, but
// Claude Code prunes those after roughly a month while the database
// keeps everything. On 2026-07-27 a real database held rollups back to
// 05-13 while the logs only reached 06-26: a rebuild would have silently
// destroyed six weeks of spend with no way to get it back.
//
// Returns a non-nil error to abort. force bypasses the check.
func checkRebuildSafe(ctx context.Context, out io.Writer, store *db.DB, root *os.Root, force bool) error {
	oldestDB, err := store.OldestRollupDay(ctx)
	if err != nil {
		return err
	}
	if oldestDB == "" {
		return nil // no history to lose
	}

	oldestLog, err := oldestLogDay(root)
	if err != nil {
		return fmt.Errorf("scan logs for coverage: %w", err)
	}

	// Covered: the logs reach at least as far back as the database.
	if oldestLog != "" && oldestLog <= oldestDB {
		fmt.Fprintf(out, "rebuild coverage check: logs reach back to %s, database starts %s. Safe to replay.\n",
			oldestLog, oldestDB)
		return nil
	}

	if force {
		fmt.Fprintf(out, "warning: --force given, discarding database history before %s.\n", horizon(oldestLog))
		return nil
	}

	return &rebuildUnsafeError{oldestDB: oldestDB, oldestLog: oldestLog}
}

// horizon renders the earliest day a replay could restore.
func horizon(oldestLog string) string {
	if oldestLog == "" {
		return "today (the logs hold no billable events)"
	}
	return oldestLog
}

// rebuildUnsafeError aborts a destructive rebuild and explains exactly
// what would be lost and how to proceed anyway.
type rebuildUnsafeError struct {
	oldestDB  string
	oldestLog string
}

func (e *rebuildUnsafeError) Error() string {
	var b strings.Builder
	b.WriteString("refusing --rebuild: the database holds history the logs can no longer replay.\n\n")
	fmt.Fprintf(&b, "  oldest day in database:  %s\n", e.oldestDB)
	if e.oldestLog == "" {
		b.WriteString("  oldest day in logs:      none found\n\n")
		b.WriteString("The logs hold no billable events, so a rebuild would wipe the\n")
		b.WriteString("database and replay nothing at all.\n")
	} else {
		fmt.Fprintf(&b, "  oldest day in logs:      %s\n\n", e.oldestLog)
		b.WriteString("Claude Code prunes its session logs after roughly a month. A rebuild\n")
		b.WriteString("replays only from the logs, so ")
		if days := daysBetween(e.oldestDB, e.oldestLog); days > 0 {
			fmt.Fprintf(&b, "all %d days ", days)
		}
		fmt.Fprintf(&b, "before %s would be\npermanently lost.\n", e.oldestLog)
	}
	b.WriteString("\nYou almost never need --rebuild. Plain `budgetclaw backfill` is additive\n")
	b.WriteString("and dedup-safe, and events stored unpriced reprice automatically after\n")
	b.WriteString("an upgrade. If you accept losing that history, re-run with:\n\n")
	b.WriteString("  budgetclaw backfill --rebuild --force\n")
	return b.String()
}

// daysBetween counts whole days between two YYYY-MM-DD strings, or 0 if
// either fails to parse. Only used to sharpen the warning text.
func daysBetween(from, to string) int {
	const layout = "2006-01-02"
	a, err := time.Parse(layout, from)
	if err != nil {
		return 0
	}
	b, err := time.Parse(layout, to)
	if err != nil {
		return 0
	}
	return int(b.Sub(a).Hours() / 24)
}

// scanFileIntoDB reads one JSONL file via the rooted FS, parses each
// line, prices priceable events, and inserts them through the DB.
// Errors that affect a single line are accumulated into stats and
// the scan continues; only catastrophic IO errors abort.
func scanFileIntoDB(ctx context.Context, root *os.Root, path string, store *db.DB, stats *backfillStats) error {
	f, err := root.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		ev, perr := parser.Parse(scanner.Bytes())
		if perr != nil {
			stats.parseErrors++
			continue
		}
		if ev == nil {
			continue
		}
		stats.scanned++

		// Re-price at the rate effective on the event's own timestamp,
		// not at "now", so a rebuilt rollup reflects the price the user
		// actually paid when the event happened.
		cost, perr := pricing.CostForModelAt(ev.Model, ev.Timestamp, pricing.Usage{
			Input:        ev.InputTokens,
			Output:       ev.OutputTokens,
			CacheRead:    ev.CacheReadTokens,
			CacheWrite5m: ev.CacheCreation5mTokens,
			CacheWrite1h: ev.CacheCreation1hTokens,
		})
		if perr != nil {
			// ErrUnknownModel (not in the table) and ErrNoRateAtTime
			// (known model, no interval covers the event's timestamp)
			// are both unpriceable. Store the event anyway so its tokens
			// survive: a later release can price it from the stored row,
			// long after Claude Code has pruned the log we read it from.
			if !errors.Is(perr, pricing.ErrUnknownModel) && !errors.Is(perr, pricing.ErrNoRateAtTime) {
				// Unexpected pricing failure. Still store it; losing the
				// event would be strictly worse than recording it unpriced.
				stats.pricingErrors++
			}
			if err := store.InsertUnpriced(ctx, ev); err != nil {
				stats.dbErrors++
				continue
			}
			stats.unpriced++
			stats.unknown[ev.Model]++
			continue
		}

		if err := store.Insert(ctx, ev, cost); err != nil {
			stats.dbErrors++
			continue
		}
		stats.priced++
		stats.models[ev.Model]++
	}
	return scanner.Err()
}
