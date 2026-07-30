package refresh

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// Signature verification for minisign-format detached signatures.
//
// Written out rather than pulled in as a dependency because it is short
// enough to read in one sitting, and because this is the code that
// decides whether a downloaded file is allowed to set the dollar figures
// a user trusts. It should be auditable without following an import.
//
// A .minisig file is four lines:
//
//	untrusted comment: <free text, NOT covered by any signature>
//	base64( algorithm[2] || key_id[8] || signature[64] )
//	trusted comment: <free text, covered by the global signature>
//	base64( global_signature[64] )
//
// The second signature is the part that is easy to miss and matters
// here: it covers signature || trusted_comment, so without checking it
// an attacker could keep a valid signature and swap the trusted comment,
// which is where we record the dataset tag and data date. minisign's own
// "comment signature verified" line refers to this check.

// ErrBadSignature reports that a bundle did not verify against any
// trusted key. It is deliberately opaque: a caller should treat every
// verification failure identically, and never branch on why.
var ErrBadSignature = errors.New("signature verification failed")

// algorithm tags. "ED" prehashes the message with blake2b-512 before
// signing, which is what current minisign produces. "Ed" signs the raw
// message and is accepted for completeness.
const (
	algPrehashed = "ED"
	algLegacy    = "Ed"
)

// verified is the trustworthy content of a signature file.
type verified struct {
	// KeyName is the trusted key that validated the bundle.
	KeyName string
	// TrustedComment is signature-covered text. We put the dataset tag
	// and data date here, so it can be believed after verification.
	TrustedComment string
}

// verifySignature checks message against a minisign signature file, and
// returns which trusted key accepted it.
//
// Every failure returns ErrBadSignature wrapped with a reason for logs.
// Callers must not treat any of them differently: a bundle either
// verifies or is discarded.
func verifySignature(message, sigFile []byte, keys []TrustedKey) (verified, error) {
	sigAlg, keyID, sig, trustedComment, globalSig, err := parseSignature(sigFile)
	if err != nil {
		return verified{}, err
	}

	// The message digest depends on the algorithm the signer used.
	var signed []byte
	switch sigAlg {
	case algPrehashed:
		h := blake2b.Sum512(message)
		signed = h[:]
	case algLegacy:
		signed = message
	default:
		return verified{}, fmt.Errorf("%w: unsupported algorithm %q", ErrBadSignature, sigAlg)
	}

	for _, k := range keys {
		pub, kID, err := decodePublicKey(k.PublicKey)
		if err != nil {
			// A malformed compiled-in key is a build mistake, not an
			// attack. Skip it rather than rejecting a good bundle; the
			// keys_test guards against this reaching a release.
			continue
		}
		if kID != keyID {
			continue // signed by a different key; not necessarily hostile
		}
		if !ed25519.Verify(pub, signed, sig) {
			return verified{}, fmt.Errorf("%w: bad signature for key %s", ErrBadSignature, k.KeyID)
		}
		// Second signature: covers signature || trusted_comment, so the
		// comment cannot be altered while keeping the first signature.
		if !ed25519.Verify(pub, append(append([]byte{}, sig...), []byte(trustedComment)...), globalSig) {
			return verified{}, fmt.Errorf("%w: trusted comment is not authentic", ErrBadSignature)
		}
		return verified{KeyName: k.Name, TrustedComment: trustedComment}, nil
	}

	return verified{}, fmt.Errorf("%w: no trusted key with id %x", ErrBadSignature, keyID)
}

// parseSignature splits a .minisig file into its parts.
func parseSignature(sigFile []byte) (alg string, keyID [8]byte, sig []byte, trustedComment string, globalSig []byte, err error) {
	// Tolerate CRLF and a trailing newline; reject anything else odd.
	lines := strings.Split(strings.ReplaceAll(strings.TrimRight(string(sigFile), "\r\n"), "\r\n", "\n"), "\n")
	if len(lines) < 4 {
		return "", keyID, nil, "", nil, fmt.Errorf("%w: expected 4 lines, got %d", ErrBadSignature, len(lines))
	}

	const tcPrefix = "trusted comment:"
	if !strings.HasPrefix(lines[2], tcPrefix) {
		return "", keyID, nil, "", nil, fmt.Errorf("%w: missing trusted comment line", ErrBadSignature)
	}
	// minisign signs the comment exactly as it appears after the prefix
	// and a single space.
	trustedComment = strings.TrimPrefix(strings.TrimPrefix(lines[2], tcPrefix), " ")

	raw, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[1]))
	if derr != nil {
		return "", keyID, nil, "", nil, fmt.Errorf("%w: signature line is not base64", ErrBadSignature)
	}
	if len(raw) != 2+8+64 {
		return "", keyID, nil, "", nil, fmt.Errorf("%w: signature is %d bytes, want 74", ErrBadSignature, len(raw))
	}

	globalSig, derr = base64.StdEncoding.DecodeString(strings.TrimSpace(lines[3]))
	if derr != nil {
		return "", keyID, nil, "", nil, fmt.Errorf("%w: global signature is not base64", ErrBadSignature)
	}
	if len(globalSig) != 64 {
		return "", keyID, nil, "", nil, fmt.Errorf("%w: global signature is %d bytes, want 64", ErrBadSignature, len(globalSig))
	}

	alg = string(raw[:2])
	copy(keyID[:], raw[2:10])
	sig = raw[10:]
	return alg, keyID, sig, trustedComment, globalSig, nil
}

// decodePublicKey unpacks a minisign public key body into an ed25519 key
// and its key id.
func decodePublicKey(b64 string) (ed25519.PublicKey, [8]byte, error) {
	var keyID [8]byte
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, keyID, fmt.Errorf("public key is not base64: %w", err)
	}
	if len(raw) != 2+8+ed25519.PublicKeySize {
		return nil, keyID, fmt.Errorf("public key is %d bytes, want 42", len(raw))
	}
	if got := string(raw[:2]); got != algLegacy {
		// Public keys carry the "Ed" tag regardless of which signing
		// variant is used.
		return nil, keyID, fmt.Errorf("public key algorithm is %q, want %q", got, algLegacy)
	}
	copy(keyID[:], raw[2:10])
	return ed25519.PublicKey(raw[10:]), keyID, nil
}
