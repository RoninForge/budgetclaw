// Package cli wires the cobra command tree for budgetclaw.
//
// The root command is intentionally thin: it parses global flags,
// configures logging, and delegates to subcommands. Subcommands live in
// sibling files inside this package and register themselves in init().
package cli

import (
	"github.com/spf13/cobra"
)

// rootCmd is the top-level cobra command. It is created lazily by
// newRootCmd so tests can build isolated command trees without touching
// package state.
var rootCmd = newRootCmd()

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "budgetclaw",
		Short: "Local spend monitor for Claude Code",
		Long: `budgetclaw watches the JSONL session logs Claude Code writes locally,
attributes each tool-call's token cost to a project and git branch, and
enforces budget caps by sending SIGTERM to the client process on breach.

It never touches API traffic. Zero key, zero prompts, zero latency added.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(
		newVersionCmd(),
		newInitCmd(),
		newStatusCmd(),
		newLimitCmd(),
		newAlertsCmd(),
		newUnlockCmd(),
		newLocksCmd(),
		newConfigCmd(),
		newWatchCmd(),
		newPricingCmd(),
	)
	return cmd
}

// Execute runs the root command with os.Args. It is the single entry
// point called by cmd/budgetclaw/main.go.
func Execute() error {
	return rootCmd.Execute()
}
