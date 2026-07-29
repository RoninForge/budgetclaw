// Package refresh holds the trust anchor for pricing data that
// budgetclaw did not compile in.
//
// The pricing table shipped inside a release is trusted because it was
// vendored from a reviewed git tag and sha256-verified against
// PROVENANCE.json at build time. Any pricing data obtained at runtime
// has to earn the same standing, because the numbers ARE the product: a
// wrong rate misreports spend, and a poisoned zero rate would silently
// disarm a user's budget cap.
//
// The anchor is a list of minisign-format ed25519 public keys, compiled
// into the binary. Signatures are produced in CI on
// RoninForge/ai-price-index and never on the machine that serves the
// data.
//
// Why the key is held in GitHub Actions and never on the VPS: the VPS
// terminates TLS with a valid certificate, so TLS proves the bytes came
// from that host, not that they are genuine. If the host that serves the
// data could also sign it, a compromise of that one box would produce
// perfectly valid signatures and signing would buy nothing. Holding the
// key in the same place that already builds and releases the binary
// means forging price data requires the same compromise that would
// already yield a malicious budgetclaw. The VPS becomes a dumb mirror
// and drops out of the trust chain.
//
// What this does not cover, stated plainly: a wrong-but-genuine upstream
// price merge (that is what plausibility gates are for), a compromise of
// the GitHub org (which already implies a malicious binary, so the
// signature adds nothing), and a local attacker who can rewrite the
// binary itself.
package refresh

// TrustedKey is one minisign public key budgetclaw will accept
// signatures from.
type TrustedKey struct {
	// Name identifies the key in diagnostics and logs.
	Name string

	// KeyID is the big-endian hex id minisign prints in its comments,
	// used to match a signature to a key without trial verification.
	KeyID string

	// PublicKey is the base64 body of a minisign .pub file: the second
	// line, without the untrusted comment. It decodes to 42 bytes,
	// namely the two-byte algorithm tag "Ed", the eight-byte key id in
	// little-endian, and the 32-byte ed25519 public key.
	PublicKey string
}

// TrustedKeys is every key whose signatures this build accepts.
//
// It is a list rather than a single key so rotation never requires a
// flag day. The sequence is: ship a release trusting both the old and
// the new key, flip the CI secret to the new key, then drop the old key
// in a later release.
//
// Compromise response is the same first two steps done immediately. A
// binary that trusts only a retired key will reject the new signatures
// and keep using its compiled-in pricing table, which is stale but
// honest, and the staleness surfaces through the unpriced-event
// reporting rather than silently producing wrong numbers. The failure
// mode is fail-safe by construction.
//
// The same public key is published in the ai-price-index README so
// anyone can verify a release independently with the standard minisign
// tool. Trust material is only ever compiled in; budgetclaw never
// fetches a key.
var TrustedKeys = []TrustedKey{
	{
		Name:      "ai-price-index-2026-07",
		KeyID:     "347A0943DE5B7C45",
		PublicKey: "RWRFfFveQwl6NGYtfQNtpgdecGGC3U8k5iqK+vGmUq3D3SP0wIfmg8P1",
	},
}
