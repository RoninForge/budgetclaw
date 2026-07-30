package refresh

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RoninForge/budgetclaw/internal/pricing"
)

// The hostile-payload corpus.
//
// testdata/hostile/ holds pinned payloads that must every one be refused,
// and refused by a named gate. manifest.json explains each and why the
// corpus exists; the short version is that a signature proves a bundle is
// ours, not that its numbers are right, and a wrong export would be signed
// just as faithfully as a correct one.
//
// The corpus is driven against parseBundle and checkPlausible directly
// rather than through the fetch path. That is not a shortcut: verification
// runs before parsing, so none of these payloads is reachable over the
// network without our signing key. Testing them here is what covers the
// two cases a signature cannot help with, a bad export signed legitimately
// and a compromised key.

type hostileManifest struct {
	Cases []struct {
		File   string `json:"file"`
		Reason Reason `json:"reason"`
		Note   string `json:"note"`
	} `json:"cases"`
}

// TestHostileCorpus runs the pinned files, then the gate-stage mutations
// that need a real table to compare against, then asserts the two halves
// together cover every declared Reason.
//
// It is one test rather than three so the completeness check can see what
// the other halves actually exercised. A gate with no payload aimed at it
// is a gate that can stop working without anyone noticing, which is the
// specific decay this whole file exists to prevent.
func TestHostileCorpus(t *testing.T) {
	covered := map[Reason]string{}

	t.Run("pinned payloads", func(t *testing.T) {
		for name, reason := range hostileParseCases(t) {
			t.Run(name, func(t *testing.T) {
				// Fresh built-in table per case: a payload must be judged
				// against a known table, not against whatever a previous
				// subtest left installed.
				isolate(t)

				raw, err := os.ReadFile(filepath.Join("testdata", "hostile", name))
				if err != nil {
					t.Fatalf("read payload: %v", err)
				}

				before := snapshotTable()
				_, _, err = parseBundle(raw)
				assertRejected(t, err, reason, before)
			})
			covered[reason] = name
		}
	})

	t.Run("gate-stage mutations", func(t *testing.T) {
		for name, c := range hostileGateCases() {
			t.Run(name, func(t *testing.T) {
				isolate(t)

				// The mutation may install a table of its own (the rollback
				// case has to), so snapshot AFTER it and before the check.
				candidate := c.mutate(t, liveTable(t))
				before := snapshotTable()

				now := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
				assertRejected(t, checkPlausible(candidate, now), c.reason, before)
			})
			covered[c.reason] = name
		}
	})

	// Completeness. Every Reason the package can produce must have at
	// least one payload aimed at it.
	for _, r := range allReasons {
		if _, ok := covered[r]; !ok {
			t.Errorf("reason %q has no payload in the corpus: add one to testdata/hostile/ "+
				"(or to hostileGateCases if it needs a table to compare against)", r)
		}
	}
}

// hostileParseCases reads the manifest, which is the single source of
// truth for what the corpus claims. Reading it rather than hardcoding the
// list means a payload added to the directory without a manifest entry is
// caught, and vice versa.
func hostileParseCases(t *testing.T) map[string]Reason {
	t.Helper()
	dir := filepath.Join("testdata", "hostile")

	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m hostileManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if len(m.Cases) == 0 {
		t.Fatal("the manifest declares no cases")
	}

	cases := make(map[string]Reason, len(m.Cases))
	for _, c := range m.Cases {
		if c.Note == "" {
			t.Errorf("%s has no note: a payload nobody can explain is a payload nobody will maintain", c.File)
		}
		cases[c.File] = c.Reason
	}

	// The manifest and the directory must agree, in both directions. A
	// payload sitting in testdata that no test runs is worse than no
	// payload at all: it reads as coverage that does not exist.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read corpus dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "manifest.json" {
			continue
		}
		if _, ok := cases[e.Name()]; !ok {
			t.Errorf("%s is in testdata/hostile/ but not in manifest.json, so nothing runs it", e.Name())
		}
	}
	for file := range cases {
		if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
			t.Errorf("manifest declares %s but the file is missing: %v", file, err)
		}
	}
	return cases
}

// gateCase is a mutation of the real dataset, which is how the
// comparative gates have to be tested: they judge a bundle against the
// table in force, so there is nothing to compare a standalone file to.
type gateCase struct {
	reason Reason
	mutate func(t *testing.T, base pricing.ExternalTable) pricing.ExternalTable
}

