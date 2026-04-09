package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/RoninForge/budgetclaw/internal/budget"
	"github.com/RoninForge/budgetclaw/internal/paths"
)

// newInitCmd creates the `budgetclaw init` command. It creates
// every XDG directory budgetclaw writes to and drops a
// documented default config.toml if one does not exist.
// Re-running init is safe: existing config is never overwritten.
func newInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create XDG directories and write a default config",
		Long: `Initialize budgetclaw state.

Creates the XDG config, state, data, and cache directories and
writes a documented default config file at
$XDG_CONFIG_HOME/budgetclaw/config.toml if it does not yet exist.
Safe to re-run: existing config files are never overwritten.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd.OutOrStdout())
		},
	}
}

func runInit(out io.Writer) error {
	dirs := []struct {
		label string
		fn    func() (string, error)
	}{
		{"config", paths.ConfigDir},
		{"state", paths.StateDir},
		{"data", paths.DataDir},
		{"cache", paths.CacheDir},
	}

	resolved := make(map[string]string, len(dirs))
	for _, d := range dirs {
		dir, err := d.fn()
		if err != nil {
			return fmt.Errorf("resolve %s dir: %w", d.label, err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s dir %s: %w", d.label, dir, err)
		}
		resolved[d.label] = dir
	}

	cfgPath := filepath.Join(resolved["config"], "config.toml")
	existed := true
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		existed = false
		if err := budget.WriteDefault(cfgPath); err != nil {
			return fmt.Errorf("write default config: %w", err)
		}
	}

	fmt.Fprintln(out, "budgetclaw initialized.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Directories:")
	fmt.Fprintf(out, "  config: %s\n", resolved["config"])
	fmt.Fprintf(out, "  state:  %s\n", resolved["state"])
	fmt.Fprintf(out, "  data:   %s\n", resolved["data"])
	fmt.Fprintf(out, "  cache:  %s\n", resolved["cache"])
	fmt.Fprintln(out)

	if existed {
		fmt.Fprintf(out, "Config already present at %s (left unchanged).\n", cfgPath)
	} else {
		fmt.Fprintf(out, "Wrote default config to %s\n", cfgPath)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Next steps:")
	fmt.Fprintln(out, "  1. Edit the config:  budgetclaw config path")
	fmt.Fprintln(out, "  2. Set a budget:     budgetclaw limit set --period daily --cap 10")
	fmt.Fprintln(out, "  3. Start watching:   budgetclaw watch")
	return nil
}
