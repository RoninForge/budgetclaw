package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/RoninForge/budgetclaw/internal/parser"
	"github.com/RoninForge/budgetclaw/internal/pricing"
)

// newPricingCmd creates the `budgetclaw pricing` parent command. It
// surfaces the embedded model pricing table to humans and to the
// weekly pricing-audit GitHub Action so drift is visible without
// digging through Go source.
func newPricingCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pricing",
		Short: "Inspect the model pricing table and diagnose drift",
	}
	cmd.AddCommand(newPricingListCmd(), newPricingDiagnoseCmd())
	return cmd
}

func newPricingListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List every model ID in the embedded pricing table",
		Long: `Print every model ID that budgetclaw can price. Use --json to
emit a machine-readable array (used by the pricing-audit workflow).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			models := pricing.KnownModels()
			out := cmd.OutOrStdout()
			if asJSON {
				return json.NewEncoder(out).Encode(models)
			}
			for _, m := range models {
				fmt.Fprintln(out, m)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON array instead of one model per line")
	return cmd
}

func newPricingDiagnoseCmd() *cobra.Command {
	var (
		dir    string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "diagnose",
		Short: "Scan local Claude Code logs for models missing from the pricing table",
		Long: `diagnose walks $HOME/.claude/projects/**/*.jsonl, counts the
models seen, and flags any that the pricing table does not know
about. Use this when budgetclaw status looks suspiciously low —
silent unknown-model events are the most common cause of
under-attribution.

The exit code is 0 when every observed model is priceable and 2
when at least one is missing. Useful in scripts:

  budgetclaw pricing diagnose --json | jq '.[] | select(.priced | not)'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPricingDiagnose(cmd.OutOrStdout(), dir, asJSON)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "log directory to scan (default: $HOME/.claude/projects)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON output for scripting")
	return cmd
}

// pricingDiagnoseRow is the per-model row emitted by diagnose. The
// JSON tags lock the field names so downstream consumers (the
// pricing-audit workflow, end-user scripts) have a stable shape.
type pricingDiagnoseRow struct {
	Model  string `json:"model"`
	Events int    `json:"events"`
	Priced bool   `json:"priced"`
}

// errMissingModels signals that the diagnose scan found at least one
// model not in the pricing table. The runner uses this to choose a
// non-zero exit code without garbling the human-readable output that
// already prints above.
type errMissingModels struct{ count int }

func (e errMissingModels) Error() string {
	return fmt.Sprintf("%d unknown model(s) detected", e.count)
}

func runPricingDiagnose(out io.Writer, dir string, asJSON bool) error {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("home dir: %w", err)
		}
		dir = filepath.Join(home, ".claude", "projects")
	}

	counts, err := scanModelsFromJSONL(dir)
	if err != nil {
		return err
	}

	known := make(map[string]bool, len(pricing.KnownModels()))
	for _, m := range pricing.KnownModels() {
		known[m] = true
	}

	rows := make([]pricingDiagnoseRow, 0, len(counts))
	for m, n := range counts {
		rows = append(rows, pricingDiagnoseRow{Model: m, Events: n, Priced: known[m]})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Model < rows[j].Model })

	missing := make([]string, 0)
	for _, r := range rows {
		if !r.Priced {
			missing = append(missing, r.Model)
		}
	}

	if asJSON {
		if err := json.NewEncoder(out).Encode(rows); err != nil {
			return err
		}
		if len(missing) > 0 {
			return errMissingModels{count: len(missing)}
		}
		return nil
	}

	if len(rows) == 0 {
		fmt.Fprintln(out, "No assistant events found under", dir)
		return nil
	}

	tw := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tEVENTS\tPRICED")
	for _, r := range rows {
		status := "yes"
		if !r.Priced {
			status = "MISSING"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\n", r.Model, r.Events, status)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(missing) > 0 {
		fmt.Fprintf(out, "\n%d model(s) missing from pricing table: %s\n",
			len(missing), strings.Join(missing, ", "))
		fmt.Fprintln(out, "Open an issue at https://github.com/RoninForge/budgetclaw/issues")
		fmt.Fprintln(out, "with the model ID(s) so we can add the rates.")
		return errMissingModels{count: len(missing)}
	}
	return nil
}

// scanModelsFromJSONL walks dir, parses every .jsonl line, and
// returns a map of model ID to assistant-event count. Non-existent
// or empty dirs yield an empty map without error so a fresh install
// reports cleanly rather than panicking.
//
// Filesystem reads are scoped through os.Root so a symlink under
// the user's $HOME/.claude/projects directory cannot redirect the
// scanner outside that root (gosec G122). The user's logs are
// already on a trusted local FS, but the boundary is cheap.
func scanModelsFromJSONL(dir string) (map[string]int, error) {
	counts := make(map[string]int)

	root, err := os.OpenRoot(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return counts, nil
		}
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		f, err := root.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()

		scanner := bufio.NewScanner(f)
		// Some Claude Code lines (large tool inputs) exceed the
		// default 64KB scanner buffer. Bump to 8 MiB so we don't
		// silently drop long lines.
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			ev, perr := parser.Parse(scanner.Bytes())
			if perr != nil || ev == nil {
				continue
			}
			counts[ev.Model]++
		}
		return scanner.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", dir, err)
	}
	return counts, nil
}
