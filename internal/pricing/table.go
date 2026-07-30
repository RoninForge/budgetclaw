package pricing

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// The effective pricing table.
//
// By default it is the one compiled into the binary from the vendored,
// pinned dataset. When the opt-in refresh is enabled and a newer dataset
// has been fetched AND its signature verified, that data is overlaid on
// top and the whole table is swapped in atomically, so a lookup never
// sees a half-applied update.
//
// Overlay, not replacement: the compiled-in table is a floor. A fetched
// model replaces that model's series wholesale, models the fetch does
// not mention keep their built-in series, and nothing is ever deleted.
// That means a bundle which omits a model cannot make already-recorded
// spend unpriceable, and it denies a hostile or simply buggy bundle the
// ability to hide spend by dropping a model. The cost of this choice is
// that a genuinely retired model lingers, which is harmless: its
// intervals are closed, so it prices history correctly and nothing new.

// Source says where the active table came from.
type Source string

const (
	// SourceBuiltIn is the table compiled into this binary.
	SourceBuiltIn Source = "built-in"
	// SourceFetched is a verified dataset overlaid on the built-in one.
	SourceFetched Source = "fetched"
)

// table is an immutable snapshot of the effective pricing data. Never
// mutated after being stored; a change means building a new one and
// swapping the pointer.
type table struct {
	series   map[string]modelHist
	aliases  map[string]string
	tag      string
	commit   string
	dataDate string
	source   Source
}

// active holds the table in force. Reads are a single atomic load, so
// pricing stays lock-free on the hot path and a concurrent swap cannot
// tear.
var active atomic.Pointer[table]

func init() { active.Store(builtIn()) }

// builtIn returns the compiled-in table.
func builtIn() *table {
	return &table{
		series:   modelSeries,
		aliases:  modelAliases,
		tag:      generatedTag,
		commit:   generatedIndexCommit,
		dataDate: generatedDataModified,
		source:   SourceBuiltIn,
	}
}

// current is the effective table for every lookup in this package.
func current() *table { return active.Load() }

// ErrInvalidTable reports that externally supplied pricing data failed
// the structural checks this package relies on. The data is discarded
// and the previous table stays in force.
var ErrInvalidTable = errors.New("invalid pricing table")

// ExternalInterval is one half-open [from, to) price window supplied
// from outside this package. To == nil means the interval is open.
type ExternalInterval struct {
	From  time.Time
	To    *time.Time
	Price float64
}

// ExternalModel is one model's full price history, supplied from
// outside this package.
type ExternalModel struct {
	ID      string
	Aliases []string
	Input   []ExternalInterval
	Output  []ExternalInterval
}

// ExternalTable is verified pricing data ready to overlay.
type ExternalTable struct {
	// Tag identifies the upstream release, for provenance reporting.
	Tag string
	// Commit is the upstream commit, if known.
	Commit string
	// DataDate is the dataset's own data date (not a fetch time).
	DataDate string
	// Models is the per-model history to overlay.
	Models []ExternalModel
}

