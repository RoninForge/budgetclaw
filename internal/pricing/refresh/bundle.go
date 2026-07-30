package refresh

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/RoninForge/budgetclaw/internal/pricing"
)

// Parsing and plausibility checks for a fetched pricing bundle.
//
// A valid signature proves the file is the one we published. It does not
// prove the numbers in it are right: an upstream data-entry mistake would
// be signed just as faithfully as a correct price. These checks are the
// second line, and they are deliberately blunt. They are not trying to
// audit prices, only to refuse changes no legitimate release would make.
//
// Every rejection means the same thing to the user: the fetched data is
// discarded and the table already in force stays. That is stale but
// honest, and the unpriced-event reporting still surfaces any gap.

// ErrRejected reports that a bundle parsed and verified but failed a
// plausibility check. It carries a stable reason for logs and diagnostics.
var ErrRejected = errors.New("pricing bundle rejected")

// Plausibility bounds.
//
// The rate window exists to catch unit errors, which are the realistic
// upstream mistake: a price recorded per thousand tokens instead of per
// million, or a decimal point lost. Real Claude rates sit between about
// $0.25 and $75 per MTok, so this window is wide enough to never fire on
// a genuine price and narrow enough to catch a factor-of-1000 slip.
const (
	minRatePerMTok = 0.005
	maxRatePerMTok = 1000.0

	// A single model's rate changing by more than this factor in one
	// release is not something we will apply unattended.
	maxRateJumpFactor = 10.0

	// Losing at least this fraction of the models we already price looks
	// like a truncated or wrongly-filtered export, not a release.
	maxModelLossFraction = 0.5

	// A dataModified further ahead than this is a clock or data problem.
	maxFutureSkew = 48 * time.Hour
)

// wireBundle is the published bundle shape. Deliberately mirrors the
// per-model documents in the dataset's models/** directory, so this
// parses the same shape the build-time codegen does.
type wireBundle struct {
	SchemaVersion string `json:"schemaVersion"`
	DataModified  string `json:"dataModified"`
	License       string `json:"license"`
	Provider      string `json:"provider"`
	Models        []struct {
		Model      string   `json:"model"`
		Provider   string   `json:"provider"`
		Aliases    []string `json:"aliases"`
		Variations map[string][]struct {
			From     string  `json:"from"`
			To       *string `json:"to"`
			PriceUSD float64 `json:"price_usd"`
			Unit     string  `json:"unit"`
		} `json:"variations"`
	} `json:"models"`
}

// parseBundle turns raw bundle bytes into a table ready to install.
//
// It does not consult the currently active table; that comparison is
// checkPlausible's job, so parsing stays a pure function of its input.
func parseBundle(raw []byte) (pricing.ExternalTable, string, error) {
	var w wireBundle
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields() // a shape we do not understand is not one we should price from
	if err := dec.Decode(&w); err != nil {
		// Retry permissively: an added field upstream should not brick
		// clients, but we want to know it happened.
		var w2 wireBundle
		if err2 := json.Unmarshal(raw, &w2); err2 != nil {
			return pricing.ExternalTable{}, "", fmt.Errorf("%w: unparseable: %v", ErrRejected, err2)
		}
		w = w2
	}

	// Only schema 1.x is understood. A major bump means the meaning of a
	// field may have changed, and guessing is not acceptable here.
	if major := strings.SplitN(w.SchemaVersion, ".", 2)[0]; major != "1" {
		return pricing.ExternalTable{}, "", fmt.Errorf("%w: unsupported schemaVersion %q", ErrRejected, w.SchemaVersion)
	}
	if w.Provider != "anthropic" {
		return pricing.ExternalTable{}, "", fmt.Errorf("%w: unexpected provider %q", ErrRejected, w.Provider)
	}
	if _, err := time.Parse("2006-01-02", w.DataModified); err != nil {
		return pricing.ExternalTable{}, "", fmt.Errorf("%w: bad dataModified %q", ErrRejected, w.DataModified)
	}
	if len(w.Models) == 0 {
		return pricing.ExternalTable{}, "", fmt.Errorf("%w: no models", ErrRejected)
	}

	out := pricing.ExternalTable{DataDate: w.DataModified}
	for _, m := range w.Models {
		if m.Model == "" {
			return pricing.ExternalTable{}, "", fmt.Errorf("%w: a model has no id", ErrRejected)
		}
		in, err := convertWire(m.Model, "input", m.Variations["input"])
		if err != nil {
			return pricing.ExternalTable{}, "", err
		}
		outIvs, err := convertWire(m.Model, "output", m.Variations["output"])
		if err != nil {
			return pricing.ExternalTable{}, "", err
		}
		// A model without both sides cannot price anything; skip it
		// rather than reject the whole bundle, so one odd record upstream
		// does not block every other model's prices.
		if len(in) == 0 || len(outIvs) == 0 {
			continue
		}
		out.Models = append(out.Models, pricing.ExternalModel{
			ID:      m.Model,
			Aliases: m.Aliases,
			Input:   in,
			Output:  outIvs,
		})
	}
	if len(out.Models) == 0 {
		return pricing.ExternalTable{}, "", fmt.Errorf("%w: no model had both input and output rates", ErrRejected)
	}
	return out, w.DataModified, nil
}

