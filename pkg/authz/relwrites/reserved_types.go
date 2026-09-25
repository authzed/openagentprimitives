package relwrites

import (
	"fmt"
	"strings"

	guardianschema "github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
)

// Scaffold object types this package names in a policy decision. Declared as
// constants rather than written inline so the policy sets below read as
// statements about types, and so a rename shows up as a compile error at every
// site rather than as a silently-inert string comparison.
const (
	TypeUser         = "user"
	TypeGroup        = "group"
	TypeService      = "service"
	TypeAgentSession = "agentsession"
	TypeCluster      = "cluster"
	TypeGitHubUser   = "github_user"
)

// principalTypes are the scaffold types that denote a PERSON (or a set of
// people). A tuple whose subject is one of these hands authority to someone.
//
// github_user belongs here too: it denotes a person TRANSITIVELY — the
// account's #sole_user is what a directory-sync-style consumer would
// traverse for durable authority, so naming the account as a subject hands
// authority to whoever the controller currently attests as its sole
// claimant. Membership costs the PR-identity write flow nothing (it never
// names a scaffold RESOURCE), while closing off a writable scaffold resource
// ever handing a relation to an attested identity the same way it is already
// closed for user/group.
var principalTypes = map[string]struct{}{
	TypeUser:       {},
	TypeGroup:      {},
	TypeGitHubUser: {},
}

// writableScaffoldResourceTypes are the code-owned scaffold types a tool may
// name as a tuple's RESOURCE. Everything else the scaffold declares is
// refused.
//
// This is an ALLOWLIST on purpose, and it is the reason the check does not
// transcribe the scaffold: the full set of scaffold definitions is derived
// from schema.zed at runtime, so a definition added there later is refused by
// default rather than silently becoming writable by every tool. The cost of
// that default is one line here when a new type genuinely needs a tool-write
// flow; the cost of the other default is a new authorization object nobody
// remembered to protect.
//
// cluster is the one entry because one shipped flow needs it: a sandbox
// toolspec writes cluster:<name>#debug_target@agentsession:<session> to pin a
// session to the cluster it is debugging. Its dangerous relation — debugger,
// which grants a person debug access — is refused by the principal-subject
// rule below rather than by banning the type.
var writableScaffoldResourceTypes = map[string]struct{}{
	TypeCluster: {},
}

// scaffoldSubjectTypes are the scaffold types a tool may name as a tuple's
// SUBJECT: principals, plus the session itself, which is what the
// cluster-pinning flow binds.
//
// github_user is the one shipped flow that needs it: the PR-identity write —
// the one flow that names this type as a subject — names the attested
// GitHub account as the subject of a tenant-owned tuple, recording who
// opened a pull request. Its danger is contained the same way cluster is
// contained on the resource side — the account can only confer read through
// #sole_user, which the useridentity controller writes for an exclusive
// attested claim, never a tool — and it is a principalType, so the rule
// below still refuses handing it a relation on any writable scaffold
// resource. Naming it as a subject also requires the emitting block to
// declare RequireSlotBound — see slotBoundRequiredSubjectTypes below.
var scaffoldSubjectTypes = map[string]struct{}{
	TypeUser:         {},
	TypeGroup:        {},
	TypeService:      {},
	TypeAgentSession: {},
	TypeGitHubUser:   {},
}

// slotBoundRequiredSubjectTypes are the subject types whose admission depends
// on WHERE the tuple came from, not just what it is: ValidateResolvedTuple
// alone allows these (scaffoldSubjectTypes admits the type; the resource in
// hand is ordinarily tenant-owned, so the principal rule never fires), but
// naming one as a SUBJECT is only safe from a block whose resource was
// already proved against a real, human-bound grant — RequireSlotBound true.
//
// github_user is the one entry, deliberately NOT all of principalTypes: it
// denotes a person TRANSITIVELY, through whichever platform subject the
// useridentity controller currently attests as the account's sole_user, so
// the resource a tool names alongside it must itself be gated on a checked
// grant rather than on a CEL expression a tool author fully controls. user,
// group and service carry no such indirection and are legitimately written
// by ordinary, non-slot-bound blocks today — widening this set to include
// them would refuse flows that work correctly now. Widening it further is a
// deliberate act, never a default.
var slotBoundRequiredSubjectTypes = map[string]struct{}{
	TypeGitHubUser: {},
}

