// Package relsource records which in-tree component owns which SpiceDB
// relations, and refuses a write or delete filter that would touch a
// relation another component claims.
//
// It is a separate package from pkg/authz/spicedb itself so a writer that
// registers a claim can import it without importing the SpiceDB client and
// creating an import cycle.
//
// Nothing in this package calls the SpiceDB client — this is the decision
// procedure only. pkg/authz/spicedb.RelWriter is the caller: every
// WriteRelationships/DeleteRelationships it handles is checked here first.
// Several in-tree packages register real claims from their own init()
// (pkg/memory/pttagmint, pkg/memory/spicedbauthorizer,
// pkg/authz/guardian/approval, pkg/channels/channelkinds/slack, and
// pkg/authz/spicedb itself), but a claim only takes effect in a binary that
// actually links the package declaring it — see
// pkg/authz/spicedb/relsource/imports and MarkComplete for how every
// binary that can construct a guarded RelWriter is kept honest about that.
package relsource

// Source is a component that writes relationships, and the relations it OWNS.
//
// Ownership is exclusive: a relation one source claims cannot be written by
// another. A relation NO source claims is unrestricted, which is what lets this
// land incrementally — declaring a claim tightens one relation without
// requiring every other relation be declared first.
//
// This is a CORRECTNESS guard, not a security boundary. Name is a value a
// caller supplies, so any in-tree package can construct a Source claiming to be
// any component, exactly as it could edit the claims themselves. What this
// catches is a well-meaning writer touching a relation it does not own — at the
// call, rather than in an incident.
type Source struct {
	// Name identifies the writer in refusal errors. Not a security identity.
	Name string
	// Claims are "definition#relation" pairs this source exclusively owns.
	Claims []string

	// SubjectIdentityClaims are the claims (a subset of Claims) whose tuples
	// link a platform `user:` subject DIRECTLY to an external identity object —
	// the set an admin console can resolve by filtering on that user as the
	// subject.
	//
	// Declared per source rather than inferred, because only the source knows
	// its own tuple SHAPE. A membership relation whose subject is a userset
	// (github_org:acme#member@github_user:123#sole_user) is NOT one of these:
	// no user-subject filter can match it, so listing it here would produce a
	// probe that silently returns nothing — a short answer that looks like a
	// complete one.
	//
	// Leaving it empty is the correct declaration for a source that writes no
	// such link at all, and for every writer that is not a directory sync
	// (memory, pt_tag, leakage): a reader walking Claims instead would probe
	// memory_entry#creator — TOUCHed on every memory Put by every user — and
	// render one console row per memory entry that person ever created.
	//
	// Register refuses an entry that is not also in Claims, or that names the
	// hash sentinel: both are provably no-op probes, and catching them at
	// registration is the whole point of declaring the set by hand.
	SubjectIdentityClaims []string

	// DisplayName is how this source is named to a human ("GitHub",
	// "1Password"). Falls back to Name when empty — see Display.
	DisplayName string
}

// Display is the human-facing name for this source: DisplayName when set, else
// Name. Name is a wire-ish identifier chosen for refusal errors
// ("githubdirectorysync"), which reads badly in a console row; Display is what
// a UI should render.
func (s Source) Display() string {
	if s.DisplayName != "" {
		return s.DisplayName
	}
	return s.Name
}
