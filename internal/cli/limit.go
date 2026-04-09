package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/RoninForge/budgetclaw/internal/budget"
)

// newLimitCmd creates the `budgetclaw limit` parent command with
// set, list, and rm subcommands. Each subcommand is a thin
// wrapper over budget.AddLimit / LoadFile / RemoveLimit.
func newLimitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "limit",
		Short: "Manage budget limit rules",
	}
	cmd.AddCommand(newLimitSetCmd(), newLimitListCmd(), newLimitRmCmd())
	return cmd
}

func newLimitSetCmd() *cobra.Command {
	var (
		project string
		branch  string
		period  string
		capUSD  float64
		action  string
	)
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Add a new budget limit rule",
		Long: `Add a new [[limit]] rule to the config file. Missing
project/branch default to "*" (match everything). Missing period
defaults to daily. Missing action defaults to warn.

Examples:
  budgetclaw limit set --period daily --cap 10
  budgetclaw limit set --project myapp --branch main --period weekly --cap 30 --action kill`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLimitSet(cmd.OutOrStdout(), project, branch, period, capUSD, action)
		},
	}
	cmd.Flags().StringVar(&project, "project", "*", `project to match ("*" for all)`)
	cmd.Flags().StringVar(&branch, "branch", "*", `branch to match, glob OK ("*" for all)`)
	cmd.Flags().StringVar(&period, "period", "daily", "daily | weekly | monthly")
	cmd.Flags().Float64Var(&capUSD, "cap", 0, "cap in USD (required)")
	cmd.Flags().StringVar(&action, "action", "warn", "warn | kill")
	_ = cmd.MarkFlagRequired("cap")
	return cmd
}

func runLimitSet(out io.Writer, project, branch, periodStr string, capUSD float64, actionStr string) error {
	if capUSD < 0 {
		return fmt.Errorf("cap must be >= 0, got %v", capUSD)
	}

	period, err := parsePeriod(periodStr)
	if err != nil {
		return err
	}
	action, err := parseAction(actionStr)
	if err != nil {
		return err
	}

	rule := budget.Rule{
		Project: project,
		Branch:  branch,
		Period:  period,
		CapUSD:  capUSD,
		Action:  action,
	}

	path, err := configPath()
	if err != nil {
		return err
	}
	if err := budget.AddLimit(path, rule); err != nil {
		return fmt.Errorf("add limit: %w", err)
	}

	fmt.Fprintf(out, "added limit: project=%s branch=%s period=%s cap=$%.2f action=%s\n",
		project, branch, period, capUSD, action)
	fmt.Fprintf(out, "config: %s\n", path)
	return nil
}

func newLimitListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show active budget limit rules",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLimitList(cmd.OutOrStdout())
		},
	}
}

func runLimitList(out io.Writer) error {
	cfg, err := loadConfigOrDefault()
	if err != nil {
		return err
	}
	if len(cfg.Rules) == 0 {
		fmt.Fprintln(out, "No limit rules configured. Add one with `budgetclaw limit set`.")
		return nil
	}

	tw := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "#\tPROJECT\tBRANCH\tPERIOD\tCAP\tACTION")
	for i, r := range cfg.Rules {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t$%.2f\t%s\n",
			i+1, r.Project, r.Branch, r.Period, r.CapUSD, r.Action)
	}
	return tw.Flush()
}

func newLimitRmCmd() *cobra.Command {
	var (
		project string
		branch  string
		period  string
	)
	cmd := &cobra.Command{
		Use:   "rm",
		Short: "Remove a budget limit rule",
		Long: `Remove every [[limit]] rule matching (project, branch, period).
Empty project or branch match "*" (the default wildcard). The
match is exact — globs and wildcards are matched verbatim, not
expanded.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLimitRm(cmd.OutOrStdout(), project, branch, period)
		},
	}
	cmd.Flags().StringVar(&project, "project", "*", "project selector")
	cmd.Flags().StringVar(&branch, "branch", "*", "branch selector")
	cmd.Flags().StringVar(&period, "period", "daily", "daily | weekly | monthly")
	return cmd
}

func runLimitRm(out io.Writer, project, branch, periodStr string) error {
	period, err := parsePeriod(periodStr)
	if err != nil {
		return err
	}

	path, err := configPath()
	if err != nil {
		return err
	}

	n, err := budget.RemoveLimit(path, project, branch, period)
	if err != nil {
		return fmt.Errorf("remove limit: %w", err)
	}
	if n == 0 {
		fmt.Fprintln(out, "No matching limit to remove.")
		return nil
	}
	fmt.Fprintf(out, "removed %d limit(s).\n", n)
	return nil
}

// parsePeriod converts a user-facing string to the budget.Period
// enum with a helpful error message on mismatch.
func parsePeriod(s string) (budget.Period, error) {
	switch s {
	case "daily", "":
		return budget.PeriodDaily, nil
	case "weekly":
		return budget.PeriodWeekly, nil
	case "monthly":
		return budget.PeriodMonthly, nil
	default:
		return 0, fmt.Errorf("period must be daily|weekly|monthly, got %q", s)
	}
}

func parseAction(s string) (budget.Action, error) {
	switch s {
	case "warn", "":
		return budget.ActionWarn, nil
	case "kill":
		return budget.ActionKill, nil
	default:
		return 0, fmt.Errorf("action must be warn|kill, got %q", s)
	}
}
