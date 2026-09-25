// Package keyid derives the content address of an Ed25519 public key: the
// stable identifier a signature carries so a verifier can look the key up, and
// the invariant that makes a key binding unforgeable — a registry refuses to
// bind an ID to a key that does not hash to it.
//
// It is a leaf package on purpose: naming a key must not require the ability
// to sign with one. Living next to the audit-log Signer in
// pkg/memory/provenance would drag that capability into the import graph of
// every subsystem that merely needs to name a key, and a browser-facing
// process that can sign can forge a session's audit chain — which
// pkg/web/webui/chat's TestChatPackageNeverImportsProvenance guards against.
// provenance.KeyID and provenance.DecodePubKey delegate here, so there is
// exactly one definition of what a key ID is.
package keyid

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// For returns the identifier for a public key: hex of the first 16 bytes of its
// SHA-256. Callers that persist a (publisher, keyID) binding MUST validate it
// against this function — that check is what stops a tampered registry entry
// from pointing a trusted key ID at an attacker's key.
func For(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:16])
}

// DecodePubKey decodes a standard-base64 Ed25519 public key, validating it is
// exactly ed25519.PublicKeySize bytes. The size check is load-bearing:
// ed25519.Verify panics on a wrong-sized key, and these keys arrive from
// ConfigMaps and CR status.
func DecodePubKey(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("pubkey not valid base64: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("pubkey is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}
