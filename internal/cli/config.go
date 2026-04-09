package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newConfigCmd creates the `budgetclaw config` parent command for
// config-related diagnostic queries. Currently only `path` is
// implemented.
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect budgetclaw config",
	}
	cmd.AddCommand(newConfigPathCmd())
	return cmd
}

func newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the full path of the config file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := configPath()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), p)
			return nil
		},
	}
}
