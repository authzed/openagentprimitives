// Package identity is the single source of truth for the canonical user
// ID used across SpiceDB writes and CheckPermission calls. Construct a
// Principal via the constructors in principal.go; call .Canonical() to
// obtain the stable base64-encoded user id.
package identity

import (
	"encoding/base64"
	"strings"
)

// DecodeForDisplay reverses Principal.Canonical for human-readable rendering:
// given the canonical id (or "user:<canonical>" SpiceDB-form), it returns
// the original email (when the canonical was produced from an email) or the
// "kind:teamScope:externalID" synthetic. Decoding failures fall back to
// the input unchanged — never mangles a display string.
//
// Only use for display. Identity comparisons, SpiceDB writes, and
// credential resolution MUST keep the canonical form.
func DecodeForDisplay(subject string) string {
	in := strings.TrimPrefix(subject, "user:")
	if in == "" {
		return subject
	}
	raw, err := base64.RawURLEncoding.DecodeString(in)
	if err != nil {
		return subject
	}
	s := string(raw)
	// Reject non-printable garbage — if the decode "succeeded" but
	// produced unprintable bytes, the input wasn't a canonical id.
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return subject
		}
	}
	return s
}

// IsCanonicalUserIDShape reports whether s has the shape every
// CanonicalUserID has — non-empty and a valid base64url
// (RawURLEncoding, unpadded) string — which is what Principal.Canonical
// produces for both the email and the synthetic form. It is a SHAPE
// check, not proof that a principal exists: it exists so a hand-authored
// config field cannot carry a raw email or external id where a canonical
// belongs, the "raw id as a SpiceDB object id" class of defect. It does
// not inspect the decoded bytes (a canonical may encode anything).
func IsCanonicalUserIDShape(s string) bool {
	if s == "" {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil
}
