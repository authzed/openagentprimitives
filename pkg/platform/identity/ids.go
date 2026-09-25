package identity

import "encoding/json"

// Defined identity-id types. Deliberately DEFINED types (not `= string`
// aliases), like Subject in subject.go, so a future representation change
// flags every call site. Each carries a distinct meaning that must not be
// interchanged — passing one where another is expected is a compile error.

// CanonicalUserID is the BARE canonical user id (base64url body, no "user:"
// prefix) used inside SpiceDB object ids. It is what Principal.Canonical()
// returns. The "user:"-prefixed form is Subject (subject.go).
//
// # Why this is a struct and not a string
//
// This id becomes a SpiceDB `user:` object, so whether it may be trusted
// depends ENTIRELY on where its bytes came from — and as a defined string type
// it recorded nothing about that. Twenty-six non-test sites converted a bare
// string straight into one, and several of those strings are exactly the
// inputs a security audit found being trusted without verification: an
// unsigned X-Admin-Subject header, a self-asserted app-tool Requester, an
// email copied out of a directory this deployment does not govern.
//
// Principal already tracked provenance properly — unexported fields, an
// explicit emailVerified, an AllowSynthetic opt-in — and then threw it away at
// this boundary. With an unexported field the only ways in are the
// constructors here, which is the same shape plangate.Handle and
// memory.Approval use, and it makes every act of taking someone's word for an
// identity a named, greppable call rather than a conversion nobody can see.
//
// The zero value is empty and unverified, so a forgotten assignment fails
// closed rather than reading as a valid identity.
type CanonicalUserID struct {
	// s is the id, and it is the ONLY field.
	//
	// Provenance deliberately does NOT live here. Go compares structs
	// field-by-field, so a `verified` or `trustReason` field would make the
	// same person reached by two paths compare unequal — and this type is used
	// as a map key, so that is a silent-miss landmine strictly worse than the
	// problem the type exists to solve. Equality is identity: same person,
	// same value, however each was learned.
	//
	// What the type DOES enforce is that the id cannot be conjured. The field
	// is unexported, so the only ways in are the constructors below, and every
	// one of them names where the bytes came from at the call site. That is
	// the property worth having: twenty-six sites used to convert a bare
	// string with nothing recording the origin, and several of those strings
	// are inputs a security audit found being trusted without verification —
	// an unsigned X-Admin-Subject header, a self-asserted app-tool Requester,
	// an email from a directory this deployment does not govern.
	//
	// Carrying verified-ness INTO the value needs a second type
	// (a VerifiedCanonical a sink can demand), not a field on this one.
	s string
}

func (c CanonicalUserID) String() string { return c.s }

// IsZero reports the absence of an id. The zero value is empty, so a forgotten
// assignment fails closed rather than reading as a valid identity.
func (c CanonicalUserID) IsZero() bool { return c.s == "" }

// MarshalJSON emits the id.
func (c CanonicalUserID) MarshalJSON() ([]byte, error) { return json.Marshal(c.s) }

// UnmarshalJSON decodes an id that arrived over a wire — asserted by whoever
// sent it, and witnessed by nobody here.
func (c *CanonicalUserID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	c.s = s
	return nil
}

// CanonicalFromTrusted accepts an id the platform did not itself verify,
// naming WHY it is being accepted.
//
// because is not stored — it documents the call site and makes these sites
// countable. `grep CanonicalFromTrusted` enumerates every place the platform
// takes someone's word for an identity, and a reason that reads "a header the
// proxy set" or "a field the caller wrote" is a finding, not a migration
// artifact.
func CanonicalFromTrusted(id string, because string) CanonicalUserID {
	_ = because // documentation at the call site; see the doc comment
	return CanonicalUserID{s: id}
}

// canonicalFromVerified is the in-package constructor for an id the platform
// established itself. Unexported: verification happens in this package.
func canonicalFromVerified(id string) CanonicalUserID { return CanonicalUserID{s: id} }

// canonicalUnverified is the in-package constructor for a well-formed id whose
// subject was never proven — an email reference, a synthetic channel id.
func canonicalUnverified(id, because string) CanonicalUserID {
	_ = because
	return CanonicalUserID{s: id}
}

// RawExternalID is a channel-native external id (e.g. a Slack "U…" member id).
// It is NOT a canonical subject and must not be handed to code that expects
// one (that mismatch was a production bug — the whole reason this type exists).
type RawExternalID string

func (r RawExternalID) String() string { return string(r) }

// Email is a (normalized-lowercase) email address.
type Email string

func (e Email) String() string { return string(e) }

// Kind is an identity's origin. An OPEN enum: the constants name the known
// values, but an unknown kind is valid data (channel kinds are pluggable) —
// there is no Validate/panic. The zero value "" is a valid "unset origin".
type Kind string

func (k Kind) String() string { return string(k) }

const (
	KindSlack Kind = "slack"
	KindLocal Kind = "local"
	KindIdP   Kind = "idp"
	KindFake  Kind = "fake"
	KindBento Kind = "bento"
)

// TeamScope is a workspace/team scope (e.g. a Slack team id). Values are
// dynamic (not an enum); typed for uniform id safety.
type TeamScope string

func (t TeamScope) String() string { return string(t) }
