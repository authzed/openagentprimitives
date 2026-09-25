package authz

import (
	"context"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz/precondition"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// SlotGrantRelationPrefix begins every slot-grant relation name. Used to
// recognise one without knowing its permission — teardown and listing sweep by
// prefix, since a session may hold grants for permissions its class no longer
// declares.
const SlotGrantRelationPrefix = "slot_grant_"

// SlotGrantRelationName is the relation a slot grant writes on the RESOURCE for
// one permission. Both the writer (this package) and the schema emitter
// (guardian/schema) derive the name here, so the two cannot drift.
//
// PER-PERMISSION, and that is the security property, not a naming preference.
// A slot grant is no longer bound to an exact call the way the session-grant
// mechanism was (its relation carried a check_hash caveat over the arguments
// hash). Dropping that binding is only safe while a grant for one permission
// cannot exercise ANOTHER permission on the same instance — otherwise a
// read-scoped slot on a repo would authorize a write, because the same tool (a
// git CLI) reaches the same resource id with different args.
//
// A single shared relation name breaks that outright: the composer emits
// `permission <p> = <rel>->interact + owner` per declared permission, so one
// tuple would satisfy every one of them. Composition is global, so the two
// permissions need not even come from the same AgentClass. Encoding the
// permission in the relation is what keeps the grants disjoint — the same
// reason the older grant_<perm>_<resType> relations were per-permission.
func SlotGrantRelationName(permission string) string {
	return SlotGrantRelationPrefix + permission
}

// SlotGrantRelation builds the tuple that binds one instance to one session:
//
//	<resourceType>:<resourceID>#slot_grant@agentsession:<ns>/<name>
//
// Note which way it points. The session-grant mechanism this replaces writes
// the mirror image — session#grant_<perm>_<type>@<type>:<id> — and that
// direction is why it needed a wildcard leaf to evaluate: following the arrow
// from the session to the resource then checks the resource's permission for
// the ORIGINAL user, who lacks it by construction. Pointing the tuple at the
// session instead means the resource's permission resolves the session's member
// set, so the Check is per-requester.
func SlotGrantRelation(resourceType, resourceID, permission string, scope SessionRef) Relation {
	return Relation{
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Relation:     SlotGrantRelationName(permission),
		SubjectType:  "agentsession",
		SubjectID:    scope.Namespace + "/" + scope.Name,
	}
}

// DefaultSlotGrantTTL bounds a slot grant when the session carries no
// wall-clock cap of its own.
//
// budget.sessionExpiration may legitimately be zero, meaning "no ceiling on how
// long this session may live". Zero must NOT become "no expiry" on the grant —
// that is precisely the leak the expiry exists to backstop, and it would let a
// session with no declared lifetime hold authority on somebody else's resource
// forever. A grant that expires early costs a re-approval; one that never
// expires costs a permanent grant nobody remembers issuing.
//
// Anchored to sessionExpiration and NOT to budget.maxDuration: maxDuration
// bounds ACTIVE RUN-TIME, which can be a few minutes of a session that stays
// reachable for days.
const DefaultSlotGrantTTL = 24 * time.Hour

// SlotGrantExpiry returns when a slot grant written now should lapse, given the
// session's wall-clock lifetime cap (budget.sessionExpiration). A zero or
// negative cap falls back to DefaultSlotGrantTTL.
func SlotGrantExpiry(now time.Time, sessionExpiration time.Duration) time.Time {
	if sessionExpiration <= 0 {
		sessionExpiration = DefaultSlotGrantTTL
	}
	return now.Add(sessionExpiration)
}

// SlotBinding is one instance bound into one slot.
//
// Permission is part of the binding, not a lookup detail: the grant tuple is
// per-permission, so a binding that omitted it could not name which grant to
// write or revoke.
type SlotBinding struct {
	ResourceType string
	// ResourceID is the DERIVED object id. Typed so a raw value cannot be
	// assigned here — see authz.ObjectID.
	ResourceID ObjectID
	Permission string
	// NoGrantRelation marks a binding whose resource type has no slot_grant
	// relation in the composed schema, because the AgentClass never declared
	// the type in spec.authz.slots. Such a binding narrows scope and grants
	// nothing — there is nothing it COULD grant.
	//
	// The zero value attempts the grant, which is what every producer with a
	// declared slot wants; only a caller that knows the type is undeclared sets
	// it. Defaulting the other way would silently turn class defaults and
	// promoted extractions into inert bindings.
	NoGrantRelation bool

	// RawID is the PRE-TRANSFORM value ResourceID was derived from — the
	// provider identifier a fact is keyed by, before ValueTransforms ran.
	// Carried ONLY by an approval that BindApproved must re-evaluate a
	// precondition for (the plan-gate path), because a fact read keys on the raw
	// form and the derived ObjectID cannot be reversed back to it. Empty for
	// every binding whose preconditions are not being evaluated here — the JIT
	// tool-approval and the human waiver — where nothing reads it.
	RawID string

	// Requires are the compiled preconditions on this binding's resource type,
	// from the AgentClass slot `requires[]`. Populated ONLY by the plan-gate
	// approval path, whose consent is to the PHASE and not to the risk a
	// precondition guards, so BindApproved must re-check them: an instance whose
	// gate is Refused (or Undetermined) is dropped here rather than bound, and
	// escalates to the human waiver card at its next tool call.
	//
	// Empty for the human WAIVER path — whose approval IS the consent to the
	// gate, recorded through PreconditionsWaived — and for every binding whose
	// type declares no precondition, where there is nothing to evaluate.
	Requires []precondition.Rule
}

// GrantSlots writes a slot grant per binding.
//
// It is deliberately all-or-nothing per call rather than best-effort per
// binding: these are authorization grants, and a partially-applied set would
// leave the agent believing it holds reach it does not, which surfaces later as
// an inexplicable denial mid-task rather than as a failure at bind time.
func GrantSlots(ctx context.Context, g RelWriter, scope SessionRef, bindings []SlotBinding, expiresAt time.Time) error {
	if g == nil || len(bindings) == 0 {
		return nil
	}
	if expiresAt.IsZero() {
		// The schema declares slot_grant `with expiration`, so SpiceDB would
		// refuse the write anyway — but say WHY here rather than surfacing a
		// caveat error from three layers down. A caller with no session budget
		// to anchor to should pass SlotGrantExpiry(now, 0).
		return fmt.Errorf("authz: slot grant needs an expiry (schema requires one); use SlotGrantExpiry")
	}
	rels := make([]Relation, 0, len(bindings))
	for _, b := range bindings {
		if b.ResourceType == "" || b.ResourceID.IsZero() || b.Permission == "" {
			// An empty id names no instance; an empty permission names no
			// relation, so the tuple would grant nothing and be unrevocable.
			return fmt.Errorf("authz: slot binding needs resourceType, resourceID and permission: %+v", b)
		}
		rel := SlotGrantRelation(b.ResourceType, b.ResourceID.String(), b.Permission, scope)
		rel.ExpiresAt = expiresAt
		rels = append(rels, rel)
	}
	if err := g.WriteRelationships(ctx, rels); err != nil {
		return fmt.Errorf("authz: write slot grants: %w", err)
	}
	return nil
}

// RevokeSlots removes slot grants.
//
// Revocation must be possible for every grant that can be written, or a binding
// is a one-way door for the life of the session — the same property the denied
// blocklist needed when channel-membership subjects became grantable.
func RevokeSlots(ctx context.Context, g RelWriter, scope SessionRef, bindings []SlotBinding) error {
	if g == nil || len(bindings) == 0 {
		return nil
	}
	rels := make([]Relation, 0, len(bindings))
	for _, b := range bindings {
		rels = append(rels, SlotGrantRelation(b.ResourceType, b.ResourceID.String(), b.Permission, scope))
	}
	if err := g.DeleteRelationships(ctx, rels); err != nil {
		return fmt.Errorf("authz: revoke slot grants: %w", err)
	}
	return nil
}

// SlotGrantCopier is the seam the fork path uses to carry a parent session's
// bound instances onto its child. *spicedb.Client satisfies it.
type SlotGrantCopier interface {
	ListSlotGrants(ctx context.Context, ns, name string) ([]SlotBinding, error)
	GrantSlots(ctx context.Context, ns, name string, bindings []SlotBinding, expiresAt time.Time) error
}

// CopySlotGrants re-points the parent's slot grants at the child, returning how
// many were carried.
//
// A grant names the SESSION as its subject, so a child session inherits nothing
// automatically — without this, a continuation would silently lose every
// instance a human had approved and start re-asking for values the user already
// granted.
//
// CALLERS MUST NOT CALL THIS FOR A takeover FORK. A takeover is a DIFFERENT user
// continuing a terminal session, and they become the child's owner; copying the
// grants would resolve slot_grant->interact + owner for a user on resources they
// never had standing on, handing them the previous owner's human-approved
// instance authority. That the agentsession#fork gate does not run for takeover
// makes the carve-out more necessary, not less — nothing else stops it. The
// decision lives at the call site because only there is the mode known; this
// function deliberately takes no mode argument, so a caller cannot pass the
// wrong one and believe it was handled.
//
// ORDERING: run this AFTER the denied blocklist is on the child. A slot grant
// resolves through slot_grant->interact, so it must never go live ahead of the
// membership it resolves against — the same window restart.go documents for
// copying denied before participants.
func CopySlotGrants(ctx context.Context, c SlotGrantCopier, parent, child SessionRef, expiresAt time.Time) (int, error) {
	if c == nil {
		return 0, nil
	}
	held, err := c.ListSlotGrants(ctx, parent.Namespace, parent.Name)
	if err != nil {
		return 0, fmt.Errorf("authz: list parent slot grants %s: %w", parent, err)
	}
	if len(held) == 0 {
		return 0, nil
	}
	if err := c.GrantSlots(ctx, child.Namespace, child.Name, held, expiresAt); err != nil {
		return 0, fmt.Errorf("authz: grant child slot grants %s: %w", child, err)
	}
	return len(held), nil
}

// SlotRevokeChecker is the standing gate for removing a slot grant.
// *spicedb.Client satisfies it.
type SlotRevokeChecker interface {
	CheckApprove(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
	CheckPlatformPermission(ctx context.Context, permission string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
}

// PlatformKillSession is the platform permission that carries revocation
// standing. Named rather than can_admin because callers must go through a
// per-area permission — the indirection is what lets the schema grow real
// per-area relations later.
const PlatformKillSession = "kill_session"

// CheckMayRevokeSlot reports whether canonicalID may remove a slot grant from
// this session.
//
// Standing is SYMMETRIC WITH APPROVAL, plus platform admin. Whoever could have
// granted the instance can take it back, which is the property that keeps
// revocation usable: a rule where approving is easier than un-approving leaves
// people unable to undo their own decisions, and they stop approving carefully.
// Platform admin is included because kill_session already means "end this
// session's authority" — being able to stop a session but not to withdraw one
// instance it holds would be a strange gap.
//
// FULLY CONSISTENT, deliberately. Revocation is the direction where a stale read
// is unsafe in the way that matters: standing that was withdrawn a moment ago
// must not still authorize taking authority away — and more importantly, a
// caller who was JUST granted approve standing must not be told they lack it.
//
// Fail-closed: any error denies, and the error is returned so the caller can
// say why rather than reporting a bare refusal.
func CheckMayRevokeSlot(ctx context.Context, c SlotRevokeChecker, sess SessionRef, canonicalID identity.CanonicalUserID) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("authz: no revoke checker wired")
	}
	ok, err := c.CheckApprove(ctx, sess.Namespace, sess.Name, canonicalID, true)
	if err != nil {
		return false, fmt.Errorf("authz: check approve standing for revoke: %w", err)
	}
	if ok {
		return true, nil
	}
	admin, err := c.CheckPlatformPermission(ctx, PlatformKillSession, canonicalID, true)
	if err != nil {
		return false, fmt.Errorf("authz: check platform standing for revoke: %w", err)
	}
	return admin, nil
}
