package refresh

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// A malformed trust anchor would not fail at build time, only later when
// a real signature arrives and cannot be checked. These tests make a bad
// key a compile-and-test-time failure instead.

// TestTrustedKeysAreWellFormed decodes every key and checks the minisign
// public-key layout: 42 bytes, "Ed" algorithm tag, 8-byte little-endian
// key id, 32-byte ed25519 public key.
func TestTrustedKeysAreWellFormed(t *testing.T) {
	if len(TrustedKeys) == 0 {
		t.Fatal("no trusted keys compiled in: signed pricing data could never be verified")
	}

	for _, k := range TrustedKeys {
		t.Run(k.Name, func(t *testing.T) {
			if k.Name == "" {
				t.Error("key has no name")
			}

			raw, err := base64.StdEncoding.DecodeString(k.PublicKey)
			if err != nil {
				t.Fatalf("public key is not valid base64: %v", err)
			}
			if len(raw) != 42 {
				t.Fatalf("decoded key is %d bytes, want 42", len(raw))
			}
			if got := string(raw[:2]); got != "Ed" {
				t.Errorf("algorithm tag = %q, want \"Ed\" (ed25519)", got)
			}

			// minisign stores the key id little-endian; its comments
			// print it big-endian. Reverse before comparing so the
			// documented KeyID matches what a user sees from
			// `minisign -V`.
			le := raw[2:10]
			be := make([]byte, len(le))
			for i, b := range le {
				be[len(le)-1-i] = b
			}
			if got := strings.ToUpper(hex.EncodeToString(be)); got != k.KeyID {
				t.Errorf("KeyID = %s, but the key body encodes %s", k.KeyID, got)
			}
		})
	}
}

// TestTrustedKeysAreUnique guards against a rotation that pastes the
// same key twice, which would look like two trusted keys while actually
// trusting one.
func TestTrustedKeysAreUnique(t *testing.T) {
	seenID := map[string]string{}
	seenKey := map[string]string{}
	for _, k := range TrustedKeys {
		if prev, dup := seenID[k.KeyID]; dup {
			t.Errorf("key id %s appears on both %q and %q", k.KeyID, prev, k.Name)
		}
		seenID[k.KeyID] = k.Name

		if prev, dup := seenKey[k.PublicKey]; dup {
			t.Errorf("identical public key on both %q and %q", prev, k.Name)
		}
		seenKey[k.PublicKey] = k.Name
	}
}

// TestNoSecretKeyMaterial is a blunt check that nothing resembling a
// minisign SECRET key was ever pasted into this file. A minisign secret
// key file begins with the untrusted comment line and its body decodes
// far longer than a public key; the cheap, reliable signal is that a
// public key entry must be exactly the 56-character base64 of 42 bytes.
func TestNoSecretKeyMaterial(t *testing.T) {
	for _, k := range TrustedKeys {
		if len(k.PublicKey) != 56 {
			t.Errorf("%s: public key field is %d characters, want 56. "+
				"Anything longer suggests secret key material was pasted here",
				k.Name, len(k.PublicKey))
		}
	}
}
