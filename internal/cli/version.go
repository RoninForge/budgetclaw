package cli

import (
	"fmt"
	"io"
	"runtime"

	"github.com/RoninForge/budgetclaw/internal/version"
	"github.com/spf13/cobra"
)

// newVersionCmd returns the `budgetclaw version` subcommand. It prints
// the semantic version, git commit, build date, and Go runtime. Values
// are injected at build time via -ldflags -X; fall back to
// runtime/debug.ReadBuildInfo when the binary is built without ldflags
// (e.g. `go run ./cmd/budgetclaw version`).
func newVersionCmd() *cobra.Command {
	var short bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return printVersion(cmd.OutOrStdout(), short)
		},
	}
	cmd.Flags().BoolVar(&short, "short", false, "print only the version number")
	return cmd
}

func printVersion(w io.Writer, short bool) error {
	info := version.Get()
	if short {
		_, err := fmt.Fprintln(w, info.Version)
		return err
	}
	_, err := fmt.Fprintf(w,
		"budgetclaw %s\n  commit:  %s\n  built:   %s\n  go:      %s\n  os/arch: %s/%s\n",
		info.Version, info.Commit, info.BuildDate,
		runtime.Version(), runtime.GOOS, runtime.GOARCH,
	)
	return err
}
