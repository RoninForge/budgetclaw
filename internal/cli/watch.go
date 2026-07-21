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

	"github.com/RoninForge/budgetclaw/internal/budget"
	"github.com/RoninForge/budgetclaw/internal/db"
	"github.com/RoninForge/budgetclaw/internal/enforcer"
	"github.com/RoninForge/budgetclaw/internal/goei"
	"github.com/RoninForge/budgetclaw/internal/ntfy"
	"github.com/RoninForge/budgetclaw/internal/paths"
	"github.com/RoninForge/budgetclaw/internal/pipeline"
	"github.com/RoninForge/budgetclaw/internal/policy"
	"github.com/RoninForge/budgetclaw/internal/watcher"
)

// guardPollInterval is how often `watch` refreshes remote Guard Mode
// policies. The plan calls for 5-15 minutes; 10 keeps caps fresh without
// hammering the server.
const guardPollInterval = 10 * time.Minute

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
		Machine:  resolveMachine("", cfg.GoeiMachine),
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

	// Guard Mode: enforce remote team policies, but only when the user has
	// explicitly opted in. A device never silently obeys a server.
	if cfg.AcceptRemotePolicies {
		if cached, err := policy.Load(); err == nil && cached != nil && len(cached.Policies) > 0 {
			p.SetGuardPolicies(policy.LocalExact(cached.Policies))
			fmt.Fprintf(out, "guard mode on: %d cached remote polic(ies) loaded\n", len(cached.Policies))
		}
		if goei.ValidToken(cfg.GoeiToken) {
			go runGuardPoller(ctx, p, notifier, cfg, logger)
		} else {
			fmt.Fprintln(out, "(guard mode on but no Goei token; add one with `budgetclaw sync --save --token ...`)")
		}
	}

	fmt.Fprintln(out, "press Ctrl-C to stop.")

	return w.Run(ctx)
}

// runGuardPoller periodically refreshes remote Guard Mode policies from Goei
// and hands the locally-enforceable ones to the pipeline. It also fires the
// warn-only, team-aggregate caps (which no single machine can kill on). The
// last-known policy set stays cached and enforced across a network blip; a
// conditional ETag request keeps an unchanged poll cheap.
func runGuardPoller(ctx context.Context, p *pipeline.Pipeline, notifier *ntfy.Client, cfg *budget.Config, log *slog.Logger) {
	client := goei.New(cfg.GoeiEndpoint, cfg.GoeiToken)
	etag := ""
	if cached, err := policy.Load(); err == nil && cached != nil {
		etag = cached.ETag
	}
	// Dedup a team-aggregate warn to once per (policy, period) for this run.
	warned := make(map[string]bool)

	pull := func() {
		resp, newETag, notModified, err := client.PullPolicies(ctx, etag)
		if err != nil {
			log.Warn("guard: policy pull failed", "err", err)
			return
		}
		if notModified {
			return
		}
		etag = newETag
		bundle := policy.BundleFromResponse(resp, newETag, now().UTC().Format(time.RFC3339))
		if err := policy.Save(bundle); err != nil {
			log.Warn("guard: policy cache save failed", "err", err)
		}
		p.SetGuardPolicies(policy.LocalExact(bundle.Policies))
		checkAggregateWarns(ctx, notifier, bundle.Policies, warned, log)
	}

	pull() // refresh immediately at startup
	ticker := time.NewTicker(guardPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pull()
		}
	}
}

// checkAggregateWarns fires a warn for each team-aggregate cap whose
// server-reported spend has reached its cap, once per period. These caps are
// warn-only by design: one machine cannot know the whole team's spend, so it
// trusts the server figure and states its staleness rather than killing. The
// period is UTC to match the server's shared team window (see evaluateGuard).
func checkAggregateWarns(ctx context.Context, notifier *ntfy.Client, policies []policy.Policy, warned map[string]bool, log *slog.Logger) {
	for _, pol := range policy.Aggregate(policies) {
		if pol.ServerSpentCents < pol.CapCents {
			continue
		}
		_, end := budget.PeriodBounds(policy.MapPeriod(pol.Period), now(), time.UTC)
		key := pol.ID + "|" + end.Format(time.RFC3339)
		if warned[key] {
			continue
		}
		warned[key] = true
		title := "budgetclaw: team budget reached"
		reason := fmt.Sprintf("team %s cap $%.2f reached ($%.2f team spend as of %s)",
			pol.Period, pol.CapUSD(), float64(pol.ServerSpentCents)/100, pol.AsOf)
		if err := notifier.SendWarn(ctx, title, reason); err != nil {
			log.Warn("ntfy: guard aggregate-warn failed", "policy", pol.ID, "err", err)
		}
	}
}

// now is a package-level clock override point. Kept as a var so
// tests can stub it without exported machinery.
var now = realNow

func realNow() time.Time { return time.Now() }
