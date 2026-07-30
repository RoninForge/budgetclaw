package cli

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/RoninForge/budgetclaw/internal/db"
	"github.com/RoninForge/budgetclaw/internal/pricing/refresh"
	"github.com/RoninForge/budgetclaw/internal/reconcile"
)

// Background price refreshing for `budgetclaw watch`.
//
// Never on the hot path. The watcher prices events from an in-memory
// table via a single atomic load; this goroutine only ever swaps that
// pointer, so an event is never delayed by a network call.

const (
	// checkInterval is the steady-state cadence. A conditional GET that
	// finds nothing new costs a 304 with an empty body, so this is close
	// to free; the interval is set by how quickly a user should stop
	// seeing a new model as unpriced, not by bandwidth.
	checkInterval = 24 * time.Hour

	// startupDelayMax spreads the first check so restarting many machines
	// at once (a laptop waking, a fleet rebooting) does not arrive as a
	// spike.
	startupDelayMin = 30 * time.Second
	startupDelayMax = 10 * time.Minute

	// intervalJitterMax spreads the daily check for the same reason.
	intervalJitterMax = 2 * time.Hour

	// reactiveCooldown bounds the reactive path. Meeting an unknown model
	// is a good reason to check immediately, but a long session on an
	// unknown model must not turn into a request per event.
	reactiveCooldown  = 1 * time.Hour
	reactiveMaxPerDay = 4
)

// pricingRefresher runs the opt-in background refresh.
type pricingRefresher struct {
	url    string
	store  *db.DB
	logger *slog.Logger

	// kick carries reactive triggers from the ingest path. Buffered depth
	// one: a second trigger while one is pending is redundant.
	kick chan struct{}
}

// newPricingRefresher wires a refresher. Returns nil when the user has
// not opted in, so callers can treat "disabled" as "no goroutine".
func newPricingRefresher(enabled bool, url string, store *db.DB, logger *slog.Logger) *pricingRefresher {
	if !enabled {
		return nil
	}
	return &pricingRefresher{
		url:    url,
		store:  store,
		logger: logger,
		kick:   make(chan struct{}, 1),
	}
}

// Trigger asks for an out-of-band check. Safe to call from the ingest
// path: it never blocks and never allocates a request itself.
func (p *pricingRefresher) Trigger() {
	if p == nil {
		return
	}
	select {
	case p.kick <- struct{}{}:
	default: // a check is already pending
	}
}

// LoadCache installs the last verified bundle from disk, so an enabled
// client keeps its fresher prices across a restart without waiting for
// the network. The signature is re-verified on load, so a corrupted or
// tampered cache is caught rather than trusted.
func (p *pricingRefresher) LoadCache() {
	if p == nil {
		return
	}
	res, err := refresh.LoadCachedTable(time.Now())
	switch {
	case err != nil:
		// Includes a failed signature on the cached file. Nothing to do
		// but keep the built-in table and fetch again shortly.
		p.logger.Warn("pricing: cached table not usable, keeping the built-in one", "err", err)
	case res.Updated:
		p.logger.Info("pricing: loaded cached table",
			"tag", res.Tag, "data", res.DataDate, "models", res.Models)
	}
}

// Run blocks until ctx is done, refreshing on a jittered schedule and on
// demand. Every failure is logged and otherwise ignored: the table in
// force stays, which is stale but never wrong.
func (p *pricingRefresher) Run(ctx context.Context) {
	if p == nil {
		return
	}

	timer := time.NewTimer(jitter(startupDelayMin, startupDelayMax))
	defer timer.Stop()

	var reactiveAt time.Time
	reactiveToday := 0
	dayStart := time.Now()

	for {
		select {
		case <-ctx.Done():
			return

		case <-timer.C:
			p.once(ctx, "scheduled")
			timer.Reset(checkInterval + jitter(0, intervalJitterMax))

		case <-p.kick:
			now := time.Now()
			// Reset the daily budget on a rolling 24h window.
			if now.Sub(dayStart) > 24*time.Hour {
				dayStart, reactiveToday = now, 0
			}
			if reactiveToday >= reactiveMaxPerDay || now.Sub(reactiveAt) < reactiveCooldown {
				continue
			}
			reactiveAt, reactiveToday = now, reactiveToday+1
			p.once(ctx, "unknown model seen")
		}
	}
}

// once performs a single refresh attempt and, if the table changed,
// reprices whatever was stored without a price.
func (p *pricingRefresher) once(ctx context.Context, reason string) {
	res, err := refresh.Refresh(ctx, p.url, time.Now())
	switch {
	case errors.Is(err, refresh.ErrNotModified):
		p.logger.Debug("pricing: table already current", "reason", reason)
		return
	case errors.Is(err, refresh.ErrOffline):
		// Routine. Not worth a WARN on a laptop that is simply offline.
		p.logger.Debug("pricing: refresh unavailable", "reason", reason, "err", err)
		return
	case errors.Is(err, refresh.ErrBadSignature):
		// This one is loud: it means someone served us something we did
		// not publish, or the file is corrupt in transit.
		p.logger.Warn("pricing: downloaded table failed signature verification, discarded",
			"reason", reason, "err", err)
		return
	case errors.Is(err, refresh.ErrRejected):
		// The gate code is logged separately from the free-text error so it
		// can be grepped and counted across machines: "rate_out_of_range"
		// appearing in the field means the published data has a unit error,
		// which is ours to fix, not the user's.
		p.logger.Warn("pricing: downloaded table failed a plausibility check, discarded",
			"reason", reason, "gate", string(refresh.ReasonOf(err)), "err", err)
		return
	case err != nil:
		p.logger.Warn("pricing: refresh failed", "reason", reason, "err", err)
		return
	}

	p.logger.Info("pricing: installed a newer table",
		"tag", res.Tag, "data", res.DataDate, "models", res.Models,
		"key", res.KeyName, "reason", reason)

	// The effective table just changed, so previously unpriceable events
	// may now price. Forced, because the caller knows the table moved.
	if rec, rerr := reconcile.Run(ctx, p.store, true); rerr != nil {
		p.logger.Warn("pricing: reprice after refresh failed", "err", rerr)
	} else if rec.Any() {
		p.logger.Info("pricing: " + rec.Summary())
	}
}

// jitter returns a duration uniformly in [min, max]. Spreading checks
// keeps many installs from arriving at the same instant.
func jitter(minDur, maxDur time.Duration) time.Duration {
	if maxDur <= minDur {
		return minDur
	}
	// #nosec G404 -- this picks a delay to spread request timing across
	// installs, nothing more. It guards no secret and gates no decision:
	// predicting it reveals only when a client will ask for a public file.
	// A CSPRNG here would add error handling for no benefit.
	return minDur + time.Duration(rand.Int64N(int64(maxDur-minDur)+1))
}
