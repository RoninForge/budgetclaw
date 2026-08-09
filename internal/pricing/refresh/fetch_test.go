package refresh

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RoninForge/budgetclaw/internal/pricing"
)

// These tests serve the REAL bundle and signature over a local server, so
// the whole path (conditional GET, verify, parse, gates, install) is
// exercised against production bytes rather than hand-made fixtures.

// isolate points the cache at a temp dir and restores the built-in
// pricing table, so tests never touch the developer's real cache or leak
// an installed table into another test.
func isolate(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	t.Setenv("HOME", dir)
	pricing.RestoreBuiltIn()
	t.Cleanup(pricing.RestoreBuiltIn)
}

// bundleServer serves the live fixtures, optionally mutated.
func bundleServer(t *testing.T, mutate func(bundle, sig []byte) (b, s []byte, status int, etag string)) *httptest.Server {
	t.Helper()
	bundle, sig := liveFixture(t)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, s, status, etag := bundle, sig, http.StatusOK, `"fixture"`
		if mutate != nil {
			b, s, status, etag = mutate(bundle, sig)
		}
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".minisig") {
			_, _ = w.Write(s)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// refreshVia runs Refresh against a test server, using its TLS client so
// the https-only rule is still enforced for real.
func refreshVia(ctx context.Context, srv *httptest.Server, now time.Time) (Result, error) {
	// Swap in the test server's client so its self-signed cert is trusted.
	// The https-only check in get() still applies to the URL.
	return refreshWithClient(ctx, srv.URL+"/history/anthropic.json", now, srv.Client())
}

// TestRefreshInstallsVerifiedBundle is the happy path end to end.
func TestRefreshInstallsVerifiedBundle(t *testing.T) {
	isolate(t)
	srv := bundleServer(t, nil)

	// A date after the fixture's dataModified so the freshness gates pass.
	now := fixtureNow(t)

	res, err := refreshVia(context.Background(), srv, now)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !res.Updated {
		t.Error("Updated = false")
	}
	if res.Models < 5 {
		t.Errorf("Models = %d, want the real bundle's model count", res.Models)
	}
	if !strings.HasPrefix(res.Tag, "v2026.") {
		t.Errorf("Tag = %q, want it read from the signed trusted comment", res.Tag)
	}
	if res.KeyName != "ai-price-index-2026-07" {
		t.Errorf("KeyName = %q", res.KeyName)
	}

	// The table must actually be in force now.
	src, tag, _ := pricing.ActiveTable()
	if src != pricing.SourceFetched || tag != res.Tag {
		t.Errorf("active table = %q %q, want fetched %q", src, tag, res.Tag)
	}
}

// TestRefreshRoundTripsThroughTheCache proves an enabled client keeps its
// fresher prices across a restart, and that the cache is re-verified
// rather than trusted.
func TestRefreshRoundTripsThroughTheCache(t *testing.T) {
	isolate(t)
	srv := bundleServer(t, nil)
	now := fixtureNow(t)

	first, err := refreshVia(context.Background(), srv, now)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// Simulate a restart: back to the built-in table, then load the cache
	// with no network at all.
	pricing.RestoreBuiltIn()
	if src, _, _ := pricing.ActiveTable(); src != pricing.SourceBuiltIn {
		t.Fatal("setup: expected the built-in table after restore")
	}

	res, err := LoadCachedTable(now)
	if err != nil {
		t.Fatalf("LoadCachedTable: %v", err)
	}
	if !res.Updated || res.Tag != first.Tag {
		t.Errorf("cache load = %+v, want the same tag as the fetch (%q)", res, first.Tag)
	}
}

// TestCachedTableIsReverifiedNotTrusted covers local tampering with the
// cache file. Re-verification is cheap and catches corruption too.
func TestCachedTableIsReverifiedNotTrusted(t *testing.T) {
	isolate(t)
	srv := bundleServer(t, nil)
	now := fixtureNow(t)

	if _, err := refreshVia(context.Background(), srv, now); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// Corrupt the cached bundle.
	c, ok := loadCache()
	if !ok {
		t.Fatal("expected a cache after a successful refresh")
	}
	if err := saveCache(cached{bundle: append(c.bundle, ' '), sig: c.sig, etag: c.etag}); err != nil {
		t.Fatal(err)
	}

	pricing.RestoreBuiltIn()
	if _, err := LoadCachedTable(now); !errors.Is(err, ErrBadSignature) {
		t.Errorf("a tampered cache should fail verification, got %v", err)
	}
	if src, _, _ := pricing.ActiveTable(); src != pricing.SourceBuiltIn {
		t.Error("a tampered cache must leave the built-in table in force")
	}
}

// TestRefreshHandlesTransportFailures checks every ordinary failure ends
// as ErrOffline or ErrNotModified, never as a pricing change.
func TestRefreshHandlesTransportFailures(t *testing.T) {
	now := fixtureNow(t)

	cases := map[string]struct {
		mutate func(b, s []byte) ([]byte, []byte, int, string)
		want   error
	}{
		"304 not modified": {
			mutate: func(b, s []byte) ([]byte, []byte, int, string) {
				return b, s, http.StatusNotModified, `"fixture"`
			},
			want: ErrNotModified,
		},
		"500 server error": {
			mutate: func(b, s []byte) ([]byte, []byte, int, string) {
				return b, s, http.StatusInternalServerError, ""
			},
			want: ErrOffline,
		},
		"404 missing": {
			mutate: func(b, s []byte) ([]byte, []byte, int, string) {
				return b, s, http.StatusNotFound, ""
			},
			want: ErrOffline,
		},
		// An HTML body fails the SIGNATURE, not the parser, because
		// verification runs first: unverified bytes are never parsed.
		"html captive portal": {
			mutate: func(b, s []byte) ([]byte, []byte, int, string) {
				return []byte("<html>sign in to continue</html>"), s, http.StatusOK, ""
			},
			want: ErrBadSignature,
		},
		"oversized body": {
			mutate: func(b, s []byte) ([]byte, []byte, int, string) {
				return make([]byte, maxBundleBytes+10), s, http.StatusOK, ""
			},
			want: ErrOffline,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			srv := bundleServer(t, tc.mutate)
			_, err := refreshVia(context.Background(), srv, now)
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
			if src, _, _ := pricing.ActiveTable(); src != pricing.SourceBuiltIn {
				t.Error("a failed refresh must leave the built-in table in force")
			}
		})
	}
}

