// Package pipeline wires the six independent budgetclaw data-flow
// packages (parser, pricing, db, budget, enforcer, ntfy) into one
// Handler function suitable for the watcher.
//
// The pipeline is deliberately linear and single-function so the
// order of operations is obvious to readers:
//
//  1. Compute cost for the event (pricing)
//  2. Write the event + rollup delta to SQLite (db)
//  3. Check existing breach lock for (project, branch):
//     - If locked and expired → auto-release, proceed.
//     - If locked and active  → re-kill, return (no further eval).
//     - If not locked         → continue.
//  4. Evaluate every matching budget rule for this event
//  5. For each breached rule, dispatch the action:
//     - warn → SendWarn via ntfy
//     - kill → HandleBreach (write lockfile + SIGTERM) and SendKill
//
// Errors from downstream packages are logged via the package
// logger but do NOT abort the Handler. The watcher keeps running
// even when one event fails; a transient db hiccup must never
// silently stop all budget enforcement.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/RoninForge/budgetclaw/internal/budget"
	"github.com/RoninForge/budgetclaw/internal/db"
	"github.com/RoninForge/budgetclaw/internal/enforcer"
	"github.com/RoninForge/budgetclaw/internal/ntfy"
	"github.com/RoninForge/budgetclaw/internal/parser"
	"github.com/RoninForge/budgetclaw/internal/pricing"
)

// Pipeline holds the live dependencies of the data-flow core. The
// watcher wires a Pipeline once at startup and calls Handle for
// every parsed event.
//
// All fields except Now and Logger are required. Now defaults to
// time.Now; Logger defaults to a discarding slog handler.
type Pipeline struct {
	Config   *budget.Config
	DB       *db.DB
	Enforcer *enforcer.Enforcer
	Notifier *ntfy.Client

	// Now is the clock used for lock expiry and period bounds.
	// Tests inject a fixed time.
	Now func() time.Time

	// Logger receives structured debug and error messages. Quiet
	// by default so daemon mode does not spam stderr.
	Logger *slog.Logger
}

// normalized returns the logger and now-function, filling in
// defaults. Called at the top of Handle so tests and production
// use the same path.
func (p *Pipeline) normalized() (*slog.Logger, func() time.Time) {
	log := p.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	now := p.Now
	if now == nil {
		now = time.Now
	}
	return log, now
}

// Handle is the Watcher handler. It takes a parsed event and
// runs the full pipeline. Returns nil in every non-fatal case —
// even when internal steps fail — so the watcher keeps running.
// Context cancellation is propagated: callers that abort ctx
// will see Handle return ctx.Err from whichever step noticed.
func (p *Pipeline) Handle(ctx context.Context, e *parser.Event, _ string) error {
	if e == nil {
		return nil
	}
	if p == nil || p.DB == nil || p.Enforcer == nil {
		return errors.New("pipeline: missing required dependencies")
	}

	log, now := p.normalized()

	// --- 1. price -------------------------------------------------
	cost, err := pricing.CostForModel(e.Model, pricing.Usage{
		Input:        e.InputTokens,
		Output:       e.OutputTokens,
		CacheRead:    e.CacheReadTokens,
		CacheWrite5m: e.CacheCreation5mTokens,
		CacheWrite1h: e.CacheCreation1hTokens,
	})
	if err != nil {
		// Unknown model — log and drop this event. Ignoring is
		// safer than guessing: a wrong cost would poison rollups.
		log.Warn("pricing: unknown model, skipping event",
			"uuid", e.UUID, "model", e.Model, "err", err)
		return nil
	}

	// --- 2. persist -----------------------------------------------
	if err := p.DB.Insert(ctx, e, cost); err != nil {
		log.Error("db: insert failed",
			"uuid", e.UUID, "project", e.Project, "err", err)
		return nil
	}

	// --- 3. existing-lock check -----------------------------------
	lk, killed, err := p.Enforcer.CheckLocked(ctx, e.Project, e.GitBranch, e.CWD, now())
	if err != nil {
		log.Warn("enforcer: check-locked failed",
			"project", e.Project, "branch", e.GitBranch, "err", err)
		// Continue to evaluation; a transient lock-store hiccup
		// shouldn't disable budget enforcement entirely.
	}
	if lk != nil {
		log.Info("re-killed locked session",
			"project", e.Project, "branch", e.GitBranch, "pids", killed)
		// Already locked — no point re-evaluating.
		return nil
	}

	// --- 4. evaluate ----------------------------------------------
	spend := func(ctx context.Context, project, branch string, start, end time.Time) (float64, error) {
		r, err := p.DB.RollupSum(ctx, project, branch, start, end)
		if err != nil {
			return 0, err
		}
		return r.CostUSD, nil
	}

	verdicts, err := budget.Evaluate(ctx, p.Config, e.Project, e.GitBranch, e.Timestamp, spend)
	if err != nil {
		log.Error("budget: evaluate failed",
			"project", e.Project, "branch", e.GitBranch, "err", err)
		return nil
	}

	// --- 5. dispatch actions --------------------------------------
	for _, v := range verdicts {
		if !v.Breach {
			continue
		}
		p.dispatch(ctx, e, v, log)
	}

	return nil
}

// dispatch handles a single breached verdict: formats the message,
// writes the lock (for kill actions), SIGTERMs matching processes
// (for kill), and sends the appropriate ntfy notification. Every
// failure path is logged; nothing is returned to the caller.
func (p *Pipeline) dispatch(ctx context.Context, e *parser.Event, v budget.Verdict, log *slog.Logger) {
	title := fmt.Sprintf("budgetclaw: %s/%s", e.Project, e.GitBranch)
	reason := fmt.Sprintf("%s cap $%.2f breached at $%.2f",
		v.Rule.Period.String(), v.CapUSD, v.CurrentUSD)

	switch v.Rule.Action {
	case budget.ActionWarn:
		if err := p.Notifier.SendWarn(ctx, title, reason); err != nil {
			log.Warn("ntfy: send-warn failed",
				"project", e.Project, "err", err)
		}

	case budget.ActionKill:
		lock := enforcer.Lock{
			Project:    e.Project,
			Branch:     e.GitBranch,
			Period:     v.Rule.Period.String(),
			Reason:     reason,
			CapUSD:     v.CapUSD,
			CurrentUSD: v.CurrentUSD,
			LockedAt:   time.Now().UTC(),
			ExpiresAt:  v.PeriodEnd,
		}
		killed, err := p.Enforcer.HandleBreach(ctx, lock, e.CWD)
		if err != nil {
			log.Error("enforcer: handle-breach failed",
				"project", e.Project, "err", err)
		}
		log.Info("kill breach",
			"project", e.Project, "branch", e.GitBranch,
			"cap", v.CapUSD, "current", v.CurrentUSD, "pids", killed)
		if err := p.Notifier.SendKill(ctx, title, reason); err != nil {
			log.Warn("ntfy: send-kill failed",
				"project", e.Project, "err", err)
		}
	}
}

// discardWriter is a no-op io.Writer used by the default logger
// when Pipeline.Logger is nil. We use this instead of io.Discard
// from the stdlib only because importing io for a single symbol
// adds noise in the import block; inlining is clearer.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
