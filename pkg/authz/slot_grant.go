package authz

import (
	"context"
	"errors"
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

	// Occupancy and Rebind are copied from the slot declaration the binding
	// came from. Empty Occupancy means "single" (the field's default): a
	// binding whose producer predates these fields is gated, never un-gated,
	// by omission.
	Occupancy string
	Rebind    string

	// PriorID, when non-zero, means this approved binding MOVES a filled
	// single-occupancy slot from PriorID to ResourceID — atomically, with
	// PriorID's grants revoked in the same request. It is derived ONLY from
	// the approval record's MovedFrom (what the card showed the approver),
	// never from a live pin read at decision time: a card can sit parked for
	// days, and the human must get exactly the move they were shown. If the
	// pin is no longer PriorID at execution, the move's MUST_MATCH fails and
	// the approval fails loudly rather than moving something unseen.
	PriorID ObjectID
}

// SlotOccupancySingle and SlotOccupancyMulti name the two occupancy modes a
// slot declares. They mirror the AgentClass CRD's `occupancy` enum
// (v1alpha1.AuthzSlotOccupancyDefault); pkg/authz cannot import v1alpha1 (that
// package imports this one), so the contract strings are restated here.
//
// Empty reads as single — see occupancyOf. A binding produced before these
// fields existed is therefore GATED, never silently un-gated, by omission.
const (
	SlotOccupancySingle = "single"
	SlotOccupancyMulti  = "multi"
)

// SlotRebindNever names the one rebind mode whose refusal message routes to a
// new session rather than to a plan amendment. The default ("approval", and the
// empty value) is handled by the switch default in GrantSlots, so it needs no
// constant of its own.
const SlotRebindNever = "never"

// occupancyOf reports a binding's occupancy, defaulting empty to single. The
// fail-closed default lives here so every reader agrees on it.
func occupancyOf(b SlotBinding) string {
	if b.Occupancy == "" {
		return SlotOccupancySingle
	}
	return b.Occupancy
}

// GrantSlots writes a slot grant per binding, enforcing single-occupancy
// pinning PER RESOURCE TYPE.
//
// Partitioned by type, not all-or-nothing across the whole call. A
// single-occupancy type binds at most one instance for the session's life: a
// second DISTINCT instance of that type is refused (ErrSlotPinned), and the
// refusal names the route out. But a refusal on ONE type must not strand
// another type's bindings in the same call — those are independent grants a
// human may already have approved — so each type is resolved on its own:
//
//   - single-occupancy types go through the SlotPinner (EnsurePin +
//     WriteGrantsPinned), which records the one pinned instance and guards the
//     grant write against a concurrent move;
//   - multi-occupancy types keep the plain batched TOUCH write, unpinned, each
//     addition gated as before.
//
// Within a single type the write is still atomic (its grants land together or
// not at all); across types, a refused type leaves the others untouched. Input
// validation (malformed binding, missing expiry) still fails the WHOLE call
// before anything is written — a caller that handed a nonsense binding has a
// bug, not a partial set to salvage.
//
// A non-nil RelWriter that does not implement SlotPinner cannot bind a
// single-occupancy type: the gate refuses that type rather than writing it
// through unpinned as if it were unmarked. The nil-writer early return stays
// first — nil grants nothing, so there is nothing to protect.
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

	// One group per resource type, preserving first-appearance order so writes
	// and refusals are deterministic. All validation runs in this pass, before
	// any write, so a malformed binding fails the whole call rather than leaving
	// a partial set behind.
	type typeGroup struct {
		rels    []Relation
		ids     map[string]struct{}
		firstID string
		single  bool
		rebind  string
	}
	order := make([]string, 0, len(bindings))
	groups := make(map[string]*typeGroup, len(bindings))
	for _, b := range bindings {
		if b.ResourceType == "" || b.ResourceID.IsZero() || b.Permission == "" {
			// An empty id names no instance; an empty permission names no
			// relation, so the tuple would grant nothing and be unrevocable.
			return fmt.Errorf("authz: slot binding needs resourceType, resourceID and permission: %+v", b)
		}
		grp := groups[b.ResourceType]
		if grp == nil {
			grp = &typeGroup{ids: make(map[string]struct{})}
			groups[b.ResourceType] = grp
			order = append(order, b.ResourceType)
		}
		// A type is single unless EVERY binding of it is multi: mixed occupancy
		// within one call is a producer bug, and the single (pinned) treatment is
		// the fail-closed one.
		if occupancyOf(b) != SlotOccupancyMulti {
			grp.single = true
		}
		if grp.rebind == "" {
			grp.rebind = b.Rebind
		}
		rel := SlotGrantRelation(b.ResourceType, b.ResourceID.String(), b.Permission, scope)
		rel.ExpiresAt = expiresAt
		grp.rels = append(grp.rels, rel)
		id := b.ResourceID.String()
		if _, seen := grp.ids[id]; !seen {
			grp.ids[id] = struct{}{}
			if grp.firstID == "" {
				grp.firstID = id
			}
		}
	}

	// refusals accumulates EVERY per-type refusal and store failure (and, below, a failed plain
	// write) so the remaining types still land and NO refusal is silently
	// dropped. Keeping only the first would hide a second single-occupancy type's
	// refusal behind the first — the model would be told about one blocked target
	// and left to rediscover the next by trying it. errors.Join at the end keeps
	// every errors.Is(…, ErrSlotPinned) reachable for the dispatchers that route
	// on the sentinel.
	var refusals []error
	var plainRels []Relation
	for _, rt := range order {
		grp := groups[rt]
		if !grp.single {
			plainRels = append(plainRels, grp.rels...)
			continue
		}
		// Single-occupancy type. More than one distinct instance arriving in one
		// set cannot be reconciled to a single occupant, so it is refused outright
		// — nothing is pinned and nothing is granted for this type.
		if len(grp.ids) > 1 {
			refusals = append(refusals, fmt.Errorf("%w: slot type %s cannot hold %d distinct instances at once; it is single-occupancy",
				ErrSlotPinned, rt, len(grp.ids)))
			continue
		}
		pinner, ok := g.(SlotPinner)
		if !ok {
			// A plain RelWriter cannot express the pin's precondition, so writing
			// the grant through it would silently drop the single-occupancy
			// guarantee. Refuse this type rather than write it unpinned.
			refusals = append(refusals, fmt.Errorf("authz: single-occupancy slot type %s needs a SlotPinner-capable writer, got %T", rt, g))
			continue
		}
		id := grp.firstID
		held, pinnedID, err := pinner.EnsurePin(ctx, rt, id, scope)
		if err != nil {
			// Collected, not returned: a store failure on THIS type must not
			// discard refusals already gathered for others, nor skip the types
			// and the multi-occupancy write still to come.
			refusals = append(refusals, fmt.Errorf("authz: ensure pin for %s:%s: %w", rt, id, err))
			continue
		}
		if held && pinnedID != id {
			// Already pinned to a DIFFERENT instance. The refusal text is
			// user-visible — it becomes text the model reads — so it names the
			// pinned instance and the route out, chosen by the slot's rebind mode.
			switch grp.rebind {
			case SlotRebindNever:
				refusals = append(refusals, fmt.Errorf("%w: this session is pinned to %s:%s for its lifetime; start a new session to target %s:%s",
					ErrSlotPinned, rt, pinnedID, rt, id))
			default: // "approval" and unset
				refusals = append(refusals, fmt.Errorf("%w: this session is pinned to %s:%s; to work on %s:%s, propose an updated plan naming it — an approved plan moves the pin",
					ErrSlotPinned, rt, pinnedID, rt, id))
			}
			continue
		}
		// held && pinnedID == id: already pinned to this SAME instance (a second
		// permission, or a re-grant after expiry) — not drift, so it still binds.
		if err := pinner.WriteGrantsPinned(ctx, grp.rels, rt, id, scope); err != nil {
			// Collected for the same reason as an EnsurePin failure. A pin that
			// EnsurePin just wrote stays: it names the instance this call meant
			// to bind, so a retry is a same-instance bind, not a refusal.
			refusals = append(refusals, fmt.Errorf("authz: write pinned slot grants for %s:%s: %w", rt, id, err))
			continue
		}
	}

	if len(plainRels) > 0 {
		if err := g.WriteRelationships(ctx, plainRels); err != nil {
			// JOIN, not return: a plain-write failure must not discard a pin
			// refusal already accumulated above for another type in this call —
			// the caller needs to see both the multi-occupancy write failure AND
			// the single-occupancy refusal that stood beside it.
			refusals = append(refusals, fmt.Errorf("authz: write slot grants: %w", err))
		}
	}
	return errors.Join(refusals...)
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
	// ListSlotPins returns every pin the parent holds, verbatim — resource and
	// subject only, no derived occupancy. See SlotPinRelation.
	ListSlotPins(ctx context.Context, ns, name string) ([]Relation, error)
	// CopySlotTuples writes rels in one batched TOUCH — no gate, no expiry
	// stamping of its own. The caller (CopySlotGrants) has already re-targeted
	// each relation's SubjectID at the child and, for a grant, stamped the
	// child's expiry; a pin tuple carries none, matching how EnsurePin writes
	// it.
	CopySlotTuples(ctx context.Context, rels []Relation) error
}