// Install validates externally supplied pricing data, overlays it on the
// compiled-in table, and activates the result atomically.
//
// Validation here is structural only: the invariants this package's
// lookups depend on. Judging whether the NUMBERS are plausible is the
// caller's job, because only the caller knows what the previous table
// said. On any error nothing changes and the active table is untouched.
func Install(t ExternalTable) error {
	if len(t.Models) == 0 {
		return fmt.Errorf("%w: no models", ErrInvalidTable)
	}

	overlay := make(map[string]modelHist, len(t.Models))
	for _, m := range t.Models {
		if m.ID == "" {
			return fmt.Errorf("%w: a model has no id", ErrInvalidTable)
		}
		in, err := convertSeries(m.ID, "input", m.Input)
		if err != nil {
			return err
		}
		out, err := convertSeries(m.ID, "output", m.Output)
		if err != nil {
			return err
		}
		overlay[m.ID] = modelHist{input: in, output: out}
	}

	// Start from the compiled-in table so it acts as a floor, then let
	// the fetched data win per model.
	base := builtIn()
	series := make(map[string]modelHist, len(base.series)+len(overlay))
	for k, v := range base.series {
		series[k] = v
	}
	for k, v := range overlay {
		series[k] = v
	}

	aliases := make(map[string]string, len(base.aliases))
	for k, v := range base.aliases {
		aliases[k] = v
	}
	for _, m := range t.Models {
		for _, a := range m.Aliases {
			if a == "" || a == m.ID {
				continue
			}
			// An alias is only useful if it resolves to a model we hold.
			if _, ok := series[m.ID]; ok {
				aliases[a] = m.ID
			}
		}
	}

	active.Store(&table{
		series:   series,
		aliases:  aliases,
		tag:      t.Tag,
		commit:   t.Commit,
		dataDate: t.DataDate,
		source:   SourceFetched,
	})
	return nil
}

// convertSeries validates one variation's intervals and converts them to
// the internal representation.
//
// The checks are exactly what priceAt assumes: at least one interval,
// each interval non-empty (from strictly before to), ordered by from,
// non-overlapping, and a positive price. A series that violated these
// would not crash, it would silently return the wrong rate, which is the
// failure this package exists to avoid.
func convertSeries(model, variation string, ivs []ExternalInterval) ([]priceInterval, error) {
	if len(ivs) == 0 {
		return nil, fmt.Errorf("%w: %s %s has no intervals", ErrInvalidTable, model, variation)
	}

	out := make([]priceInterval, 0, len(ivs))
	var prevTo *time.Time
	openSeen := false

	for i, iv := range ivs {
		if iv.From.IsZero() {
			return nil, fmt.Errorf("%w: %s %s interval %d has no start", ErrInvalidTable, model, variation, i)
		}
		if iv.Price <= 0 {
			return nil, fmt.Errorf("%w: %s %s interval %d price is %v", ErrInvalidTable, model, variation, i, iv.Price)
		}
		from := iv.From.UTC()
		var to *time.Time
		if iv.To != nil {
			t := iv.To.UTC()
			if !from.Before(t) {
				return nil, fmt.Errorf("%w: %s %s interval %d ends %v at or before it starts %v",
					ErrInvalidTable, model, variation, i, t, from)
			}
			to = &t
		}

		// An open interval must be the last one; anything after it would
		// be unreachable, since priceAt returns the first match.
		if openSeen {
			return nil, fmt.Errorf("%w: %s %s has an interval after an open one", ErrInvalidTable, model, variation)
		}
		if to == nil {
			openSeen = true
		}

		if i > 0 {
			if prevTo == nil {
				return nil, fmt.Errorf("%w: %s %s interval %d follows an open interval",
					ErrInvalidTable, model, variation, i)
			}
			if from.Before(*prevTo) {
				return nil, fmt.Errorf("%w: %s %s interval %d starts %v before the previous ends %v",
					ErrInvalidTable, model, variation, i, from, *prevTo)
			}
		}

		out = append(out, priceInterval{from: from, to: to, priceUSD: iv.Price})
		prevTo = to
	}
	return out, nil
}

// ActiveTable reports what pricing data is in force, for diagnostics and
// for the reconcile pass, which re-prices stored events whenever the
// table identity changes.
func ActiveTable() (source Source, tag, dataDate string) {
	t := current()
	return t.source, t.tag, t.dataDate
}

// RestoreBuiltIn reverts to the compiled-in table. Used by tests, and
// available as a safety valve if a fetched table ever needs backing out
// without reinstalling the binary.
func RestoreBuiltIn() { active.Store(builtIn()) }

// DatasetDate is the data date of the active table, as YYYY-MM-DD. It
// answers "how old are these prices" without conflating that with when
// the binary was built or when a fetch last ran.
func DatasetDate() string { return current().dataDate }
