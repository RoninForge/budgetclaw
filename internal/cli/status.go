package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/RoninForge/budgetclaw/internal/budget"
	"github.com/RoninForge/budgetclaw/internal/db"
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

	// Aggregate by (project, branch) across the three ranges.
	type key struct{ project, branch string }
	type agg struct{ day, week, month float64 }
	rows := make(map[key]*agg)

	merge := func(list []db.Rollup, pick func(*agg, float64)) {
		for _, r := range list {
			k := key{r.Project, r.GitBranch}
			if rows[k] == nil {
				rows[k] = &agg{}
			}
			pick(rows[k], r.CostUSD)
		}
	}
	merge(today, func(a *agg, v float64) { a.day = v })
	merge(week, func(a *agg, v float64) { a.week = v })
	merge(month, func(a *agg, v float64) { a.month = v })

	if len(rows) == 0 {
		fmt.Fprintln(out, "No activity tracked yet. Run `budgetclaw watch` to start.")
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
	for _, k := range keys {
		a := rows[k]
		fmt.Fprintf(tw, "%s\t%s\t$%.2f\t$%.2f\t$%.2f\n",
			k.project, k.branch, a.day, a.week, a.month)
		sumDay += a.day
		sumWeek += a.week
		sumMonth += a.month
	}

	// Only show totals row if there's more than one (project, branch).
	if len(keys) > 1 {
		fmt.Fprintf(tw, "TOTAL\t\t$%.2f\t$%.2f\t$%.2f\n", sumDay, sumWeek, sumMonth)
	}
	return tw.Flush()
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
