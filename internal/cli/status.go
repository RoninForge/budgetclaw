package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/RoninForge/budgetclaw/internal/budget"
	"github.com/RoninForge/budgetclaw/internal/db"
	"github.com/RoninForge/budgetclaw/internal/pricing"
)

// newStatusCmd creates the `budgetclaw status` command. It opens
// the state database and prints per-project/per-branch spend for
// today, this week, and this month.
func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show current spend by project and branch",
		Long: `Print a table of current spend per (project, branch) across
today, this week, and this month. Periods are computed in the
timezone from your config file (UTC by default).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd.Context(), cmd.OutOrStdout())
		},
	}
}

func runStatus(ctx context.Context, out io.Writer) error {
	cfg, err := loadConfigOrDefault()
	if err != nil {
		return err
	}

	store, err := db.Open("")
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now()
	todayStart, todayEnd := budget.PeriodBounds(budget.PeriodDaily, now, cfg.Timezone)
	weekStart, weekEnd := budget.PeriodBounds(budget.PeriodWeekly, now, cfg.Timezone)
	monthStart, monthEnd := budget.PeriodBounds(budget.PeriodMonthly, now, cfg.Timezone)

	today, err := store.StatusByProject(ctx, todayStart, todayEnd)
	if err != nil {
		return fmt.Errorf("query today: %w", err)
	}
	week, err := store.StatusByProject(ctx, weekStart, weekEnd)
	if err != nil {
		return fmt.Errorf("query week: %w", err)
	}
	month, err := store.StatusByProject(ctx, monthStart, monthEnd)
	if err != nil {
		return fmt.Errorf("query month: %w", err)
	}

	// Aggregate by (project, branch) across the three ranges. Each period
	// carries whether it contains unpriced events, so an incomplete figure
	// can be marked as a minimum rather than presented as the truth.
	type key struct{ project, branch string }
	type agg struct {
		day, week, month             float64
		dayPart, weekPart, monthPart bool
	}
	rows := make(map[key]*agg)

	merge := func(list []db.Rollup, pick func(*agg, float64, bool)) {
		for _, r := range list {
			k := key{r.Project, r.GitBranch}
			if rows[k] == nil {
				rows[k] = &agg{}
			}
			pick(rows[k], r.CostUSD, r.UnpricedCount > 0)
		}
	}
	merge(today, func(a *agg, v float64, p bool) { a.day, a.dayPart = v, p })
	merge(week, func(a *agg, v float64, p bool) { a.week, a.weekPart = v, p })
	merge(month, func(a *agg, v float64, p bool) { a.month, a.monthPart = v, p })

	if len(rows) == 0 {
		fmt.Fprintln(out, "No activity tracked yet. Run `budgetclaw watch` to start.")
		maybeShowDiscovery(out)
		return nil
	}

	// Sort deterministically: project first, then branch.
	keys := make([]key, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].project != keys[j].project {
			return keys[i].project < keys[j].project
		}
		return keys[i].branch < keys[j].branch
	})

	tw := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tBRANCH\tTODAY\tWEEK\tMONTH")

	var sumDay, sumWeek, sumMonth float64
	var anyDay, anyWeek, anyMonth bool
	for _, k := range keys {
		a := rows[k]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			k.project, k.branch,
			money(a.day, a.dayPart), money(a.week, a.weekPart), money(a.month, a.monthPart))
		sumDay += a.day
		sumWeek += a.week
		sumMonth += a.month
		anyDay = anyDay || a.dayPart
		anyWeek = anyWeek || a.weekPart
		anyMonth = anyMonth || a.monthPart
	}

	// Only show totals row if there's more than one (project, branch).
	if len(keys) > 1 {
		fmt.Fprintf(tw, "TOTAL\t\t%s\t%s\t%s\n",
			money(sumDay, anyDay), money(sumWeek, anyWeek), money(sumMonth, anyMonth))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// Name the unpriced backlog, if any. Runs after the table so the
	// numbers stay the first thing read.
	if err := showUnpriced(ctx, out, store); err != nil {
		return err
	}

	// Best-effort: if this repo points at a Goei team and this machine has not joined,
	// show the one-time join disclosure. Never affects the status command's success.
	maybeShowDiscovery(out)
	return nil
}

// money formats a dollar figure, appending "+" when the period it
// covers contains events that are stored but not yet priced.
//
// The marker reads as "at least this much". It is preferred over a
// footnote symbol because it is self-describing at a glance and survives
// being pasted into an issue without its legend.
func money(v float64, partial bool) string {
	if partial {
		return fmt.Sprintf("$%.2f+", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

// showUnpriced prints the unpriced-backlog block when the database holds
// events this build could not price. It writes nothing at all in the
// healthy case, so normal output is unchanged.
//
// Counts and token volume only: estimating dollars for a model whose
// rate we do not have would replace a known unknown with a fabricated
// number.
func showUnpriced(ctx context.Context, out io.Writer, store *db.DB) error {
	has, err := store.HasUnpriced(ctx)
	if err != nil {
		return fmt.Errorf("probe unpriced: %w", err)
	}
	if !has {
		return nil
	}

	models, err := store.UnpricedByModel(ctx)
	if err != nil {
		return fmt.Errorf("query unpriced: %w", err)
	}

	var total int
	for _, m := range models {
		total += m.EventCount
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "WARNING: %s stored events are not priced. Figures marked + are minimums.\n",
		humanCount(total))

	tw := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	for _, m := range models {
		fmt.Fprintf(tw, "  %s\t%s events\t%s tokens\tfirst seen %s\n",
			m.Model, humanCount(m.EventCount), humanCount(m.Tokens), m.FirstSeen)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	tag, _ := pricing.Provenance()
	if tag != "" {
		fmt.Fprintf(out, "This build's pricing table (%s) has no rate for the model(s) above.\n", tag)
	}
	fmt.Fprintln(out, "The events are stored in full and price automatically once the table")
	fmt.Fprintln(out, "learns them. Nothing is lost. Upgrade: brew upgrade budgetclaw")
	return nil
}

// humanCount renders an integer with thousands separators, so a six
// digit event count is legible at a glance.
func humanCount(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// loadConfigOrDefault loads config.toml or returns a sane empty
// config (UTC timezone, zero rules) when the file does not exist.
// Used by every command that reads but does not modify the config.
func loadConfigOrDefault() (*budget.Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	cfg, err := budget.LoadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &budget.Config{Timezone: time.UTC}, nil
		}
		return nil, fmt.Errorf("load config: %w", err)
	}
	if cfg.Timezone == nil {
		cfg.Timezone = time.UTC
	}
	return cfg, nil
}
