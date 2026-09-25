package authz

import "fmt"

// ObjectID is a value that has already passed through a slot's declared
// transform chain and is therefore legal as a SpiceDB object id.
//
// The field is unexported so an ObjectID cannot be minted from a raw string by
// conversion. That is the entire point of the type. This exact bug — a raw
// value reaching SpiceDB as an object id — recurred three times in one session
// across three subsystems, surviving unit tests, integration, e2e and code
// review every time, because the wrong value COMPILES: an untransformed URL and
// an escaped one are both strings, and the failure surfaces as an
// InvalidArgument from a dependency rather than anything the type system sees.
//
// identity.CanonicalUserID is the counter-example to avoid: it is a defined
// string type, so `identity.CanonicalUserID(rawEmail)` is a conversion that
// encodes nothing and compiles cleanly.
type ObjectID struct{ s string }

// NewObjectID derives an object id from a free-form value and the transform
// chain the slot declared (AgentClass.status.resolvedSlots[].valueTransforms).
//
// This is the ONLY way to turn user- or agent-supplied text into an ObjectID.
func NewObjectID(raw string, transforms []string) (ObjectID, error) {
	id, err := ApplyTransforms(raw, transforms)
	if err != nil {
		return ObjectID{}, fmt.Errorf("authz: derive object id from %q: %w", raw, err)
	}
	if id == "" {
		return ObjectID{}, fmt.Errorf("authz: %q produced an empty object id", raw)
	}
	return ObjectID{s: id}, nil
}

// TrustedObjectID wraps a value that is ALREADY a canonical object id because
// it was read back out of SpiceDB, not derived from input.
//
// Deliberately verbose and greppable: every call site is a place where the
// canonicalization guarantee rests on provenance rather than on the transform
// chain, so `grep TrustedObjectID` must list them all for review.
func TrustedObjectID(s string) ObjectID { return ObjectID{s: s} }

// String renders the id for a SpiceDB call, a log, or an error.
func (o ObjectID) String() string { return o.s }

// IsZero reports whether this is the unset zero value.
func (o ObjectID) IsZero() bool { return o.s == "" }
