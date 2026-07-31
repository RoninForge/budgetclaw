package refresh

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// The zero-egress canary.
//
// "Off by default, and offline operation stays fully supported" is a
// promise about behaviour, so it gets tested like one. A tripwire replaces
// the default HTTP transport and fails the test on any attempt to use it.
//
// This catches the failure mode that documentation cannot: a future change
// that adds a version check, a telemetry ping, or an eager warm-up on a
// path a user never opted into. The claim in `pricing auto off` is that
// budgetclaw makes NO network requests, and a test is the only thing that
// keeps that true after the person who wrote it has moved on.
//
// The tripwire works because the production client leaves Transport nil
// and so uses http.DefaultTransport. A future client that installs its own
// transport would slip past this, which is exactly why the depguard rule
// in .golangci.yml confines net/http to the few packages allowed to have
// it: the two checks cover each other.

// tripwire is a RoundTripper that refuses every request and records it.
type tripwire struct {
	mu       sync.Mutex
	attempts []string
}

func (tw *tripwire) RoundTrip(req *http.Request) (*http.Response, error) {
	tw.mu.Lock()
	tw.attempts = append(tw.attempts, req.Method+" "+req.URL.String())
	tw.mu.Unlock()
	return nil, fmt.Errorf("zero-egress canary: unexpected request to %s", req.URL)
}

func (tw *tripwire) tripped() []string {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	return append([]string(nil), tw.attempts...)
}

// noEgress installs the tripwire for the duration of a test and reports
// any request that was attempted.
func noEgress(t *testing.T) *tripwire {
	t.Helper()
	tw := &tripwire{}
	saved := http.DefaultTransport
	http.DefaultTransport = tw
	t.Cleanup(func() {
		http.DefaultTransport = saved
		if got := tw.tripped(); len(got) > 0 {
			t.Errorf("expected zero network requests, got %d: %v", len(got), got)
		}
	})
	return tw
}

// TestLocalOperationsMakeNoRequests covers everything the package does
// with bytes it already has. None of it should touch the network, and the
// cache-load path is the interesting one: it runs on every command for an
// opted-in user, so an accidental fetch there would be constant.
func TestLocalOperationsMakeNoRequests(t *testing.T) {
	isolate(t)
	noEgress(t)

	now := fixtureNow(t)

	// No cache present: must return quietly, not go looking for one.
	res, err := LoadCachedTable(now)
	if err != nil {
		t.Errorf("LoadCachedTable with no cache: %v", err)
	}
	if res.Updated {
		t.Error("LoadCachedTable reported an update with nothing cached")
	}

	// Verify, parse and install from bytes in hand.
	bundle, sig := liveFixture(t)
	if _, err := verifySignature(bundle, sig, TrustedKeys); err != nil {
		t.Errorf("verifySignature: %v", err)
	}
	table, _, err := parseBundle(bundle)
	if err != nil {
		t.Errorf("parseBundle: %v", err)
	}
	if err := checkPlausible(table, now); err != nil {
		t.Errorf("checkPlausible: %v", err)
	}
}

// TestCacheLoadOfAVerifiedBundleMakesNoRequests is the restart path for a
// user who HAS opted in. Fresher prices must come back from disk without a
// round trip, so a laptop opening on a plane still prices correctly.
func TestCacheLoadOfAVerifiedBundleMakesNoRequests(t *testing.T) {
	isolate(t)
	now := fixtureNow(t)

	// Populate the cache through the real path, with the tripwire not yet
	// armed: this fetch is expected.
	bundle, sig := liveFixture(t)
	if err := saveCache(cached{bundle: bundle, sig: sig, etag: `"fixture"`}); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}

	noEgress(t)

	res, err := LoadCachedTable(now)
	if err != nil {
		t.Fatalf("LoadCachedTable: %v", err)
	}
	if !res.Updated {
		t.Error("a seeded cache should have installed a table")
	}
}

// TestTripwireActuallyTrips guards the guard. A canary that cannot detect
// a request is worse than none, because it reads as proof.
func TestTripwireActuallyTrips(t *testing.T) {
	tw := &tripwire{}
	saved := http.DefaultTransport
	http.DefaultTransport = tw
	defer func() { http.DefaultTransport = saved }()

	//nolint:noctx // deliberately the simplest possible request; the point
	// is only to prove the transport is intercepted.
	if _, err := http.Get("https://example.invalid/should-be-blocked"); err == nil {
		t.Fatal("the tripwire let a request through")
	}
	if got := tw.tripped(); len(got) != 1 {
		t.Fatalf("tripwire recorded %d attempts, want 1: %v", len(got), got)
	}
}

// TestProductionClientUsesTheDefaultTransport is what makes the canary
// meaningful. If a future change gave the client its own transport, every
// egress test in this file would keep passing while testing nothing.
func TestProductionClientUsesTheDefaultTransport(t *testing.T) {
	if c := defaultClient(); c.Transport != nil {
		t.Fatalf("defaultClient has its own Transport (%T), so the zero-egress canary "+
			"no longer observes its requests: either route it through "+
			"http.DefaultTransport or teach noEgress about the new transport", c.Transport)
	}
}
