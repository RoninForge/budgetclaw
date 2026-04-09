package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/RoninForge/budgetclaw/internal/enforcer"
)

// newLocksCmd creates the `budgetclaw locks` parent command for
// inspecting active budget-breach locks. Currently only `list`
// is implemented; future subcommands may include `prune` for
// manual expired-lock cleanup.
func newLocksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "locks",
		Short: "Manage active budget-breach locks",
	}
	cmd.AddCommand(newLocksListCmd(), newLocksPathCmd())
	return cmd
}

func newLocksListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show every active budget-breach lock",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLocksList(cmd.OutOrStdout())
		},
	}
}

func runLocksList(out io.Writer) error {
	ls, err := enforcer.NewLockStore()
	if err != nil {
		return fmt.Errorf("open lock store: %w", err)
	}
	locks, err := ls.List()
	if err != nil {
		return fmt.Errorf("list locks: %w", err)
	}
	if len(locks) == 0 {
		fmt.Fprintln(out, "No active locks.")
		return nil
	}

	tw := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tBRANCH\tPERIOD\tCURRENT\tCAP\tLOCKED")
	for _, lk := range locks {
		fmt.Fprintf(tw, "%s\t%s\t%s\t$%.2f\t$%.2f\t%s\n",
			lk.Project, lk.Branch, lk.Period, lk.CurrentUSD, lk.CapUSD,
			lk.LockedAt.Format("2006-01-02 15:04"))
	}
	return tw.Flush()
}

func newLocksPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the directory where lock files are stored",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ls, err := enforcer.NewLockStore()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), ls.Dir())
			return nil
		},
	}
}
