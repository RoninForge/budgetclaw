package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/RoninForge/budgetclaw/internal/budget"
)

// newPrsCmd creates the `budgetclaw prs` command tree: the opt-in and status for
// cost-per-PR, where `budgetclaw sync` reads local git metadata (branch and merge/squash
// info) so Goei can attribute cost per pull request. Off by default: git is a new data
// source beyond the Claude Code logs, so it is never read until you turn it on.
func newPrsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prs",
		Short: "Manage cost-per-PR (send local git PR metadata with sync)",
		Long: `Cost-per-PR attributes your Claude Code spend to pull requests. With it on,
budgetclaw reads local git for the repos you already have spend in and sends
content-free PR metadata with your next sync: the PR number, base branch,
commit count, and diff size. Never commit messages, never code.

It is opt-in: git is read only after you turn it on.

  budgetclaw prs on      # send git PR metadata with sync
  budgetclaw prs off     # stop reading git and sending PR metadata
  budgetclaw prs status  # show whether it is on`,
	}
	cmd.AddCommand(newPrsOnCmd(), newPrsOffCmd(), newPrsStatusCmd())
	return cmd
}

func newPrsOnCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "on",
		Short: "Turn cost-per-PR git collection on",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPrsSet(cmd.OutOrStdout(), true)
		},
	}
}

func newPrsOffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "off",
		Short: "Turn cost-per-PR git collection off",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPrsSet(cmd.OutOrStdout(), false)
		},
	}
}

func runPrsSet(out io.Writer, on bool) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := budget.SetCollectGit(path, on); err != nil {
		return fmt.Errorf("update config: %w", err)
	}
	if on {
		fmt.Fprintln(out, "Cost-per-PR is ON. `budgetclaw sync` will read local git for repos you have spend in")
		fmt.Fprintln(out, "and send content-free PR metadata (number, base, commit count, diff size).")
	} else {
		fmt.Fprintln(out, "Cost-per-PR is OFF. No git is read and no PR metadata is sent.")
	}
	fmt.Fprintf(out, "config: %s\n", path)
	return nil
}

func newPrsStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether cost-per-PR is on",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfigOrDefault()
			if err != nil {
				return err
			}
			state := "off"
			if cfg.CollectGit {
				state = "on"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Cost-per-PR: %s\n", state)
			return nil
		},
	}
}
