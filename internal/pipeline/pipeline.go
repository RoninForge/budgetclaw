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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/RoninForge/budgetclaw/internal/budget"
	"github.com/RoninForge/budgetclaw/internal/db"
	"github.com/RoninForge/budgetclaw/internal/enforcer"
	"github.com/RoninForge/budgetclaw/internal/goei"
	"github.com/RoninForge/budgetclaw/internal/ntfy"
	"github.com/RoninForge/budgetclaw/internal/parser"
	"github.com/RoninForge/budgetclaw/internal/policy"
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

	// Machine is the identity stamped on Guard Mode audit events, so the
	// team owner can see which machine a runaway was stopped on. Empty is
	// fine (the server treats it as unknown).
	Machine string

	// guardMu guards guardPolicies, the locally-enforceable remote caps.
	// The watch loop's policy ticker swaps the set atomically via
	// SetGuardPolicies while Handle reads a snapshot per event.
	guardMu       sync.Mutex
	guardPolicies []policy.Policy

	// firedGuardMu guards firedGuard, which dedups a remote breach's
	// notification + audit event to once per (policy, period, action) in
	// this run. The kill mechanics still run every time so each offending
	// session is stopped; only the notify/audit is deduped.
	firedGuardMu sync.Mutex
	firedGuard   map[string]bool

	// unknownModelsMu guards unknownModels. The watcher dispatches
	// Handle from a single goroutine today, but the lock keeps the
	// dedupe correct if that ever changes.
	unknownModelsMu sync.Mutex
	// unknownModels remembers which model IDs already produced a
	// loud WARN in this watcher run so a single Opus 4.7 session
	// burning thousands of unpriceable events does not flood
	// stderr. Run `budgetclaw pricing diagnose` for ground-truth
	// detection across the historical log corpus.
	unknownModels map[string]int
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
	// Price the event at the rate effective on its own timestamp, not
	// at "now". An event recorded while a model was on an older tier
	// is priced at that older tier, so historical cost is frozen as
	// fact (see decision 15e). e.Timestamp is always set by the parser.
	cost, err := pricing.CostForModelAt(e.Model, e.Timestamp, pricing.Usage{
		Input:        e.InputTokens,
		Output:       e.OutputTokens,
		CacheRead:    e.CacheReadTokens,
		CacheWrite5m: e.CacheCreation5mTokens,
		CacheWrite1h: e.CacheCreation1hTokens,
	})
	if err != nil {
		if errors.Is(err, pricing.ErrUnknownModel) || errors.Is(err, pricing.ErrNoRateAtTime) {
			// ErrUnknownModel: model not in the table at all.
			// ErrNoRateAtTime: known model, but no price interval covers
			// the event's timestamp (a retired model, or an event older
			// than the model's earliest recorded price). Both are skipped
			// non-fatally with the same dedupe-WARN so a long session of
			// unpriceable events does not flood stderr.
			p.logUnknownModel(log, e.Model, e.UUID)
			return nil
		}
		// Some other pricing failure (shouldn't happen with the
		// current implementation, but keep the path safe).
		log.Warn("pricing: unexpected error, skipping event",
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

	// --- 6. Guard Mode: enforce remote team policies --------------
	// Reached only when the scope was not already locked (step 3 returns
	// early otherwise), so this never double-fires on a locked session.
	p.evaluateGuard(ctx, e, now(), log)

	return nil
}

// SetGuardPolicies atomically replaces the locally-enforceable remote policy
// set. Called by the watch loop's policy ticker after each refresh; only
// local_exact policies belong here (server-aggregate caps warn elsewhere).
func (p *Pipeline) SetGuardPolicies(policies []policy.Policy) {
	p.guardMu.Lock()
	p.guardPolicies = policies
	p.guardMu.Unlock()
}

func (p *Pipeline) guardSnapshot() []policy.Policy {
	p.guardMu.Lock()
	defer p.guardMu.Unlock()
	return p.guardPolicies
}

// markFiredGuard records that (policy, period, action) has notified this run
// and reports whether this call is the first to do so.
func (p *Pipeline) markFiredGuard(key string) bool {
	p.firedGuardMu.Lock()
	defer p.firedGuardMu.Unlock()
	if p.firedGuard == nil {
		p.firedGuard = make(map[string]bool)
	}
	if p.firedGuard[key] {
		return false
	}
	p.firedGuard[key] = true
	return true
}

// evaluateGuard checks this event against every locally-enforceable remote
// policy and enforces breaches. A per-developer/team cap sums the whole
// machine's spend; a per-project cap sums that project and only acts on a
// session in it, so an unrelated project is never killed for another's cap.
func (p *Pipeline) evaluateGuard(ctx context.Context, e *parser.Event, now time.Time, log *slog.Logger) {
	policies := p.guardSnapshot()
	if len(policies) == 0 {
		return
	}

	for _, pol := range policies {
		// Remote caps use UTC, not the user's local [general].timezone: a shared
		// team cap must resolve to one window for everyone, and the server defines
		// that window (and its "spent so far") in UTC. Local TOML rules keep the
		// user's own timezone; only these server-owned policies are pinned to UTC.
		start, end := budget.PeriodBounds(policy.MapPeriod(pol.Period), now, time.UTC)

		var spent float64
		var err error
		if pol.ScopeType == "project" {
			// Only the capped project's own sessions are eligible; a runaway
			// in a different project is not this cap's business.
			if pol.ScopeValue == "" || e.Project != pol.ScopeValue {
				continue
			}
			spent, err = p.DB.ProjectSum(ctx, pol.ScopeValue, start, end)
		} else {
			// team / dev -> the whole machine is this developer's own spend.
			spent, err = p.DB.TotalSum(ctx, start, end)
		}
		if err != nil {
			log.Warn("guard: spend query failed", "policy", pol.ID, "err", err)
			continue
		}
		if spent <= pol.CapUSD() {
			continue
		}
		p.fireGuard(ctx, e, pol, spent, end, now, log)
	}
}

// fireGuard enforces one breached remote policy: kill (SIGTERM + lock) or
// warn, plus a once-per-period notification and queued audit event.
func (p *Pipeline) fireGuard(ctx context.Context, e *parser.Event, pol policy.Policy, spent float64, periodEnd, now time.Time, log *slog.Logger) {
	first := p.markFiredGuard(pol.ID + "|" + periodEnd.Format(time.RFC3339) + "|" + pol.Action)
	title := "budgetclaw: guard stopped a runaway"
	reason := fmt.Sprintf("%s cap $%.2f breached at $%.2f (%s)",
		pol.Period, pol.CapUSD(), spent, setByLabel(pol.SetBy))

	if pol.Action == "kill" {
		lock := enforcer.Lock{
			Project:    e.Project,
			Branch:     e.GitBranch,
			Period:     policy.MapPeriod(pol.Period).String(),
			Reason:     reason,
			CapUSD:     pol.CapUSD(),
			CurrentUSD: spent,
			LockedAt:   time.Now().UTC(),
			ExpiresAt:  periodEnd,
			PolicyID:   pol.ID,
		}
		killed, err := p.Enforcer.HandleBreach(ctx, lock, e.CWD)
		if err != nil {
			log.Error("guard: handle-breach failed", "policy", pol.ID, "project", e.Project, "err", err)
		}
		log.Info("guard kill breach",
			"policy", pol.ID, "project", e.Project, "branch", e.GitBranch,
			"cap", pol.CapUSD(), "current", spent, "pids", killed)
		if first {
			if err := p.Notifier.SendKill(ctx, title, reason); err != nil {
				log.Warn("ntfy: guard send-kill failed", "policy", pol.ID, "err", err)
			}
			p.queueGuardEvent(ctx, e, pol, spent, periodEnd, now, log)
		}
		return
	}

	// warn
	if first {
		if err := p.Notifier.SendWarn(ctx, title, reason); err != nil {
			log.Warn("ntfy: guard send-warn failed", "policy", pol.ID, "err", err)
		}
		p.queueGuardEvent(ctx, e, pol, spent, periodEnd, now, log)
	}
}

// queueGuardEvent persists a content-free audit event for the next sync.
func (p *Pipeline) queueGuardEvent(ctx context.Context, e *parser.Event, pol policy.Policy, spent float64, periodEnd, now time.Time, log *slog.Logger) {
	ev := goei.GuardEvent{
		PolicyID:    pol.ID,
		Action:      pol.Action,
		ScopeType:   pol.ScopeType,
		ScopeValue:  e.Project, // where the runaway actually was
		Machine:     p.Machine,
		AmountCents: int(spent*100 + 0.5),
		CapCents:    pol.CapCents,
		At:          now.UTC().Format(time.RFC3339),
		DedupKey:    pol.ID + ":" + periodEnd.Format(time.RFC3339) + ":" + pol.Action,
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		log.Warn("guard: marshal audit event failed", "policy", pol.ID, "err", err)
		return
	}
	if _, err := p.DB.QueueGuardEvent(ctx, ev.DedupKey, string(raw)); err != nil {
		log.Warn("guard: queue audit event failed", "policy", pol.ID, "err", err)
	}
}

// setByLabel renders who set a policy for a legible breach message.
func setByLabel(setBy string) string {
	if setBy == "" {
		return "set by your team"
	}
	return "set by " + setBy
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

// logUnknownModel emits a loud WARN the first time a model is seen
// in this watcher run, and a quiet DEBUG for every event after.
// Returns the new total count for the model; tests use it to verify
// dedupe behavior.
func (p *Pipeline) logUnknownModel(log *slog.Logger, model, uuid string) int {
	p.unknownModelsMu.Lock()
	if p.unknownModels == nil {
		p.unknownModels = make(map[string]int)
	}
	count := p.unknownModels[model] + 1
	p.unknownModels[model] = count
	p.unknownModelsMu.Unlock()

	if count == 1 {
		log.Warn("pricing: unknown model — events skipped until next release; run `budgetclaw pricing diagnose` for the full set",
			"model", model, "uuid", uuid)
	} else {
		log.Debug("pricing: unknown model (suppressed repeat)",
			"model", model, "count", count)
	}
	return count
}

// UnknownModels returns a snapshot of model IDs the pipeline has
// skipped this run, paired with their event counts. Useful for
// status output and tests; safe for concurrent callers.
func (p *Pipeline) UnknownModels() map[string]int {
	p.unknownModelsMu.Lock()
	defer p.unknownModelsMu.Unlock()
	out := make(map[string]int, len(p.unknownModels))
	for k, v := range p.unknownModels {
		out[k] = v
	}
	return out
}

// discardWriter is a no-op io.Writer used by the default logger
// when Pipeline.Logger is nil. We use this instead of io.Discard
// from the stdlib only because importing io for a single symbol
// adds noise in the import block; inlining is clearer.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