// CopySlotGrants re-points the parent's slot grants AND pins at the child,
// VERBATIM, returning how many grants were carried.
//
// VERBATIM, not re-gated through GrantSlots: a lifecycle clone is not a new
// arrival. GrantSlots enforces single-occupancy from a SlotBinding's declared
// Occupancy field, which a tuple read back out of SpiceDB cannot reconstruct —
// ListSlotGrants returns bindings with no Occupancy set, and occupancyOf reads
// that as "single" for every type, fail-closed. Routing the copy back through
// GrantSlots would therefore refuse outright any restart of a multi-occupancy
// slot currently holding two or more instances, and any legacy two-instance
// session recorded before single-occupancy pinning existed — copying NOTHING
// for that type on every such restart. The pin invariant still holds: a pin is
// listed from the parent and copied as-is onto the child, which starts with no
// pin of its own, so there is nothing for the copied pin to conflict with.
//
// A grant names the SESSION as its subject, so a child session inherits nothing
// automatically — without this, a continuation would silently lose every
// instance a human had approved and start re-asking for values the user already
// granted. A pin is the same kind of tuple, naming the session as subject: left
// uncopied, the child starts unpinned and its first bind can land on a
// different instance than the one the parent had already committed to, with no
// error to say so.
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
	pins, err := c.ListSlotPins(ctx, parent.Namespace, parent.Name)
	if err != nil {
		return 0, fmt.Errorf("authz: list parent slot pins %s: %w", parent, err)
	}
	if len(held) == 0 && len(pins) == 0 {
		return 0, nil
	}

	rels := make([]Relation, 0, len(held)+len(pins))
	for _, b := range held {
		rel := SlotGrantRelation(b.ResourceType, b.ResourceID.String(), b.Permission, child)
		rel.ExpiresAt = expiresAt
		rels = append(rels, rel)
	}
	for _, p := range pins {
		// The pin is otherwise copied byte-for-byte; only the subject moves.
		p.SubjectID = child.String()
		rels = append(rels, p)
	}

	if err := c.CopySlotTuples(ctx, rels); err != nil {
		return 0, fmt.Errorf("authz: copy slot tuples to child %s: %w", child, err)
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
