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

	"github.com/spf13/cobra"

	"github.com/RoninForge/budgetclaw/internal/db"
	"github.com/RoninForge/budgetclaw/internal/parser"
	"github.com/RoninForge/budgetclaw/internal/pricing"
)

// newBackfillCmd creates the `budgetclaw backfill` subcommand. It
// re-scans every JSONL log under the user's Claude Code projects
// directory and inserts any missing rollups into the local state
// database. Idempotent on event UUID, so repeated runs are safe.
//
// The primary use case is recovering attribution after a release
// adds new model pricing: events the prior watcher saw but skipped
// (because the model was unknown) become attributable as soon as
// backfill is run with the new binary.
func newBackfillCmd() *cobra.Command {
	var (
		dir     string
		rebuild bool
	)
	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Re-scan historical JSONL logs to seed the rollup database",
		Long: `backfill walks $HOME/.claude/projects/**/*.jsonl, prices
every assistant event, and inserts the rollups into the local
state database. Safe to run repeatedly: events are deduped on
their UUID and the rollup is only incremented once.

Use after upgrading to a release that adds new model pricing —
historical events the prior watcher saw but skipped (because the
model was unknown) become attributable on the next run.

--rebuild truncates the events and rollups tables before scanning,
so a pricing correction is reflected in historical totals. Use
after a release fixes a wrong rate for a model that already has
rollups in the DB; without --rebuild, the old rate stays baked
into the rollup row because Insert is idempotent on uuid.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBackfill(cmd.Context(), cmd.OutOrStdout(), dir, rebuild)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "log directory to scan (default: $HOME/.claude/projects)")
	cmd.Flags().BoolVar(&rebuild, "rebuild", false, "wipe events + rollups before scanning (use after a pricing correction)")
	return cmd
}

// backfillStats accumulates the per-run summary returned to stdout.
type backfillStats struct {
	scanned     int            // assistant events parsed
	priced      int            // events successfully priced and forwarded to DB.Insert
	skipped     int            // events for which pricing returned ErrUnknownModel
	parseErrors int            // malformed JSONL lines we ignored
	dbErrors    int            // db.Insert failures (logged, then skipped)
	models      map[string]int // count per priceable model
	unknown     map[string]int // count per unpriceable model
}

func runBackfill(ctx context.Context, out io.Writer, dir string, rebuild bool) error {
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

	if rebuild {
		if err := store.Reset(ctx); err != nil {
			return fmt.Errorf("reset db: %w", err)
		}
		fmt.Fprintln(out, "wiped events + rollups (rebuild mode)")
	}

	stats := backfillStats{
		models:  make(map[string]int),
		unknown: make(map[string]int),
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(out, "No log directory at", dir, "— nothing to backfill.")
			return nil
		}
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

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

	fmt.Fprintf(out, "scanned %d events, priced %d, skipped %d (unknown model)",
		stats.scanned, stats.priced, stats.skipped)
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
		fmt.Fprintln(out, "Unknown models (events not attributed):")
		for _, k := range keys {
			fmt.Fprintf(out, "  %s: %d events\n", k, stats.unknown[k])
		}
		fmt.Fprintln(out, "Run `budgetclaw pricing diagnose` for the full per-model breakdown.")
	}

	return nil
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

		cost, perr := pricing.CostForModel(ev.Model, pricing.Usage{
			Input:        ev.InputTokens,
			Output:       ev.OutputTokens,
			CacheRead:    ev.CacheReadTokens,
			CacheWrite5m: ev.CacheCreation5mTokens,
			CacheWrite1h: ev.CacheCreation1hTokens,
		})
		if perr != nil {
			if errors.Is(perr, pricing.ErrUnknownModel) {
				stats.skipped++
				stats.unknown[ev.Model]++
				continue
			}
			stats.skipped++
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
