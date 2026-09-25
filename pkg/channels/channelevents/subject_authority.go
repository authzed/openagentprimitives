package channelevents

import (
	"errors"
	"fmt"
)

// The NATS SUBJECT is the routing authority for a session envelope;
// Envelope.Session is not. A publisher's per-session JWT permits publishing
// under exactly one "ap.session.<ns>.<own-name>.>" tree — covering ".in." as
// well as ".out." — while every consumer of a cluster-wide
// "ap.session.*.*.<segment>.<kind>" subscription receives whatever
// Envelope.Session the publisher chose to write into its JSON. Routing off the
// envelope alone therefore makes the per-session grant non-load-bearing: a
// publisher authorized on session A's subject could resolve session B's parked
// approval, clear B's PendingInteractions entry, deliver a message into B, or
// drive B's resurface.
//
// AuthorizeInSubject / AuthorizeOutSubject are that cross-check, factored here
// — beside the Envelope and the ParseIn/OutSubject parsers they build on — so
// every consumer runs the SAME gate rather than each re-deriving it. The e2e
// harness stands in for channelsd in every scenario, so a check that lives
// only in internal/cmd/channelsd is a check no e2e run exercises.
//
// Legitimate publishers always agree: PublishIn / PublishOut and every manual
// publish site build subject and envelope from one (ns, name) pair, so a
// mismatch is a bug or an attack, never normal traffic.

var (
	// ErrUnroutableSubject reports a subject that does not parse as a session
	// subject of the expected direction. Fail-closed: an envelope arriving on
	// one is dropped rather than routed off its own claim.
	ErrUnroutableSubject = errors.New("unroutable subject")
	// ErrSessionMismatch reports an envelope whose Session disagrees with the
	// session its subject authorizes.
	ErrSessionMismatch = errors.New("envelope session does not match the authorized subject")
)

// AuthorizeInSubject returns the (ns, name) that subject — an inbound session
// subject, "ap.session.<ns>.<name>.in.<kind>" — authorizes, after confirming
// env.Session claims that same pair.
//
// It wraps ErrUnroutableSubject when the subject is not a parseable inbound
// session subject, and ErrSessionMismatch when the envelope claims a different
// session; callers match with errors.Is. Both errors name the subject and the
// claimed session so the caller's log locates the publisher.
func AuthorizeInSubject(subject string, env Envelope) (ns, name string, err error) {
	return authorizeSubject(ParseInSubject, subject, env)
}

// AuthorizeOutSubject is AuthorizeInSubject's outbound twin, for consumers of
// "ap.session.<ns>.<name>.out.<kind>".
//
// It matters on a per-session (non-wildcard) subscription too: the subscriber
// pinned the subject, so anything delivered on it belongs to that session, but
// the decoded Envelope it forwards onward still carries the publisher's own
// claim. A consumer that hands that envelope to a viewer, a cache keyed by
// session, or another bus hop must confirm the claim first.
func AuthorizeOutSubject(subject string, env Envelope) (ns, name string, err error) {
	return authorizeSubject(ParseOutSubject, subject, env)
}

// authorizeSubject is the shared body: parse the subject with the direction's
// parser, then compare. Factored so the two directions can never drift apart in
// strictness, exactly as the subjects package does for the parsers themselves.
func authorizeSubject(
	parse func(string) (string, string, bool), subject string, env Envelope,
) (string, string, error) {
	claimed := env.Session.Namespace + "/" + env.Session.Name
	ns, name, ok := parse(subject)
	if !ok {
		return "", "", fmt.Errorf("%w %q (envelope claims %s)", ErrUnroutableSubject, subject, claimed)
	}
	if ns != env.Session.Namespace || name != env.Session.Name {
		return "", "", fmt.Errorf("%w: subject authorizes %s, envelope claims %s",
			ErrSessionMismatch, ns+"/"+name, claimed)
	}
	return ns, name, nil
}