// TestRefreshRejectsTamperedBundleOverTheWire is the MITM case.
func TestRefreshRejectsTamperedBundleOverTheWire(t *testing.T) {
	isolate(t)
	srv := bundleServer(t, func(b, s []byte) ([]byte, []byte, int, string) {
		// Flip a price digit, keeping the genuine signature.
		tampered := append([]byte{}, b...)
		if i := strings.Index(string(tampered), `"price_usd": 5`); i >= 0 {
			tampered[i+len(`"price_usd": `)] = '9'
		}
		return tampered, s, http.StatusOK, ""
	})

	now := fixtureNow(t)
	if _, err := refreshVia(context.Background(), srv, now); !errors.Is(err, ErrBadSignature) {
		t.Errorf("error = %v, want ErrBadSignature", err)
	}
	if src, _, _ := pricing.ActiveTable(); src != pricing.SourceBuiltIn {
		t.Error("a tampered bundle must leave the built-in table in force")
	}
}

// TestRefreshRejectsNonHTTPS keeps the transport requirement honest.
func TestRefreshRejectsNonHTTPS(t *testing.T) {
	isolate(t)
	_, err := Refresh(context.Background(), "http://example.invalid/anthropic.json",
		fixtureNow(t))
	if !errors.Is(err, ErrOffline) {
		t.Errorf("error = %v, want ErrOffline", err)
	}
}

// TestPlausibilityGates covers the checks a valid signature cannot
// provide: data that is authentic but wrong, replayed, or truncated.
//
// The table under test is parsed from the REAL bundle and then mutated,
// so "plausible" means plausible against the actual production dataset
// rather than against a hand-made stub.
func TestPlausibilityGates(t *testing.T) {
	bundle, _ := liveFixture(t)
	base, _, err := parseBundle(bundle)
	if err != nil {
		t.Fatalf("parse the live bundle: %v", err)
	}
	base.Tag = "v" + fixtureDataDate(t) + "-aaaaaaa"
	base.DataDate = fixtureDataDate(t)
	now := fixtureNow(t)

	t.Run("accepts the real dataset", func(t *testing.T) {
		isolate(t)
		if err := checkPlausible(base, now); err != nil {
			t.Errorf("the real dataset was rejected: %v", err)
		}
	})

	t.Run("rejects a future dataModified", func(t *testing.T) {
		isolate(t)
		tbl := base
		tbl.DataDate = "2027-01-01"
		if err := checkPlausible(tbl, now); !errors.Is(err, ErrRejected) {
			t.Errorf("error = %v, want ErrRejected", err)
		}
	})

	t.Run("rejects a rollback to older data", func(t *testing.T) {
		isolate(t)
		// Install the newer table first, then offer an older one. This is
		// the one attack a signature cannot stop on its own: replaying a
		// genuine, still-validly-signed old release.
		if err := pricing.Install(base); err != nil {
			t.Fatal(err)
		}
		older := base
		older.DataDate = "2026-06-01"
		if err := checkPlausible(older, now); !errors.Is(err, ErrRejected) {
			t.Errorf("error = %v, want ErrRejected (replaying an old signed bundle)", err)
		}
	})

	t.Run("rejects a tenfold rate jump", func(t *testing.T) {
		isolate(t)
		jump := base
		jump.Models = append([]pricing.ExternalModel{}, base.Models...)
		for i, m := range jump.Models {
			if m.ID != "claude-opus-4-8" {
				continue
			}
			hundredfold := make([]pricing.ExternalInterval, len(m.Input))
			copy(hundredfold, m.Input)
			for j := range hundredfold {
				hundredfold[j].Price *= 100
			}
			jump.Models[i].Input = hundredfold
		}
		if err := checkPlausible(jump, now); !errors.Is(err, ErrRejected) {
			t.Errorf("error = %v, want ErrRejected", err)
		}
	})

	t.Run("rejects a truncated model set", func(t *testing.T) {
		isolate(t)
		truncated := base
		truncated.Models = base.Models[:1] // one model out of twenty
		if err := checkPlausible(truncated, now); !errors.Is(err, ErrRejected) {
			t.Errorf("error = %v, want ErrRejected for mass model loss", err)
		}
	})
}

// TestParseRejectsUnitErrors covers the realistic upstream mistake: a
// rate recorded per thousand tokens instead of per million.
func TestParseRejectsUnitErrors(t *testing.T) {
	bundle, _ := liveFixture(t)
	// 5 -> 5000 is a factor-1000 slip, the classic unit error.
	broken := strings.Replace(string(bundle), `"price_usd": 5,`, `"price_usd": 5000,`, 1)

	if _, _, err := parseBundle([]byte(broken)); !errors.Is(err, ErrRejected) {
		t.Errorf("error = %v, want ErrRejected for an out-of-range rate", err)
	}
}
