package refresh

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdata holds a REAL bundle and signature fetched from the live
// service, so these tests prove the verifier against production output
// rather than against something this package generated itself.
func liveFixture(t *testing.T) (bundle, sig []byte) {
	t.Helper()
	const dir = "testdata"
	b, err := os.ReadFile(filepath.Join(dir, "anthropic.json"))
	if err != nil {
		t.Fatalf("read bundle fixture: %v", err)
	}
	s, err := os.ReadFile(filepath.Join(dir, "anthropic.json.minisig"))
	if err != nil {
		t.Fatalf("read signature fixture: %v", err)
	}
	return b, s
}

// TestVerifyLiveSignature is the headline test: the production signature
// over the production bundle verifies against the compiled-in key.
func TestVerifyLiveSignature(t *testing.T) {
	bundle, sig := liveFixture(t)

	v, err := verifySignature(bundle, sig, TrustedKeys)
	if err != nil {
		t.Fatalf("live signature must verify: %v", err)
	}
	if v.KeyName != "ai-price-index-2026-07" {
		t.Errorf("KeyName = %q", v.KeyName)
	}
	// The trusted comment is where we record the release, and it is only
	// believable because the second signature covers it.
	if !strings.Contains(v.TrustedComment, "ai-price-index v2026.") {
		t.Errorf("TrustedComment = %q, want it to name the dataset release", v.TrustedComment)
	}
	if !strings.Contains(v.TrustedComment, "dataModified=") {
		t.Errorf("TrustedComment = %q, want a dataModified stamp", v.TrustedComment)
	}
}

// TestVerifyRejectsTamperedBundle is the property that matters: any
// change to the content, however small, must fail.
func TestVerifyRejectsTamperedBundle(t *testing.T) {
	bundle, sig := liveFixture(t)

	cases := map[string][]byte{
		"one byte appended":  append(append([]byte{}, bundle...), ' '),
		"one byte truncated": bundle[:len(bundle)-1],
		"empty":              {},
	}
	// Flip a digit inside a price, the most valuable thing to tamper with.
	flipped := append([]byte{}, bundle...)
	if i := strings.Index(string(flipped), `"price_usd": 5`); i >= 0 {
		flipped[i+len(`"price_usd": `)] = '1'
		cases["price digit changed"] = flipped
	}

	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := verifySignature(msg, sig, TrustedKeys); err == nil {
				t.Fatal("tampered bundle verified, which must never happen")
			} else if !errors.Is(err, ErrBadSignature) {
				t.Errorf("error should wrap ErrBadSignature, got %v", err)
			}
		})
	}
}

// TestVerifyRejectsSwappedTrustedComment covers the check that is easy to
// omit. The trusted comment records which release the bundle is, so an
// attacker who could rewrite it while keeping a valid content signature
// could make an old bundle claim to be a new one.
func TestVerifyRejectsSwappedTrustedComment(t *testing.T) {
	bundle, sig := liveFixture(t)

	lines := strings.Split(strings.TrimRight(string(sig), "\n"), "\n")
	if len(lines) < 4 {
		t.Fatalf("unexpected fixture shape: %d lines", len(lines))
	}
	lines[2] = "trusted comment: ai-price-index v2099.01.01-deadbee dataModified=2099-01-01"
	forged := []byte(strings.Join(lines, "\n") + "\n")

	if _, err := verifySignature(bundle, forged, TrustedKeys); err == nil {
		t.Fatal("a swapped trusted comment verified; the global signature is not being checked")
	} else if !errors.Is(err, ErrBadSignature) {
		t.Errorf("error should wrap ErrBadSignature, got %v", err)
	}
}

// TestVerifyRejectsUntrustedKey proves an attacker cannot simply sign
// with their own key: the key id must match one we compiled in.
func TestVerifyRejectsUntrustedKey(t *testing.T) {
	bundle, sig := liveFixture(t)

	other := []TrustedKey{{
		Name:  "someone-else",
		KeyID: "0000000000000000",
		// A structurally valid but different minisign public key.
		PublicKey: "RWQf6LRCGA9i53mlYecO4IzT51TGPpvWucNSCh1CBM0QTaLn73Y7GFO3",
	}}

	if _, err := verifySignature(bundle, sig, other); err == nil {
		t.Fatal("signature verified against an untrusted key set")
	} else if !errors.Is(err, ErrBadSignature) {
		t.Errorf("error should wrap ErrBadSignature, got %v", err)
	}
}

// TestVerifyRejectsMalformedSignatureFiles keeps the parser from
// panicking or silently accepting garbage, which is what a captive
// portal or a truncated download actually looks like.
func TestVerifyRejectsMalformedSignatureFiles(t *testing.T) {
	bundle, sig := liveFixture(t)
	good := strings.Split(strings.TrimRight(string(sig), "\n"), "\n")

	cases := map[string]string{
		"empty":                  "",
		"html error page":        "<html><body>404 Not Found</body></html>",
		"two lines":              good[0] + "\n" + good[1],
		"signature not base64":   good[0] + "\nnot base64 at all!\n" + good[2] + "\n" + good[3],
		"missing comment line":   good[0] + "\n" + good[1] + "\nnope\n" + good[3],
		"global sig not base64":  good[0] + "\n" + good[1] + "\n" + good[2] + "\n!!!!",
		"signature wrong length": good[0] + "\nRUR=\n" + good[2] + "\n" + good[3],
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := verifySignature(bundle, []byte(body), TrustedKeys); err == nil {
				t.Fatal("malformed signature file was accepted")
			} else if !errors.Is(err, ErrBadSignature) {
				t.Errorf("error should wrap ErrBadSignature, got %v", err)
			}
		})
	}
}

// TestVerifyToleratesCRLF covers a signature that travelled through a
// system that rewrote line endings.
func TestVerifyToleratesCRLF(t *testing.T) {
	bundle, sig := liveFixture(t)
	crlf := []byte(strings.ReplaceAll(string(sig), "\n", "\r\n"))

	if _, err := verifySignature(bundle, crlf, TrustedKeys); err != nil {
		t.Errorf("CRLF signature should still verify: %v", err)
	}
}
