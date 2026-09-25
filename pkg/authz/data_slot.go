package authz

import (
	"context"
	"fmt"
	"time"
)

// A DATA slot hands a child specific information; an AUTHZ slot hands it
// authority. Two columns of one mechanism: both are declared on the child,
// bound at handoff, per-session, human-approvable, expiring, and torn down at
// SessionEnd, and both are checked by exactly one SpiceDB permission.
//
//	           binds                       checked by
//	authz      a resource instance         slot_grant_<perm>->interact + owner
//	data       a pt-tag whose content      pt_tag#reader / pt_tag#access
//	           the child may read

// DataSlotRelation is the relation a data slot writes. It is `granted_to` on
// the tag — declared `agentsession with expiration` — so a bound tag resolves
// through `access` for the child and expires on its own if teardown never runs.
const DataSlotRelation = "granted_to"

// DataSlotResourceType is the object a data slot binds.
const DataSlotResourceType = "pt_tag"

// DataSlotBinding binds one pt-tag into one of a child's declared data slots.
//
// BY REFERENCE, and the absence of a content field is the design rather than
// an omission. The parent names a tag; the child reads the datum through that
// tag's own audience check. Because there is nowhere in this struct to put
// bytes, a parent structurally CANNOT launder content into a data slot — which
// is the architectural separation of instruction and data channels the handoff
// design rests on, and the thing a provenance-recovery result says is required.
//
// Adding a field that could carry the datum would defeat the whole mechanism
// while looking like a convenience. TestDataSlotBindingCarriesAReferenceAndNothingElse
// is what stands in the way.
type DataSlotBinding struct {
	// Slot is the slot name the child declared ("diff", "logs"). It is the
	// child's vocabulary, not the tag's: the same tag bound into two slots is
	// two bindings, and a child asks for more by naming a slot.
	Slot string
	// TagID is the pt_tag object id. The whole payload.
	TagID string
}

// DataSlotGrantRelation builds the tuple binding one tag to one session:
//
//	pt_tag:<tagID>#granted_to@agentsession:<ns>/<name>
//
// Pointed at the TAG rather than at the session, matching SlotGrantRelation's
// direction and for the same reason: the tag's own permissions then resolve
// the session's membership, so `access` and `reader` answer per-holder rather
// than needing a wildcard leaf.
//
// Note what this does NOT do: `granted_to` is deliberately not arrowed onward
// in the schema, so binding a tag to a child grants that child alone. A
// grandchild needs its own binding, which is monotonic attenuation applied to
// information.
func DataSlotGrantRelation(tagID string, scope SessionRef) Relation {
	return Relation{
		ResourceType: DataSlotResourceType,
		ResourceID:   tagID,
		Relation:     DataSlotRelation,
		SubjectType:  "agentsession",
		SubjectID:    scope.Namespace + "/" + scope.Name,
	}
}

// GrantDataSlots binds tags into a child's data slots.
func GrantDataSlots(ctx context.Context, g RelWriter, scope SessionRef, bindings []DataSlotBinding, expiresAt time.Time) error {
	if g == nil || len(bindings) == 0 {
		return nil
	}
	if expiresAt.IsZero() {
		// The schema declares granted_to `with expiration`, so SpiceDB would
		// refuse the write anyway — but say WHY here rather than surfacing a
		// caveat error from three layers down, exactly as GrantSlots does.
		return fmt.Errorf("authz: data slot grant needs an expiry (schema requires one)")
	}
	rels := make([]Relation, 0, len(bindings))
	for _, b := range bindings {
		if b.Slot == "" || b.TagID == "" {
			// An empty tag id names no datum; an empty slot name means the
			// child has nothing to ask for. Either way the tuple would grant
			// access to nothing while looking like a binding that worked.
			return fmt.Errorf("authz: data slot binding needs slot and tagID: %+v", b)
		}
		rel := DataSlotGrantRelation(b.TagID, scope)
		rel.ExpiresAt = expiresAt
		rels = append(rels, rel)
	}
	if err := g.WriteRelationships(ctx, rels); err != nil {
		return fmt.Errorf("authz: write data slot grants: %w", err)
	}
	return nil
}