// ValidateSlotBoundSubject refuses a resolved tuple whose subject type is in
// slotBoundRequiredSubjectTypes when the emitting block does not declare
// RequireSlotBound.
//
// ValidateResolvedTuple cannot enforce this on its own: it sees only the
// tuple, never which block emitted it or whether that block opted into the
// slot-bound gate. Without this check, a writesRelationships block naming a
// github_user subject on a tenant resource but never setting
// requireSlotBound: true would pass ValidateResolvedTuple (the type is
// allowlisted) and never reach the slot-bound checker either (Run only
// consults it for a marked block) — admitted by neither gate, exactly the
// discipline-a-future-edit-forgets failure this closes. Called once per
// resolved tuple in Evaluate's emit closure, the one place both the tuple and
// its emitting block's RequireSlotBound flag are in hand.
//
// The error is tagged slotBoundRefusal (same wrapper filterSlotBound uses),
// not returned bare: this is an authorization ruling — a manifest naming a
// subject type that hands durable authority without ever proving the write
// is bound to a real grant — not a mechanism failure, and it needs to reach
// a caller through errors.Is(err, ErrSlotBoundRefused) the same way a
// filterSlotBound refusal does. Evaluate returns this error unwrapped, and
// Run's `fmt.Errorf("block %d: evaluate: %w", ...)` preserves the tag through
// its own wrap, so errors.Is still finds it from the caller's joined error.
// Without the tag, this case fell through to a dispatcher's default
// "mechanism failure" arm — logged only, tool.Result untouched — which is
// the exact silent-refusal shape RequireSlotBound exists to close.
func ValidateSlotBoundSubject(t ResolvedTuple, requireSlotBound bool) error {
	subType := objectType(t.Subject)
	if _, required := slotBoundRequiredSubjectTypes[subType]; required && !requireSlotBound {
		return slotBoundRefusal{fmt.Errorf("tuple subject type %q requires a slot-bound block; set requireSlotBound: true (resource=%q, relation=%q, subject=%q)",
			subType, t.Resource, t.Relation, t.Subject)}
	}
	return nil
}

// ValidateResolvedTuple is the structural gate every tuple must pass before it
// reaches the writer.
//
// Until this existed the only check was that resource and subject each contain
// a colon. Which type, which relation and which subject all came from CEL over
// `args` — model-authored — and `result`, the upstream MCP server's response,
// and the runner wrote the result with the preshared SpiceDB token mounted
// into every runner pod. Every escalation target is schema-valid, so SpiceDB
// refused none of them: agentsession#owner confers approve/hold/manage_scope/
// fork, agentsession#parent forges delegation lineage, pt_tag#direct_reader
// widens a datum's audience, an uncaveated externaltoken grant satisfies
// use_token regardless of the presented value, and platform#admin is the
// cluster.
//
// It needs no malicious tenant to matter: a shipped example writes an owner
// tuple straight from response items, keyed on an email in the response, so a
// hostile or compromised upstream can make any address the approver of any
// record.
//
// Three rules:
//
//   - shape: resource and subject must both be <type>:<id>;
//   - a scaffold type may be the RESOURCE only if it is explicitly writable;
//   - a scaffold type may be the SUBJECT only if it is a principal or the
//     session, and a writable scaffold resource still may not hand a PRINCIPAL
//     a relation — which is what separates cluster#debug_target@agentsession
//     from cluster#debugger@user.
//
// What this deliberately does NOT do is check that the type belongs to the
// MCPServer declaring the write. That is the stronger rule and belongs at
// admission, where that server's own spiceDBSchema.resources are in hand.
// This is the runtime backstop, and it still holds when the type is chosen by
// a CEL expression at call time.
func ValidateResolvedTuple(t ResolvedTuple) error {
	if !strings.Contains(t.Resource, ":") || !strings.Contains(t.Subject, ":") {
		return fmt.Errorf("malformed tuple (resource=%q, subject=%q): both must be <type>:<id>", t.Resource, t.Subject)
	}

	resType := objectType(t.Resource)
	if isScaffoldType(resType) {
		if _, ok := writableScaffoldResourceTypes[resType]; !ok {
			return fmt.Errorf("tuple resource type %q is reserved: a tool may not write relationships on the platform's own authorization objects (resource=%q, relation=%q)",
				resType, t.Resource, t.Relation)
		}
	}

	subType := objectType(t.Subject)
	if isScaffoldType(subType) {
		if _, ok := scaffoldSubjectTypes[subType]; !ok {
			return fmt.Errorf("tuple subject type %q is reserved: a tool may not name a platform authorization object as the subject (subject=%q)",
				subType, t.Subject)
		}
	}

	// A writable scaffold resource still must not hand a PERSON authority on a
	// platform object. The session-pinning flow writes
	// cluster#debug_target@agentsession, which grants nobody anything; what
	// this refuses is cluster#debugger@user, which the resource rule alone
	// would allow.
	if _, principal := principalTypes[subType]; principal && isScaffoldType(resType) {
		return fmt.Errorf("tuple would grant %q a relation on the reserved type %q: a tool may not hand a principal authority over a platform object (resource=%q, relation=%q)",
			t.Subject, resType, t.Resource, t.Relation)
	}
	return nil
}

// isScaffoldType reports whether typ is a code-owned scaffold definition.
//
// Derived from the compiled scaffold rather than transcribed, so it cannot
// drift: a definition added to schema.zed is recognized here with no matching
// edit, and — because the resource rule is an allowlist — is refused by
// default until someone decides it should be writable.
func isScaffoldType(typ string) bool {
	_, ok := guardianschema.ReservedDefinitionNames()[typ]
	return ok
}

// objectType takes the <type> half of a <type>:<id> reference. A subject may
// carry a relation suffix (group:eng#member); the type is still what precedes
// the colon.
func objectType(ref string) string {
	typ, _, _ := strings.Cut(ref, ":")
	return typ
}
