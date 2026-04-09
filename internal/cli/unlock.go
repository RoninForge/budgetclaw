package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/RoninForge/budgetclaw/internal/enforcer"
)

// newUnlockCmd creates the `budgetclaw unlock` command for
// manually releasing a budget-breach lock before its auto-expiry.
// Useful after increasing a cap or when the user intentionally
// wants to resume work on a locked project.
func newUnlockCmd() *cobra.Command {
	var branch string
	cmd := &cobra.Command{
		Use:   "unlock <project>",
		Short: "Release a budget-breach lock",
		Long: `Remove the active lock for (project, branch) so that the
next Claude Code run in that project is not killed on startup.

Branch defaults to "main". Use --branch for feature branches.

Locks also auto-expire when their budget period rolls over, so
manual unlock is only needed when you want to resume work early
or when you have increased the cap.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUnlock(cmd.OutOrStdout(), args[0], branch)
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "main", "branch to unlock")
	return cmd
}

func runUnlock(out io.Writer, project, branch string) error {
	ls, err := enforcer.NewLockStore()
	if err != nil {
		return fmt.Errorf("open lock store: %w", err)
	}
	lk, err := ls.IsLocked(project, branch)
	if err != nil {
		return fmt.Errorf("check lock: %w", err)
	}
	if lk == nil {
		fmt.Fprintf(out, "%s/%s is not locked.\n", project, branch)
		return nil
	}
	if err := ls.Release(project, branch); err != nil {
		return fmt.Errorf("release lock: %w", err)
	}
	fmt.Fprintf(out, "unlocked %s/%s (was: %s)\n", project, branch, lk.Reason)
	return nil
}