// convertWire parses one variation's intervals.
func convertWire(model, variation string, ivs []struct {
	From     string  `json:"from"`
	To       *string `json:"to"`
	PriceUSD float64 `json:"price_usd"`
	Unit     string  `json:"unit"`
}) ([]pricing.ExternalInterval, error) {
	out := make([]pricing.ExternalInterval, 0, len(ivs))
	for i, iv := range ivs {
		from, err := time.Parse("2006-01-02", iv.From)
		if err != nil {
			return nil, fmt.Errorf("%w: %s %s interval %d bad from %q", ErrRejected, model, variation, i, iv.From)
		}
		var to *time.Time
		if iv.To != nil && *iv.To != "" {
			t, err := time.Parse("2006-01-02", *iv.To)
			if err != nil {
				return nil, fmt.Errorf("%w: %s %s interval %d bad to %q", ErrRejected, model, variation, i, *iv.To)
			}
			to = &t
		}
		// The whole table is per-million-token; a different unit would be
		// silently mispriced by a factor we cannot guess.
		if iv.Unit != "" && iv.Unit != "usd_per_mtok" {
			return nil, fmt.Errorf("%w: %s %s interval %d unit %q", ErrRejected, model, variation, i, iv.Unit)
		}
		if iv.PriceUSD < minRatePerMTok || iv.PriceUSD > maxRatePerMTok {
			return nil, fmt.Errorf("%w: %s %s rate %v is outside $%v..$%v per MTok, which suggests a unit error",
				ErrRejected, model, variation, iv.PriceUSD, minRatePerMTok, maxRatePerMTok)
		}
		out = append(out, pricing.ExternalInterval{From: from, To: to, Price: iv.PriceUSD})
	}
	return out, nil
}

// checkPlausible compares a parsed bundle against the table currently in
// force and refuses changes no legitimate release would make.
//
// now is injected so tests do not depend on the wall clock.
func checkPlausible(t pricing.ExternalTable, now time.Time) error {
	newDate, err := time.Parse("2006-01-02", t.DataDate)
	if err != nil {
		return fmt.Errorf("%w: bad dataModified %q", ErrRejected, t.DataDate)
	}

	// Future-dated data means a clock problem on one side or a bad record.
	if newDate.After(now.UTC().Add(maxFutureSkew)) {
		return fmt.Errorf("%w: dataModified %s is more than %s in the future",
			ErrRejected, t.DataDate, maxFutureSkew)
	}

	// Anti-rollback. A signature stays valid forever, so replaying a
	// genuine old bundle is the one attack a signature cannot stop by
	// itself. Refusing to move backwards is what stops it.
	if _, _, activeDate := pricing.ActiveTable(); activeDate != "" {
		if cur, err := time.Parse("2006-01-02", activeDate); err == nil && newDate.Before(cur) {
			return fmt.Errorf("%w: dataModified %s is older than the active table's %s (rollback)",
				ErrRejected, t.DataDate, activeDate)
		}
	}

	// Mass model loss looks like a truncated or wrongly-filtered export.
	active := pricing.KnownModels()
	if len(active) > 0 {
		incoming := make(map[string]bool, len(t.Models))
		for _, m := range t.Models {
			incoming[m.ID] = true
			for _, a := range m.Aliases {
				incoming[a] = true
			}
		}
		missing := 0
		for _, id := range active {
			if !incoming[id] {
				missing++
			}
		}
		if frac := float64(missing) / float64(len(active)); frac >= maxModelLossFraction {
			return fmt.Errorf("%w: %d of %d known models absent (%.0f%%), which looks truncated",
				ErrRejected, missing, len(active), frac*100)
		}
	}

	// A large rate move on a model we already price is held rather than
	// applied. Compared at "now" against the incoming open interval,
	// because that is the rate that would start being charged.
	for _, m := range t.Models {
		cur, err := pricing.RatesForAt(m.ID, now)
		if err != nil {
			continue // new model, nothing to compare against
		}
		next, ok := openRate(m.Input)
		if !ok || cur.InputPerMTok <= 0 {
			continue
		}
		if factor := ratio(next, cur.InputPerMTok); factor > maxRateJumpFactor {
			return fmt.Errorf("%w: %s input rate moves %.0fx (%v to %v), which needs a human look",
				ErrRejected, m.ID, factor, cur.InputPerMTok, next)
		}
	}

	return nil
}

// openRate returns the price of the series' open interval, if any.
func openRate(ivs []pricing.ExternalInterval) (float64, bool) {
	for _, iv := range ivs {
		if iv.To == nil {
			return iv.Price, true
		}
	}
	return 0, false
}

// ratio is the larger of a/b and b/a, so a tenfold move is caught in
// either direction.
func ratio(a, b float64) float64 {
	if a <= 0 || b <= 0 {
		return math.Inf(1)
	}
	return math.Max(a/b, b/a)
}
