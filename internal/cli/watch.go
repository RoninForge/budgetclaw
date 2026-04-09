package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/RoninForge/budgetclaw/internal/db"
	"github.com/RoninForge/budgetclaw/internal/enforcer"
	"github.com/RoninForge/budgetclaw/internal/ntfy"
	"github.com/RoninForge/budgetclaw/internal/paths"
	"github.com/RoninForge/budgetclaw/internal/pipeline"
	"github.com/RoninForge/budgetclaw/internal/watcher"
)

// newWatchCmd creates the `budgetclaw watch` command. This is
// the long-running daemon mode: it wires the full pipeline
// (parser → pricing → db → budget → enforcer → ntfy) and hands
// the handler to the fsnotify-based watcher. Blocks until
// SIGINT or SIGTERM is received, then cleans up.
func newWatchCmd() *cobra.Command {
	var verbose bool
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Run the long-lived watcher daemon",
		Long: `Start the budgetclaw watcher.

Tails $HOME/.claude/projects/**/*.jsonl, attributes every event
to its project and git branch, evaluates budget rules, and
enforces kill actions by SIGTERMing the matching Claude Code
process and writing a lockfile that catches silent relaunches.

The watcher runs in the foreground and blocks until Ctrl-C. Pair
with launchd/systemd for automatic startup.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWatch(cmd.Context(), cmd.OutOrStdout(), verbose)
		},
	}
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "log every parsed event")
	return cmd
}

func runWatch(parent context.Context, out io.Writer, verbose bool) error {
	// Wire SIGINT/SIGTERM → ctx cancel so Ctrl-C shuts down cleanly.
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Load config (may be empty).
	cfg, err := loadConfigOrDefault()
	if err != nil {
		return err
	}

	// Open state db (creates XDG dirs on first run).
	store, err := db.Open("")
	if err != nil {
		return fmt.Errorf("open state db: %w", err)
	}
	defer func() { _ = store.Close() }()

	// Initialize enforcer.
	enf, err := enforcer.NewEnforcer()
	if err != nil {
		return fmt.Errorf("init enforcer: %w", err)
	}

	// Prune any locks whose period already rolled over, so we
	// start clean.
	if n, err := enf.Locks.Prune(now()); err == nil && n > 0 {
		fmt.Fprintf(out, "pruned %d expired lock(s).\n", n)
	}

	// Notifier (noop if ntfy unconfigured).
	notifier := ntfy.New(ntfy.Options{
		Server: cfg.NtfyServer,
		Topic:  cfg.NtfyTopic,
	})

	// Resolve the Claude Code projects directory.
	projectsDir, err := paths.ClaudeProjectsDir()
	if err != nil {
		return fmt.Errorf("resolve claude projects dir: %w", err)
	}

	// Build the pipeline with a structured logger.
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level}))

	p := &pipeline.Pipeline{
		Config:   cfg,
		DB:       store,
		Enforcer: enf,
		Notifier: notifier,
		Logger:   logger,
	}

	// Create the watcher with the pipeline's Handle method.
	w, err := watcher.New(projectsDir, p.Handle, watcher.Options{Logger: logger})
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}
	defer func() { _ = w.Close() }()

	fmt.Fprintf(out, "budgetclaw watching %s\n", projectsDir)
	if notifier.IsNoop() {
		fmt.Fprintln(out, "(ntfy alerts not configured; run `budgetclaw alerts setup` to enable)")
	}
	if len(cfg.Rules) == 0 {
		fmt.Fprintln(out, "(no budget rules yet; run `budgetclaw limit set --cap X` to add one)")
	}
	fmt.Fprintln(out, "press Ctrl-C to stop.")

	return w.Run(ctx)
}

// now is a package-level clock override point. Kept as a var so
// tests can stub it without exported machinery.
var now = realNow

func realNow() time.Time { return time.Now() }
