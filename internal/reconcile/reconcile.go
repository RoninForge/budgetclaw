// Package reconcile prices events that were stored without a cost.
//
// budgetclaw stores an event whose model the pricing table does not know
// rather than discarding it (see db.InsertUnpriced). Those rows keep
// their full token counts, so once the table learns the model the spend
// can be recovered from the rows themselves. This package is what does
// that, automatically, with no user action.
//
// It depends on both db and pricing, which is why it is its own package:
// db stays free of any pricing dependency, and pricing stays free of any
// storage dependency. The caller wires the two together.
//
// The pass is safe to run from several places at once. Every update is
// guarded so a second run, or a racing process, prices nothing twice.
package reconcile

import (
	"context"
	"fmt"

	"github.com/RoninForge/budgetclaw/internal/db"
	"github.com/RoninForge/budgetclaw/internal/pricing"
)

// watermarkKey records the pricing-table identity of the last completed
// pass. While it is unchanged, an unpriced row cannot become priceable:
// it was already rejected by this exact table.
const watermarkKey = "reconcile.last_pricing_id"

// batchSize is how many events are repriced per transaction. Large
// enough that a big backlog does not spend all its time in commits,
// small enough that a crash loses little and other writers are not
// blocked for long.
const batchSize = 500

// Result reports what a pass did. Repriced is the number of events that
// gained a cost; Recovered is the dollar total those events added.
// Remaining counts rows still unpriceable, which is a legitimate steady
// state (a retired model with no interval covering an old event).
type Result struct {
	Repriced  int
	Recovered float64
	Models    []string
	Remaining int
}

// Any reports whether the pass changed anything worth telling the user.
func (r Result) Any() bool { return r.Repriced > 0 }

// Run prices whatever it can of the unpriced backlog.
//
// It is cheap to call unconditionally. The healthy case costs one
// indexed existence probe and returns immediately, so callers can run it
// at startup without thinking about it.
//
// force skips the watermark check. Use it when the effective pricing
// table may have changed in a way the tag does not capture, or when the
// user explicitly asked for a repair pass.
func Run(ctx context.Context, store *db.DB, force bool) (Result, error) {
	var res Result

	// Fast path: nothing is unpriced, so there is nothing to do. The
	// partial index is empty here, which makes this close to free.
	has, err := store.HasUnpriced(ctx)
	if err != nil {
		return res, err
	}
	if !has {
		return res, nil
	}

	pricingID := identity()

	if !force {
		last, err := store.MetaGet(ctx, watermarkKey)
		if err != nil {
			return res, err
		}
		if last == pricingID {
			// Same table as the last completed pass. Anything still
			// unpriced was already rejected by it, so a rescan would
			// only burn IO to reach the same answer.
			return res, nil
		}
	}

	models, err := store.UnpricedModels(ctx)
	if err != nil {
		return res, err
	}

	known := knownSet()
	for _, model := range models {
		// Cheap map lookup before touching any rows. A model the table
		// still does not know cannot gain a price in this pass.
		if !known[model] {
			continue
		}
		n, usd, err := repriceModel(ctx, store, model)
		if err != nil {
			return res, err
		}
		if n > 0 {
			res.Repriced += n
			res.Recovered += usd
			res.Models = append(res.Models, model)
		}
	}

	// Record the watermark only after a complete pass, so a crash
	// halfway through resumes on the next run instead of being skipped.
	if err := store.MetaSet(ctx, watermarkKey, pricingID); err != nil {
		return res, err
	}

	remaining, err := store.UnpricedByModel(ctx)
	if err != nil {
		return res, err
	}
	for _, m := range remaining {
		res.Remaining += m.EventCount
	}
	return res, nil
}

// repriceModel walks one model's backlog in keyset-paginated batches and
// prices every row it can, returning the count and dollars recovered.
func repriceModel(ctx context.Context, store *db.DB, model string) (int, float64, error) {
	var (
		count   int
		total   float64
		afterID string
	)
	for {
		batch, err := store.UnpricedBatch(ctx, model, afterID, batchSize)
		if err != nil {
			return count, total, err
		}
		if len(batch) == 0 {
			return count, total, nil
		}

		priced := make([]db.RepricedEvent, 0, len(batch))
		for _, e := range batch {
			// Point-in-time: price at the rate effective on the event's
			// own timestamp, exactly as the ingest path would have done
			// had the table been current that day. A reconciled event is
			// therefore indistinguishable from one priced on arrival.
			cost, err := pricing.CostForModelAt(e.Model, e.Timestamp, pricing.Usage{
				Input:        e.Input,
				Output:       e.Output,
				CacheRead:    e.CacheRead,
				CacheWrite5m: e.CacheWrite5m,
				CacheWrite1h: e.CacheWrite1h,
			})
			if err != nil {
				// Still unpriceable at this timestamp. Leave it stored
				// and unpriced: honest, and it costs nothing to keep.
				continue
			}
			priced = append(priced, db.RepricedEvent{
				UUID:      e.UUID,
				Project:   e.Project,
				GitBranch: e.GitBranch,
				Timestamp: e.Timestamp,
				CostUSD:   cost,
			})
			total += cost
		}

		applied, err := store.ApplyReprice(ctx, priced)
		if err != nil {
			return count, total, err
		}
		count += applied

		// Advance past this batch regardless of how many priced, so rows
		// that remain unpriceable do not make this loop spin forever.
		afterID = batch[len(batch)-1].UUID
	}
}

// identity is the fingerprint of the effective pricing table. The
// vendored dataset tag changes on every price release, which is exactly
// when previously-unpriceable events might become priceable.
func identity() string {
	tag, commit := pricing.Provenance()
	if tag == "" && commit == "" {
		return "unknown"
	}
	return tag + "@" + commit
}

// knownSet is the set of model ids the current table can price, as a map
// for O(1) lookup during the scan.
func knownSet() map[string]bool {
	models := pricing.KnownModels()
	set := make(map[string]bool, len(models))
	for _, m := range models {
		set[m] = true
	}
	return set
}

// Summary renders a one-line, user-facing description of a pass that
// changed something. Returns "" when nothing was repriced, so callers
// can print it unconditionally.
func (r Result) Summary() string {
	if !r.Any() {
		return ""
	}
	models := ""
	switch len(r.Models) {
	case 0:
	case 1:
		models = " (" + r.Models[0] + ")"
	default:
		models = fmt.Sprintf(" (%s and %d more)", r.Models[0], len(r.Models)-1)
	}
	return fmt.Sprintf("repriced %d previously unpriced event(s): $%.2f recovered%s",
		r.Repriced, r.Recovered, models)
}
