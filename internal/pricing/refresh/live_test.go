package refresh

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/RoninForge/budgetclaw/internal/pricing"
)

// The live-signature check.
//
// Everything else in this package tests against a pinned fixture, which is
// what a test should do: it is reproducible, and it does not depend on a
// server being up. But a pinned fixture cannot notice the one failure that
// costs the most and shows the least.
//
// That failure is a signing-key rotation, or any publisher change that
// alters the signature format, without the matching public key compiled
// into budgetclaw. Every opted-in client would then reject every update.
// Correctly, quietly, and forever: the signature does not verify, so the
// table in force is kept, a debug line is logged, and prices freeze. No
// crash, no error a user would see, no alert. Exactly the shape of the
// staleness problem this whole guard was built to end, reintroduced one
// level up.
//
// So this runs against the real published file, on a schedule, in CI. It
// is env-gated rather than build-tagged so it cannot run by accident on a
// developer machine or in the ordinary test job, where a network
// dependency has no business being.
//
// Enable with: BUDGETCLAW_LIVE_PRICING_CHECK=1 go test ./internal/pricing/refresh/ -run TestLive

const liveCheckEnv = "BUDGETCLAW_LIVE_PRICING_CHECK"

// TestLivePublishedBundleVerifies fetches the real bundle through the real
// code path and asserts it is still something this binary would accept.
func TestLivePublishedBundleVerifies(t *testing.T) {
	requireLiveCheck(t)
	isolate(t)

	now := time.Now().UTC()

	res, err := Refresh(context.Background(), DefaultBundleURL, now)
	switch {
	case errors.Is(err, ErrOffline):
		// A network problem is not a signing problem. Whether the service
		// is reachable is a question for uptime monitoring; conflating the
		// two here would make this check cry wolf and get muted, which
		// costs more than the coverage is worth.
		t.Skipf("could not reach the published bundle, which this check does not judge: %v", err)

	case errors.Is(err, ErrBadSignature):
		t.Fatalf("THE LIVE BUNDLE DOES NOT VERIFY against any key compiled into this binary.\n"+
			"Every client with price auto-update on is now silently refusing every update: "+
			"the signature fails, the built-in table is kept, and prices freeze with no user-visible error.\n"+
			"Most likely the signing key was rotated without adding the new public key to "+
			"internal/pricing/refresh/keys.go.\nerror: %v", err)

	case errors.Is(err, ErrRejected):
		t.Fatalf("the live bundle verified but failed the %q gate, so no client will install it: %v",
			ReasonOf(err), err)

	case errors.Is(err, ErrNotModified):
		// Only possible with a warm cache, which isolate() prevents.
		t.Fatalf("unexpected 304 with no cache present: %v", err)

	case err != nil:
		t.Fatalf("refresh failed: %v", err)
	}

	if !res.Updated {
		t.Fatal("refresh reported no update despite succeeding")
	}
	t.Logf("live bundle OK: %s (data %s), %d models, verified against %s",
		res.Tag, res.DataDate, res.Models, res.KeyName)

	if res.KeyName == "" {
		t.Error("no key name recorded, so we cannot tell which key signed this")
	}
	if res.Models < 5 {
		t.Errorf("the live bundle carries only %d models, which looks truncated", res.Models)
	}

	// The published data must not be older than what this binary already
	// ships. If it is, the publisher has regressed and opted-in clients
	// are being offered a downgrade, which they will refuse as a rollback.
	assertNotOlderThanBuiltIn(t, res.DataDate)
}

// TestLiveSignatureFileIsWellFormed checks the detached signature on its
// own terms, so a malformed .minisig is distinguishable from a content
// mismatch when something does break.
func TestLiveSignatureFileIsWellFormed(t *testing.T) {
	requireLiveCheck(t)
	isolate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*requestTimeout)
	defer cancel()

	bundle, _, err := get(ctx, defaultClient(), DefaultBundleURL, "", maxBundleBytes)
	if err != nil {
		t.Skipf("could not fetch the bundle: %v", err)
	}
	sig, _, err := get(ctx, defaultClient(), DefaultBundleURL+".minisig", "", maxSignatureBytes)
	if err != nil {
		t.Skipf("could not fetch the signature: %v", err)
	}

	v, err := verifySignature(bundle, sig, TrustedKeys)
	if err != nil {
		t.Fatalf("the live signature does not verify: %v", err)
	}

	// The trusted comment is the only part of the signature file whose
	// contents we can believe, and it is where the release is recorded.
	// A blank one means the publisher stopped stamping it.
	if v.TrustedComment == "" {
		t.Error("the trusted comment is empty, so a fetched table has no provenance to report")
	}
	if tag := tagFromComment(v.TrustedComment); tag == "" || tag == v.TrustedComment {
		t.Errorf("no release tag found in the trusted comment %q", v.TrustedComment)
	}
}

// assertNotOlderThanBuiltIn compares published data against the compiled-in
// dataset date.
func assertNotOlderThanBuiltIn(t *testing.T, liveDate string) {
	t.Helper()

	pricing.RestoreBuiltIn()
	_, _, builtInDate := pricing.ActiveTable()

	live, err := time.Parse("2006-01-02", liveDate)
	if err != nil {
		t.Errorf("live dataModified %q is unparseable", liveDate)
		return
	}
	built, err := time.Parse("2006-01-02", builtInDate)
	if err != nil {
		t.Errorf("compiled-in dataModified %q is unparseable", builtInDate)
		return
	}
	if live.Before(built) {
		t.Errorf("the published data is dated %s but this binary already ships %s: "+
			"opted-in clients will refuse the published bundle as a rollback",
			liveDate, builtInDate)
	}
}

func requireLiveCheck(t *testing.T) {
	t.Helper()
	if os.Getenv(liveCheckEnv) == "" {
		t.Skipf("network test, set %s=1 to run", liveCheckEnv)
	}
}