func hostileGateCases() map[string]gateCase {
	return map[string]gateCase{
		// A clock wrong on either side, or a bad record. Accepting it
		// would also poison the anti-rollback check from then on, since
		// nothing older than the bogus date could ever be installed.
		"data dated next year": {
			reason: ReasonFutureDataDate,
			mutate: func(_ *testing.T, base pricing.ExternalTable) pricing.ExternalTable {
				base.DataDate = "2027-01-01"
				return base
			},
		},

		// The one attack a signature cannot stop by itself: a signature
		// stays valid forever, so a genuine old release can be replayed
		// intact. Refusing to move backwards is the whole defence.
		"replay of an older signed release": {
			reason: ReasonRollback,
			mutate: func(t *testing.T, base pricing.ExternalTable) pricing.ExternalTable {
				t.Helper()
				// Install the newer table first, then offer the older one.
				if err := pricing.Install(base); err != nil {
					t.Fatalf("install the newer table: %v", err)
				}
				base.DataDate = "2026-06-01"
				return base
			},
		},

		// A truncated or wrongly-filtered export. Worth refusing on its
		// own terms, and it also denies an attacker the trick of hiding
		// spend by dropping the model it was spent on.
		"export truncated to one model": {
			reason: ReasonModelLoss,
			mutate: func(t *testing.T, base pricing.ExternalTable) pricing.ExternalTable {
				t.Helper()
				if len(base.Models) < 4 {
					t.Fatalf("fixture has only %d models, too few to test truncation", len(base.Models))
				}
				base.Models = base.Models[:1]
				return base
			},
		},

		// A hundredfold move on a model we already price. Prices do
		// change, but not like this, and not unattended.
		"hundredfold rate move": {
			reason: ReasonRateJump,
			mutate: func(t *testing.T, base pricing.ExternalTable) pricing.ExternalTable {
				t.Helper()
				const target = "claude-opus-4-8"
				models := append([]pricing.ExternalModel{}, base.Models...)
				found := false
				for i, m := range models {
					if m.ID != target {
						continue
					}
					scaled := make([]pricing.ExternalInterval, len(m.Input))
					copy(scaled, m.Input)
					for j := range scaled {
						scaled[j].Price *= 100
					}
					models[i].Input = scaled
					found = true
				}
				if !found {
					t.Fatalf("fixture no longer carries %s; pick another model for this case", target)
				}
				base.Models = models
				return base
			},
		},
	}
}

// liveTable parses the real fixture into a table dated just ahead of the
// active one, so the freshness gates pass and only the mutation under
// test decides the outcome.
func liveTable(t *testing.T) pricing.ExternalTable {
	t.Helper()
	bundle, _ := liveFixture(t)
	base, _, err := parseBundle(bundle)
	if err != nil {
		t.Fatalf("the live fixture must parse: %v", err)
	}
	base.Tag = "v2026.08.01-aaaaaaa"
	base.DataDate = "2026-08-01"
	return base
}

// tableID identifies whatever table is in force, so a test can prove a
// refusal changed nothing.
type tableID struct {
	src           pricing.Source
	tag, dataDate string
}

func snapshotTable() tableID {
	src, tag, dataDate := pricing.ActiveTable()
	return tableID{src: src, tag: tag, dataDate: dataDate}
}

// assertRejected checks a payload was refused, refused as a rejection
// rather than some other failure, refused by the gate we aimed at, and
// that it left pricing untouched.
func assertRejected(t *testing.T, err error, want Reason, before tableID) {
	t.Helper()

	if err == nil {
		t.Fatal("payload was ACCEPTED, which must never happen")
	}
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("error does not wrap ErrRejected, so callers will not treat it as a rejection: %v", err)
	}
	if got := ReasonOf(err); got != want {
		t.Errorf("refused by gate %q, want %q (%v)", got, want, err)
	}
	// The property that matters more than the code: a refused payload
	// must not change what prices a call. Compared against the table that
	// was in force rather than against the built-in one, because the
	// rollback case legitimately starts from a fetched table.
	if after := snapshotTable(); after != before {
		t.Errorf("a refused payload changed the active table: %+v -> %+v", before, after)
	}
}

// TestReasonOfIgnoresOtherErrors keeps the accessor honest: only a real
// rejection has a gate code, so nothing can mistake an offline blip or a
// signature failure for a data problem.
func TestReasonOfIgnoresOtherErrors(t *testing.T) {
	for name, err := range map[string]error{
		"nil":           nil,
		"offline":       ErrOffline,
		"not modified":  ErrNotModified,
		"bad signature": ErrBadSignature,
		"bare sentinel": ErrRejected,
		"unrelated":     errors.New("something else"),
	} {
		t.Run(name, func(t *testing.T) {
			if got := ReasonOf(err); got != ReasonNone {
				t.Errorf("ReasonOf(%v) = %q, want ReasonNone", err, got)
			}
		})
	}
}

// TestRejectionMessageNamesItsGate covers what a user actually sees. The
// code is only useful if it survives into the message and through
// wrapping, which is how it reaches a log line.
func TestRejectionMessageNamesItsGate(t *testing.T) {
	err := reject(ReasonRateOutOfRange, "claude-x input rate 5000 is out of range")

	if got := ReasonOf(err); got != ReasonRateOutOfRange {
		t.Errorf("ReasonOf = %q", got)
	}
	for _, want := range []string{"rate_out_of_range", "5000", "pricing bundle rejected"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q is missing %q", err.Error(), want)
		}
	}

	// Wrapped one level deeper, as the CLI does before printing.
	wrapped := errors.Join(errors.New("refusing the downloaded price table"), err)
	if got := ReasonOf(wrapped); got != ReasonRateOutOfRange {
		t.Errorf("the gate code must survive wrapping, got %q", got)
	}
	if !errors.Is(wrapped, ErrRejected) {
		t.Error("a wrapped rejection must still satisfy errors.Is(err, ErrRejected)")
	}
}
