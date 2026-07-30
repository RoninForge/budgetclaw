package cli

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// The zero-egress canary, at the level a user experiences it.
//
// `budgetclaw pricing auto off` prints "budgetclaw makes no network
// requests." That is a promise, and this is what holds it. A tripwire
// replaces the default HTTP transport and fails on any attempt to use it,
// then the ordinary commands are run through it.
//
// The refresh package has its own version of this covering the fetch
// internals. This one covers the wiring, which is where the mistake would
// actually be made: a status command that eagerly warms a cache, a version
// check bolted onto the root command, a refresher started before the
// opt-in was read.

// egressTripwire refuses every request and records it.
type egressTripwire struct {
	mu       sync.Mutex
	attempts []string
}

func (tw *egressTripwire) RoundTrip(req *http.Request) (*http.Response, error) {
	tw.mu.Lock()
	tw.attempts = append(tw.attempts, req.Method+" "+req.URL.String())
	tw.mu.Unlock()
	return nil, fmt.Errorf("zero-egress canary: unexpected request to %s", req.URL)
}

// noEgress arms the tripwire for the rest of the test.
func noEgress(t *testing.T) {
	t.Helper()
	tw := &egressTripwire{}
	saved := http.DefaultTransport
	http.DefaultTransport = tw
	t.Cleanup(func() {
		http.DefaultTransport = saved
		tw.mu.Lock()
		defer tw.mu.Unlock()
		if len(tw.attempts) > 0 {
			t.Errorf("budgetclaw made %d network request(s) when it must make none: %v",
				len(tw.attempts), tw.attempts)
		}
	})
}

// TestDefaultPostureMakesNoRequests runs the commands a user actually
// types, on a machine that never opted in. Every one must be local.
func TestDefaultPostureMakesNoRequests(t *testing.T) {
	setupXDG(t)
	if _, _, err := execCmd(t, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}

	noEgress(t)

	// Reads, listings and diagnostics. `status` is the one run most often,
	// and `pricing provenance` and `status` both consult the price table,
	// which is the code most likely to grow a fetch by accident.
	for _, args := range [][]string{
		{"version"},
		{"status"},
		{"pricing", "rates"},
		{"pricing", "provenance"},
		{"pricing", "diagnose"},
		{"limit", "list"},
		{"locks", "list"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, _, err := execCmd(t, args...); err != nil {
				t.Fatalf("%v: %v", args, err)
			}
		})
	}
}

// TestRefreshWithoutConsentMakesNoRequest is the load-bearing case. The
// command exists to fetch, and it still must not fetch until the user has
// said yes.
func TestRefreshWithoutConsentMakesNoRequest(t *testing.T) {
	setupXDG(t)
	if _, _, err := execCmd(t, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}

	noEgress(t)

	stdout, _, err := execCmd(t, "pricing", "refresh")
	if err != nil {
		t.Fatalf("pricing refresh: %v", err)
	}
	if !strings.Contains(stdout, "off") {
		t.Errorf("output should say auto-update is off, got %q", stdout)
	}
}

// TestTurningAutoOnDoesNotItselfFetch separates consent from action.
// Flipping the setting writes config and explains what will happen; the
// user then chooses when the first request goes out.
func TestTurningAutoOnDoesNotItselfFetch(t *testing.T) {
	setupXDG(t)
	if _, _, err := execCmd(t, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}

	noEgress(t)

	stdout, _, err := execCmd(t, "pricing", "auto", "on")
	if err != nil {
		t.Fatalf("pricing auto on: %v", err)
	}
	if !strings.Contains(stdout, "Price auto-update is ON") {
		t.Errorf("output should confirm the setting, got %q", stdout)
	}

	// And with the setting now on, a plain read still stays local: there
	// is no cache yet, and a one-shot command must not go and build one.
	if _, _, err := execCmd(t, "status"); err != nil {
		t.Fatalf("status after opting in: %v", err)
	}
	if _, _, err := execCmd(t, "pricing", "provenance"); err != nil {
		t.Fatalf("pricing provenance after opting in: %v", err)
	}
}

// TestAutoOffAfterOnGoesQuietAgain covers withdrawing consent. Turning it
// back off must stop the requests, not merely stop advertising them.
func TestAutoOffAfterOnGoesQuietAgain(t *testing.T) {
	setupXDG(t)
	if _, _, err := execCmd(t, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, _, err := execCmd(t, "pricing", "auto", "on"); err != nil {
		t.Fatalf("pricing auto on: %v", err)
	}

	noEgress(t)

	stdout, _, err := execCmd(t, "pricing", "auto", "off")
	if err != nil {
		t.Fatalf("pricing auto off: %v", err)
	}
	if !strings.Contains(stdout, "makes no network requests") {
		t.Errorf("output should restate the offline guarantee, got %q", stdout)
	}

	// The refresher must now decline to exist at all, rather than exist
	// and choose not to act.
	if r := newPricingRefresher(false, "", nil, nil); r != nil {
		t.Error("newPricingRefresher returned a refresher while auto-update is off")
	}

	if _, _, err := execCmd(t, "pricing", "refresh"); err != nil {
		t.Fatalf("pricing refresh: %v", err)
	}
}

// TestCanaryObservesAnIntentionalFetch is the positive control, and it is
// the reason the tests above mean anything. It proves the tripwire is
// actually in the path of a request the CLI initiates, so "zero requests"
// is a measurement rather than an absence of measurement.
//
// It doubles as coverage for --force, which is the documented way to fetch
// once without changing the saved setting.
func TestCanaryObservesAnIntentionalFetch(t *testing.T) {
	setupXDG(t)
	if _, _, err := execCmd(t, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}

	tw := &egressTripwire{}
	saved := http.DefaultTransport
	http.DefaultTransport = tw
	defer func() { http.DefaultTransport = saved }()

	// Auto-update is off, so only --force should get out.
	stdout, _, err := execCmd(t, "pricing", "refresh", "--force")
	if err != nil {
		t.Fatalf("pricing refresh --force: %v", err)
	}

	tw.mu.Lock()
	attempts := len(tw.attempts)
	first := ""
	if attempts > 0 {
		first = tw.attempts[0]
	}
	tw.mu.Unlock()

	if attempts == 0 {
		t.Fatal("--force made no request, so the canary above proves nothing: " +
			"it cannot distinguish a tool that stays offline from a transport it never sees")
	}
	if !strings.Contains(first, "roninforge.org") {
		t.Errorf("the request went to %q, want the published price table", first)
	}
	// A blocked transport is an ordinary offline condition, not a failure.
	if !strings.Contains(stdout, "Could not reach") {
		t.Errorf("a failed fetch should report it plainly and change nothing, got %q", stdout)
	}
}

// TestDisabledRefresherIsInert pins the nil-receiver behaviour the watch
// daemon depends on. Every entry point has to be safe to call when the
// user has not opted in, because watch.go calls them unconditionally.
func TestDisabledRefresherIsInert(t *testing.T) {
	setupXDG(t)
	noEgress(t)

	r := newPricingRefresher(false, "https://example.invalid/anthropic.json", nil, nil)
	if r != nil {
		t.Fatal("a disabled refresher should be nil")
	}

	// None of these may panic on a nil receiver, and none may reach out.
	r.LoadCache()
	r.Trigger()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx) // must return immediately rather than block or dial
}