// RevokeDataSlots removes data slot grants.
//
// Symmetric with GrantDataSlots by construction — the same relation builder
// produces both tuples — because revocation must be possible for every grant
// that can be written, or a binding outlives the reason it was made. The
// expiry is the backstop for a teardown that never runs, not a substitute for
// this.
func RevokeDataSlots(ctx context.Context, g RelWriter, scope SessionRef, bindings []DataSlotBinding) error {
	if g == nil || len(bindings) == 0 {
		return nil
	}
	rels := make([]Relation, 0, len(bindings))
	for _, b := range bindings {
		if b.TagID == "" {
			continue
		}
		rels = append(rels, DataSlotGrantRelation(b.TagID, scope))
	}
	if len(rels) == 0 {
		return nil
	}
	if err := g.DeleteRelationships(ctx, rels); err != nil {
		return fmt.Errorf("authz: revoke data slot grants: %w", err)
	}
	return nil
}

// SessionPermissionChecker answers whether a SESSION holds a permission on a
// resource.
//
// A session subject, not a user one, because that is the question data-slot
// attenuation asks: `access = session + session->ancestor + granted_to`, so a
// parent's standing on a tag is a fact about the SESSION the tag was minted
// in, not about whoever started it. The user-subject checks alongside this
// (CheckOnResource, HasAnyOfType) cannot express it.
type SessionPermissionChecker interface {
	SessionHasOnResource(ctx context.Context, resourceType, resourceID, permission string, session SessionRef) (bool, error)
}

// DataSlotAccessPermission is what a parent must hold on a tag to hand it on.
const DataSlotAccessPermission = "access"

// FilterDelegableDataSlots splits requested bindings into those the parent may
// delegate and those it may not.
//
// The rule is the data column's version of approverCanDelegateSlots: a parent
// must not bind what it has no standing to delegate. For a tag that is exactly
// `pt_tag:<T>#access@agentsession:<parent>` — one SpiceDB check per slot, no
// new enforcement path.
//
// What it buys is the plan gate's property: the parent's judgment SELECTS
// within an envelope it cannot widen. A compromised or injected parent can
// still choose badly among the tags it holds, but its worst case is bounded by
// its own reach — it grants nothing, and can only fail to deny.
//
// Refused bindings are RETURNED rather than dropped, so a caller can tell a
// parent which slots it asked for and did not get. Silently binding fewer
// slots than were requested leaves a child waiting on data that will never
// arrive, with nothing anywhere saying why.
func FilterDelegableDataSlots(
	ctx context.Context, c SessionPermissionChecker, parent SessionRef, requested []DataSlotBinding,
) (delegable, refused []DataSlotBinding, err error) {
	if len(requested) == 0 {
		return nil, nil, nil
	}
	if c == nil {
		// Fail closed, UNLIKE approverCanDelegateSlots, which treats an unwired
		// lookup as everything-delegable. That is safe there because the
		// instance axis is re-checked at OrderToolCallAuthz afterwards; a data
		// slot has no second gate behind it — binding the tag IS the grant, and
		// the child reads through it immediately.
		return nil, append([]DataSlotBinding(nil), requested...), nil
	}

	// One question per distinct TAG. A parent's standing on a tag does not
	// change between slots, and the same tag bound into two slots is two
	// bindings but one fact.
	seen := make(map[string]bool, len(requested))
	for _, b := range requested {
		if b.TagID == "" || seen[b.TagID] {
			continue
		}
		ok, cerr := c.SessionHasOnResource(ctx, DataSlotResourceType, b.TagID, DataSlotAccessPermission, parent)
		if cerr != nil {
			// An unanswerable question is not a yes. Treating a check failure
			// as delegable would let a transient outage widen what a parent
			// hands a child, at the one moment nobody is watching.
			return nil, nil, fmt.Errorf("authz: checking parent access to tag %q: %w", b.TagID, cerr)
		}
		seen[b.TagID] = ok
	}
	for _, b := range requested {
		if b.TagID != "" && seen[b.TagID] {
			delegable = append(delegable, b)
			continue
		}
		refused = append(refused, b)
	}
	return delegable, refused, nil
}
