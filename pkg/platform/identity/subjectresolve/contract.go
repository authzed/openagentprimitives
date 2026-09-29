// Package subjectresolve resolves an agent-supplied user REFERENCE to a
// canonical platform user, server-side, against records the platform already
// trusts — never an agent-claimed mapping. Unknown schemes, malformed ids,
// and ambiguous joins are unresolved, fail-closed, with a human-readable
// reason the caller can relay.
//
// A reference is handled by exactly one registered Resolver, tried in
// registration order: scheme-specific resolvers (email, trigger-author) are
// registered first and only claim a reference that is unambiguously theirs;
// the generic resource fallback is registered last and claims anything
// syntactically shaped like "<type>:<id>". Adding a new reference form is a
// new Resolver + a Register call — never a switch in a consumer — and
// Usages() derives the `get_preferences` tool's user-argument documentation
// from exactly the same registrations, so the two can never drift apart.
package subjectresolve

import "context"

// Env carries the injected capabilities resolvers may use.
type Env struct {
	// Relations reads one object's user-mapping relations from SpiceDB.
	Relations RelationReader
	// SessionAnnotations returns the owning AgentSession's annotations
	// (for session-derived references like trigger-author). Nil when no
	// session context exists.
	SessionAnnotations func(ctx context.Context) (map[string]string, error)
}

// RelationReader is the narrow SpiceDB surface the resource resolver needs.
type RelationReader interface {
	// UserSubjects returns the bare canonical ids of the `user:` subjects on
	// the given object's relation. Resolution reads "sole_user" only — the
	// durable single-claimant authority — never the session-membership
	// "user" relation, which is legitimately multi-claimant and may be
	// mid-derivation.
	UserSubjects(ctx context.Context, objType, objID, relation string) ([]string, error)
}

// Resolution is the outcome of resolving one reference.
type Resolution struct {
	// Subject is the bare canonical user id; empty when unresolved.
	Subject string
	// SubjectProven reports whether the resolver VOUCHES that the platform
	// links Subject to the reference — true only for a resolution backed by a
	// linkage authority (a resource's sole_user relation, trigger-author's
	// recursion into one). The email resolver leaves it false: it
	// canonicalizes any well-formed address into a subject in FORM only, with
	// nothing behind it, so a consumer must apply its own existence bar
	// before treating an unproven subject as a real platform user. The zero
	// value is unproven on purpose — a future resolver that forgets to claim
	// proof gets the stricter treatment, not the looser one.
	SubjectProven bool
	// Reason says why resolution failed, in words an agent may relay.
	Reason string
}

// Resolver is one reference-scheme handler, registered via Register.
type Resolver interface {
	// Usage returns the reference form and a one-line description, exactly as
	// shown to the model in tool schemas (see Usages). Form is the literal
	// syntax (e.g. "email:<address>"); description says whom it resolves
	// (e.g. "the platform user with this verified email").
	Usage() (form, description string)

	// TryResolve attempts to resolve ref under this resolver's scheme.
	// matched=false means ref is outside this resolver's scheme entirely, and
	// Resolve tries the next registered resolver. matched=true commits to this
	// resolver's outcome (res, err) even when res is unresolved.
	TryResolve(ctx context.Context, ref string, env Env) (res Resolution, matched bool, err error)
}

// Usage is one resolver's self-description, as surfaced by Usages().
type Usage struct {
	Form        string
	Description string
}
