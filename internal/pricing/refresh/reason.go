package refresh

import (
	"errors"
	"fmt"
)

// Stable reason codes for a refused bundle.
//
// Every rejection already wraps ErrRejected, which is what callers branch
// on. The code is the finer grain underneath: it names WHICH gate fired,
// in a string that is safe to log, safe to compare in a test, and stable
// across releases. Two things need that.
//
// The first is diagnosis in the field. "pricing bundle rejected" tells a
// user nothing they can act on; reason=rate_out_of_range tells them the
// published data has a unit error, which is our bug, not theirs.
//
// The second is keeping this guard from rotting. The hostile-payload
// corpus asserts the reason, not merely that something was refused. That
// distinction matters more than it looks: a payload aimed at the rate
// window could start failing at the schema check instead, the intended
// gate would stop being exercised, and a test that only checked for
// ErrRejected would stay green while the gate decayed.
//
// Signature failures deliberately have NO reason code. verifySignature
// keeps its failures opaque on purpose, so nothing can branch on why a
// signature did not verify. That is a security property, and giving it
// sub-codes would invite exactly the branching it forbids.

// Reason is a stable, machine-readable code naming the gate that refused
// a bundle.
type Reason string

// The complete set. Adding one here without covering it in the corpus
// fails TestHostileCorpus, which is the point: a gate nothing exercises
// is a gate that can quietly stop working.
const (
	// ReasonNone means the error was not a rejection.
	ReasonNone Reason = ""

	// Parse stage: the bundle's own shape and values, judged without
	// reference to the table currently in force.

	// ReasonUnparseable is not valid JSON, or not an object at all. What a
	// captive portal or an error page looks like.
	ReasonUnparseable Reason = "unparseable"
	// ReasonUnsupportedSchema is a schemaVersion outside 1.x, where a field
	// may have changed meaning and guessing is not acceptable.
	ReasonUnsupportedSchema Reason = "unsupported_schema"
	// ReasonUnexpectedProvider is a bundle for someone else's prices.
	ReasonUnexpectedProvider Reason = "unexpected_provider"
	// ReasonBadDataDate is a missing or unparseable dataModified.
	ReasonBadDataDate Reason = "bad_data_date"
	// ReasonNoModels is a bundle carrying no models at all.
	ReasonNoModels Reason = "no_models"
	// ReasonMalformedModel is a model record missing its identity.
	ReasonMalformedModel Reason = "malformed_model"
	// ReasonBadIntervalDate is an unparseable from or to on an interval.
	ReasonBadIntervalDate Reason = "bad_interval_date"
	// ReasonUnexpectedUnit is a rate in units other than per-million-token,
	// which would be mispriced by a factor we cannot guess.
	ReasonUnexpectedUnit Reason = "unexpected_unit"
	// ReasonRateOutOfRange is a rate outside the plausible window: the
	// realistic upstream mistake, a price recorded per thousand tokens
	// instead of per million.
	ReasonRateOutOfRange Reason = "rate_out_of_range"
	// ReasonNoPriceableModel is a bundle where no model had both an input
	// and an output series, so nothing in it could price anything.
	ReasonNoPriceableModel Reason = "no_priceable_model"

	// Gate stage: judged by comparison against the table in force, so
	// these cannot be decided from the bundle alone.

	// ReasonFutureDataDate is data dated implausibly far ahead, meaning a
	// clock problem on one side or a bad record.
	ReasonFutureDataDate Reason = "future_data_date"
	// ReasonRollback is older data than what is already installed. A
	// signature stays valid forever, so replaying a genuine old release is
	// the one attack signing cannot stop by itself.
	ReasonRollback Reason = "rollback"
	// ReasonModelLoss is the absence of a large share of the models we
	// already price, which looks like a truncated or wrongly-filtered
	// export rather than a release.
	ReasonModelLoss Reason = "model_loss"
	// ReasonRateJump is a rate moving further in one release than we will
	// apply unattended.
	ReasonRateJump Reason = "rate_jump"
)

// allReasons is every code a rejection can carry, for the completeness
// check in the corpus test.
var allReasons = []Reason{
	ReasonUnparseable,
	ReasonUnsupportedSchema,
	ReasonUnexpectedProvider,
	ReasonBadDataDate,
	ReasonNoModels,
	ReasonMalformedModel,
	ReasonBadIntervalDate,
	ReasonUnexpectedUnit,
	ReasonRateOutOfRange,
	ReasonNoPriceableModel,
	ReasonFutureDataDate,
	ReasonRollback,
	ReasonModelLoss,
	ReasonRateJump,
}

// rejection is a refusal that carries its reason code.
type rejection struct {
	reason Reason
	msg    string
}

func (e *rejection) Error() string {
	return fmt.Sprintf("%s [%s]: %s", ErrRejected.Error(), e.reason, e.msg)
}

// Unwrap makes errors.Is(err, ErrRejected) true, so existing callers that
// branch on the sentinel keep working unchanged.
func (e *rejection) Unwrap() error { return ErrRejected }

// reject builds a rejection with a reason code.
func reject(r Reason, format string, args ...any) error {
	return &rejection{reason: r, msg: fmt.Sprintf(format, args...)}
}

// ReasonOf returns the stable code from a rejection, or ReasonNone when
// err is not one. Use it for logging and diagnostics; use
// errors.Is(err, ErrRejected) to decide whether to act.
func ReasonOf(err error) Reason {
	var r *rejection
	if errors.As(err, &r) {
		return r.reason
	}
	return ReasonNone
}
