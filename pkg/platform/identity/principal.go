package identity

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// ErrSyntheticSubject is returned by Canonical/Subject when a principal has
// no verified email and did NOT explicitly opt into the synthetic
// (unforgeable kind:teamScope:externalID) encoding via AllowSynthetic.
//
// Silently minting a synthetic subject for a principal that SHOULD have
// resolved to a real user:<email> writes/checks a subject that no real grant
// resolves to — the class of bug that turned owner approvals into denials and
// denied owners the right to restart their own sessions. Fail loud: the caller
// must either supply a verified email or explicitly say "yes, synthetic is
// intended here" via AllowSynthetic.
var ErrSyntheticSubject = errors.New("identity: refusing to mint a synthetic subject for a principal with no verified email (call AllowSynthetic to opt in)")

// Principal is the single representation of a human user anywhere in
// the system. Construct via the constructors below — never as a
// struct literal — so the email-vs-synthetic decision and the "was
// this identity proven?" judgment live in exactly one package. Fields
// are unexported; read them through the typed accessors below.
type Principal struct {
	email         Email // normalized lowercase; empty → synthetic canonical (only with AllowSynthetic)
	emailVerified bool  // true only when an IdP or trusted channel proved it
	kind          Kind  // "slack", "local", "idp", ...
	teamScope     TeamScope
	externalID    RawExternalID
	displayName   string

	raw string // RawSubject passthrough; bypasses encoding entirely

	// allowSynthetic gates the email-less synthetic encoding. Set only via
	// AllowSynthetic — an explicit opt-in for principals that legitimately
	// key on the unforgeable kind:teamScope:externalID subject (guests,
	// foreign-workspace users, bots, the local CLI user, channel-native ids
	// with no email). Without it, Canonical/Subject error rather than
	// silently minting a synthetic subject.
	allowSynthetic bool
}

// AllowSynthetic returns a copy of the principal explicitly permitted to
// canonicalize to the synthetic kind:teamScope:externalID subject when it has
// no verified email. Use ONLY where the synthetic subject is the intended
// identity (guests / foreign-workspace users / bots / local CLI user /
// channel-native ids without email). For a principal that carries an email
// this is a no-op (the email always wins the canonical).
func (p Principal) AllowSynthetic() Principal {
	p.allowSynthetic = true
	return p
}

// Email returns the principal's normalized-lowercase email, or "" if unset.
func (p Principal) Email() Email { return p.email }

// EmailVerified reports whether the email was PROVEN — an IdP login or a
// trusted-workspace channel lookup — rather than merely referenced.
func (p Principal) EmailVerified() bool { return p.emailVerified }

// Kind returns the principal's origin ("slack", "local", "idp", ...).
func (p Principal) Kind() Kind { return p.kind }

// TeamScope returns the principal's workspace/team scope.
func (p Principal) TeamScope() TeamScope { return p.teamScope }

// ExternalID returns the principal's channel-native external id.
func (p Principal) ExternalID() RawExternalID { return p.externalID }

// DisplayName returns the principal's human-readable display name.
func (p Principal) DisplayName() string { return p.displayName }

// VerifiedEmail is the Principal for an identity whose email was
// PROVEN — an IdP login or a trusted-workspace channel lookup.
func VerifiedEmail(email Email, displayName string) Principal {
	return Principal{email: Email(strings.ToLower(string(email))), emailVerified: true, displayName: displayName}
}

// EmailReference encodes an email that is configuration/reference data
// (authz CEL expressions, bootstrap relationships, display tooling) —
// NOT a proven login. Same canonical as VerifiedEmail, EmailVerified
// stays false.
func EmailReference(email Email) Principal {
	return Principal{email: Email(strings.ToLower(string(email)))}
}

// IdPUser is the Principal an identity-provider Complete() produces.
// verified mirrors the provider's email_verified claim verbatim;
// identityd enforces it before trusting the login.
func IdPUser(email Email, verified bool, displayName string) Principal {
	return Principal{email: Email(strings.ToLower(string(email))), emailVerified: verified, displayName: displayName, kind: KindIdP}
}

// FromExternal is the Principal for a channel-attributed user. email,
// when non-empty, MUST have been verified by the channel (e.g. Slack
// users.info inside the installed workspace) — it wins the canonical
// and marks the principal verified. Empty email yields the stable
// synthetic kind:teamScope:externalID canonical.
func FromExternal(kind Kind, teamScope TeamScope, externalID RawExternalID, email Email) Principal {
	return Principal{
		email:         Email(strings.ToLower(string(email))),
		emailVerified: email != "",
		kind:          kind,
		teamScope:     teamScope,
		externalID:    externalID,
	}
}

// RawSubject wraps a pre-formed SpiceDB subject ("user:<id>") that is
// used verbatim — the bento channel's spec.authzSubject escape hatch.
// Registered here so even the bypass is visible in the one location.
func RawSubject(s string) Principal { return Principal{raw: s} }

// Canonical returns the canonical user id used in SpiceDB object ids.
// Encoding (pinned by canonical_golden_test.go):
// base64url(lower(email)) when Email is set, else — only when the caller
// opted in via AllowSynthetic — base64url(kind:teamScope:externalID).
//
// An email-less principal that did NOT opt into the synthetic encoding
// returns ErrSyntheticSubject: minting a synthetic subject for an identity
// that should have carried a verified email silently writes/checks a subject
// no real grant resolves to. Callers must handle the error (surface it, fail
// closed) rather than proceed with a phantom subject.
func (p Principal) Canonical() (CanonicalUserID, error) {
	// RawSubject bypasses encoding entirely, so the caller supplied the id and
	// the platform proved nothing about it.
	if p.raw != "" {
		return canonicalUnverified(strings.TrimPrefix(p.raw, "user:"), "identity.RawSubject passthrough"), nil
	}
	enc := base64.RawURLEncoding
	if p.email != "" {
		id := enc.EncodeToString([]byte(strings.ToLower(string(p.email))))
		// The email is the identity, so whether this canonical is verified is
		// exactly whether the email was proven. This is the distinction the
		// string type erased: an IdP login and an unproven email reference
		// produced indistinguishable ids.
		if p.emailVerified {
			return canonicalFromVerified(id), nil
		}
		return canonicalUnverified(id, "email reference, never proven by an IdP or a trusted channel"), nil
	}
	if !p.allowSynthetic {
		return CanonicalUserID{}, fmt.Errorf("%w (kind=%q teamScope=%q externalID=%q)",
			ErrSyntheticSubject, p.kind, p.teamScope, p.externalID)
	}
	// A synthetic id keys on the channel-native triple, which the CHANNEL
	// asserted. Unforgeable within that channel, but not a proof of who the
	// person is.
	return canonicalUnverified(
		enc.EncodeToString([]byte(string(p.kind)+":"+string(p.teamScope)+":"+string(p.externalID))),
		"synthetic channel-native id (no verified email)"), nil
}

// Subject returns the full SpiceDB subject reference, "user:<canonical>".
// Propagates Canonical's ErrSyntheticSubject for an un-opted-in email-less
// principal.
func (p Principal) Subject() (Subject, error) {
	if p.raw != "" {
		return Subject(p.raw), nil
	}
	c, err := p.Canonical()
	if err != nil {
		return "", err
	}
	return c.Subject(), nil
}
