package identity

import (
	"errors"
	"strings"
)

// userObjectType/subjectUserPrefix centralize the "user:" SpiceDB object-type
// spelling. This file is the ONLY place the "user:" string literal may be
// written; every other package must go through Subject.CanonicalUserID() or
// CanonicalUserID.Subject() instead of hand-rolling the prefix.
const (
	userObjectType    = "user"
	subjectUserPrefix = userObjectType + ":"
)

// ErrNonUserSubject is returned by Subject.CanonicalUserID when the subject's
// SpiceDB object-type is not "user" (e.g. a group or service-account subject),
// so a caller that needs a bare user canonical fails closed instead of silently
// misreading a non-user subject.
var ErrNonUserSubject = errors.New("identity: subject is not a user subject")

// Subject is a canonical SpiceDB subject reference, "user:<canonical>".
//
// It is a DEFINED type (deliberately NOT a `= string` alias): it reads and
// JSON-marshals exactly like a string today, but gives a compiler-enforced
// seam so the underlying representation can change later (e.g. to a struct)
// with every call site flagged. Empty means "no subject" (an agent/runner-
// authored turn, or a non-human inbound).
type Subject string

// String returns the raw subject string.
func (s Subject) String() string { return string(s) }

// Empty reports whether the subject is unset.
func (s Subject) Empty() bool { return s == "" }

// ObjectType returns the SpiceDB object-type segment (before the first ':'),
// or "" if the subject has no type prefix.
func (s Subject) ObjectType() string {
	str := string(s)
	if i := strings.IndexByte(str, ':'); i >= 0 {
		return str[:i]
	}
	return ""
}

// CanonicalUserID returns the bare canonical user id iff this is a user
// subject; otherwise ErrNonUserSubject (fail-closed). This is the typed
// replacement for strings.TrimPrefix(subject, "user:").
func (s Subject) CanonicalUserID() (CanonicalUserID, error) {
	if s.ObjectType() != userObjectType {
		return CanonicalUserID{}, ErrNonUserSubject
	}
	// A Subject is a reference the caller already held; parsing it out does
	// not prove who it names, so the canonical stays unverified.
	return canonicalUnverified(strings.TrimPrefix(string(s), subjectUserPrefix),
		"parsed from an existing user: subject reference"), nil
}

// Subject returns the full "user:<canonical>" SpiceDB subject reference for c.
// It is the single typed CanonicalUserID→Subject conversion; call it instead of
// hand-building `"user:"+canon.String()`, which yields a string that merely
// looks like a Subject.
func (c CanonicalUserID) Subject() Subject { return Subject(subjectUserPrefix + c.String()) }

// SubjectRef returns the SpiceDB subject reference for c when c is whatever
// principal happened to act, which is not always a human.
//
// Use this rather than Subject wherever the canonical came from an inbound. A
// kind that provides no starting user (a cron firing) assigns the Channel's
// spec.authzSubject verbatim and RawSubject.Canonical() preserves a qualified
// value unchanged, so a CanonicalUserID can already BE a full reference like
// "service:nightly-report". Subject would double-prefix it into
// "user:service:nightly-report" — a subject naming nothing, in an audit line
// someone is meant to look up.
//
// The discriminator is sound, not heuristic: a bare canonical is base64 (of an
// email, or of the synthetic kind:teamScope:externalID tuple) and the base64
// alphabet contains no ':', so a colon can only be a type prefix.
//
// Empty stays empty: no principal to name is not the same as "user:".
func (c CanonicalUserID) SubjectRef() Subject {
	if c.IsZero() {
		return ""
	}
	if s := Subject(c.String()); s.ObjectType() != "" {
		return s
	}
	return c.Subject()
}
