package authz

import (
	"fmt"
	"regexp"
	"strings"
)

// SubjectType is a SpiceDB subject object type — the token before ':' in a
// "<type>:<id>" reference.
type SubjectType string

const (
	SubjectUser         SubjectType = "user"
	SubjectService      SubjectType = "service"
	SubjectGroup        SubjectType = "group"
	SubjectAgentSession SubjectType = "agentsession"
)

// subjectRE matches a SpiceDB subject "<type>:<id>" with an optional
// "#<relation>" suffix. The id charset covers base64url user canonicals
// (A-Za-z0-9-_), "<ns>/<name>" object ids, and service/bot names.
var subjectRE = regexp.MustCompile(`^([a-z][a-z0-9_]*):([A-Za-z0-9._@/-]+)(#[a-z][a-z0-9_]*)?$`)

// ValidateSubject reports whether s is a well-formed CONCRETE SpiceDB subject
// "<type>:<id>" whose type is one of allowed, with NO "#relation" suffix.
//
// This is the guard for any site that ASSIGNS a subject as the identity a
// session acts as (e.g. Channel.spec.authzSubject → started_by). Such a
// subject must never be arbitrary: a less-trusted config source must not be
// able to assert a human "user:" (impersonation) or a malformed value. Sites
// that merely write relationships or name a subject-set to CHECK against do
// NOT use this — they are not identity assignment.
//
// allowed must be non-empty; an empty set rejects everything (fail closed).
func ValidateSubject(s string, allowed ...SubjectType) error {
	return validateSubject(s, false, allowed)
}

// ValidateSubjectSet is ValidateSubject but also permits a subject-set
// reference "<type>:<id>#<relation>" (e.g. "group:eng#member") — for sites
// that name a set to check against rather than a concrete acting subject.
func ValidateSubjectSet(s string, allowed ...SubjectType) error {
	return validateSubject(s, true, allowed)
}

func validateSubject(s string, allowRelation bool, allowed []SubjectType) error {
	m := subjectRE.FindStringSubmatch(s)
	if m == nil {
		return fmt.Errorf("invalid subject %q: must be %q", s, "<type>:<id>")
	}
	if m[3] != "" && !allowRelation {
		return fmt.Errorf("invalid subject %q: a concrete subject may not carry a #relation suffix", s)
	}
	typ := SubjectType(m[1])
	for _, a := range allowed {
		if typ == a {
			return nil
		}
	}
	return fmt.Errorf("subject %q has type %q, not permitted here (allowed: %s)", s, typ, joinTypes(allowed))
}

func joinTypes(types []SubjectType) string {
	if len(types) == 0 {
		return "(none)"
	}
	ss := make([]string, len(types))
	for i, t := range types {
		ss[i] = string(t)
	}
	return strings.Join(ss, ", ")
}

// ParseAgentSessionSubject parses an "agentsession:<namespace>/<name>" SpiceDB
// subject into the session it names.
//
// It is the ONE parser for that shape. The same string reaches three
// unrelated packages — Channel.spec.authzSubject (the counterparty end of an
// agent channel), read by both the agent kind and channelsd's pair-Channel
// resolution, and channelkinds.InboundEvent.AuthzSubject (the acting subject
// of an inbound), checked by the authz gate — and each of them used to cut it
// apart itself. One of those copies checked only the prefix and the '/', so
// what kept it from accepting an id full of arbitrary bytes was a CRD Pattern
// in a different repository file happening to agree with subjectRE.
// Consolidated so the charset guarantee lives with the parser rather than
// resting on that.
//
// ValidateSubject does the hard half: it is the repo's one definition of a
// well-formed concrete subject, and pinning the type to SubjectAgentSession is
// what makes "user:…" and "service:…" fail here rather than being cut into a
// plausible-looking namespace and name.
func ParseAgentSessionSubject(s string) (SessionRef, error) {
	if err := ValidateSubject(s, SubjectAgentSession); err != nil {
		return SessionRef{}, err
	}
	// The type token is already validated, so everything after the first ':'
	// is the object id — cut there rather than re-spelling the prefix, which
	// would then be two spellings to keep in step.
	_, id, _ := strings.Cut(s, ":")
	ns, name, ok := strings.Cut(id, "/")
	if !ok || ns == "" || name == "" {
		return SessionRef{}, fmt.Errorf(
			"invalid subject %q: an agentsession id must be %q", s, "<namespace>/<name>")
	}
	return SessionRef{Namespace: ns, Name: name}, nil
}
