package spicedb

// RelationWriter's implementation of authz.SlotPinner — the
// precondition-capable surface the single-occupancy gate (authz.GrantSlots)
// needs and plain WriteRelationships cannot express: an atomic "first one in
// wins" bind, a grant write that refuses to land if the pin moved out from
// under it, and an atomic move that repoints the pin and revokes the old
// instance's grants in one RPC.
//
// EnsurePin deliberately bypasses RelationWriter.onWrite, the freshness-floor
// hook: it writes only the slot_pin tuple, and nothing ever Checks that
// relation (see authz.SlotPinRelation — it exists to be matched by this file's
// own MUST_MATCH / MUST_NOT_MATCH preconditions, not by a permission Check), so
// there is no floor for a later check to need advanced.
//
// WriteGrantsPinned and MovePin are the opposite case and DO advance the floor
// (via RelationWriter.advanceFloor), because they write — or revoke — the
// slot_grant_* tuples a permission Check DOES resolve. A single-occupancy grant
// that skipped the floor would reopen the approve-then-stale-denied race the
// floor exists to close, for the DEFAULT bind path, exactly as an unpinned grant
// written through plain WriteRelationships would.

import (
	"context"
	"errors"
	"fmt"
	"io"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// pinSubjectType mirrors slotGrantSubjectType (slot_grants.go): the pin's
// subject, like the grant's, is always the session.
const pinSubjectType = "agentsession"

// pinFilter builds the RelationshipFilter matching slot_pin tuples for
// resourceType held by scope. resourceID empty matches ANY instance of the
// type (used by EnsurePin's MUST_NOT_MATCH and the conflict/advisory read);
// non-empty narrows to exactly one (used by the MUST_MATCH guards).
func pinFilter(resourceType, resourceID string, scope authz.SessionRef) *v1.RelationshipFilter {
	return &v1.RelationshipFilter{
		ResourceType:       resourceType,
		OptionalResourceId: resourceID,
		OptionalRelation:   authz.SlotPinRelationName,
		OptionalSubjectFilter: &v1.SubjectFilter{
			SubjectType:       pinSubjectType,
			OptionalSubjectId: scope.String(),
		},
	}
}

// mustMatchPin is the MUST_MATCH precondition that resourceType:pinnedID is
// still the pin agentsession:<scope> holds. Shared by WriteGrantsPinned and
// MovePin: both guard against the pin having moved since the caller last
// read it.
func mustMatchPin(resourceType, pinnedID string, scope authz.SessionRef) *v1.Precondition {
	return &v1.Precondition{
		Operation: v1.Precondition_OPERATION_MUST_MATCH,
		Filter:    pinFilter(resourceType, pinnedID, scope),
	}
}

// EnsurePin implements authz.SlotPinner. It issues one TOUCH of the pin
// tuple, guarded by a MUST_NOT_MATCH precondition that the session holds no
// slot_pin on ANY instance of resourceType yet (resource id left unset on
// the filter — "no instance", not "not this one"). SpiceDB evaluates the
// precondition and the write atomically, so whichever of two racing binds
// reaches SpiceDB first wins the slot and the other is refused by the
// server, not by a read-then-write race in this process.
func (w *RelationWriter) EnsurePin(ctx context.Context, resourceType, resourceID string, scope authz.SessionRef) (held bool, pinnedID string, err error) {
	pin := authz.SlotPinRelation(resourceType, resourceID, scope)
	_, err = w.c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation:    v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: relationshipFor(pin),
		}},
		OptionalPreconditions: []*v1.Precondition{{
			Operation: v1.Precondition_OPERATION_MUST_NOT_MATCH,
			Filter:    pinFilter(resourceType, "", scope),
		}},
	})
	if err == nil {
		return false, "", nil
	}
	if status.Code(err) != codes.FailedPrecondition {
		return false, "", fmt.Errorf("ensure pin %s:%s for %s: %w", resourceType, resourceID, scope, err)
	}

	// The precondition fired: some instance is already pinned. Read back
	// which one, fully consistent — this process must resolve exactly what
	// SpiceDB just told it is there, not an eventually-consistent guess.
	found, rerr := w.ReadPin(ctx, resourceType, scope)
	if rerr != nil {
		return false, "", fmt.Errorf("ensure pin %s:%s for %s: read back pinned instance: %w", resourceType, resourceID, scope, rerr)
	}
	if found == "" {
		// A reported conflict with no pin found on read-back is a state this
		// code cannot explain (the schema says single-occupancy; the server
		// just said something matched). Fail closed rather than silently
		// report "unpinned" on the strength of a result that contradicts the
		// write that produced it.
		return false, "", fmt.Errorf("ensure pin %s:%s for %s: write reported a conflicting pin but the fully-consistent read found none", resourceType, resourceID, scope)
	}
	return true, found, nil
}

// WriteGrantsPinned implements authz.SlotPinner. rels are TOUCHed in the
// same RPC as a MUST_MATCH precondition that resourceType:pinnedID is still
// the session's pin — the guard that stops a same-instance bind racing an
// approved MovePin from resurrecting the revoked instance's authority after
// the fact: if the pin moved between this caller deciding pinnedID and this
// write landing, the precondition fails and nothing is granted.
func (w *RelationWriter) WriteGrantsPinned(ctx context.Context, rels []authz.Relation, resourceType, pinnedID string, scope authz.SessionRef) error {
	if len(rels) == 0 {
		return nil
	}
	updates := make([]*v1.RelationshipUpdate, 0, len(rels))
	for _, r := range rels {
		updates = append(updates, &v1.RelationshipUpdate{
			Operation:    v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: relationshipFor(r),
		})
	}
	resp, err := w.c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates:               updates,
		OptionalPreconditions: []*v1.Precondition{mustMatchPin(resourceType, pinnedID, scope)},
	})
	if err == nil {
		// Advance the session's freshness floor to this grant write, keyed by the
		// granted rels' own resources, so the ToolCallAuthz check that immediately
		// follows an in-process pinned grant reads at-least-as-fresh as the grant
		// — the same read-your-writes guarantee the plain WriteRelationships path
		// gives an unpinned grant.
		w.advanceFloor(rels, resp)
		return nil
	}
	if status.Code(err) == codes.FailedPrecondition {
		return fmt.Errorf("write grants for %s:%s pinned by %s: %w", resourceType, pinnedID, scope, authz.ErrSlotPinned)
	}
	return fmt.Errorf("write grants for %s:%s pinned by %s: %w", resourceType, pinnedID, scope, err)
}

// MovePin implements authz.SlotPinner. One RPC: a MUST_MATCH precondition
// that fromID is still pinned, a DELETE of the fromID pin, a TOUCH of the
// toID pin, and a DELETE of every relation in revoke — atomic, so an
// approved move either fully lands (old pin gone, new pin held, old grants
// revoked) or changes nothing at all.
func (w *RelationWriter) MovePin(ctx context.Context, resourceType, fromID, toID string, revoke []authz.Relation, scope authz.SessionRef) error {
	fromPin := authz.SlotPinRelation(resourceType, fromID, scope)
	toPin := authz.SlotPinRelation(resourceType, toID, scope)
	updates := []*v1.RelationshipUpdate{
		{
			Operation:    v1.RelationshipUpdate_OPERATION_DELETE,
			Relationship: relationshipFor(fromPin),
		},
		{
			Operation:    v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: relationshipFor(toPin),
		},
	}
	for _, r := range revoke {
		updates = append(updates, &v1.RelationshipUpdate{
			Operation:    v1.RelationshipUpdate_OPERATION_DELETE,
			Relationship: relationshipFor(r),
		})
	}
	resp, err := w.c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates:               updates,
		OptionalPreconditions: []*v1.Precondition{mustMatchPin(resourceType, fromID, scope)},
	})
	if err == nil {
		// Advance the session's freshness floor for every resource this move
		// touched: the new and old pin instances, and each revoked grant's
		// resource. A Check on the displaced instance right after the move must
		// read at-least-as-fresh as the revoke (its grants are gone), and one on
		// the new instance must see the pin flip — the same read-your-writes floor
		// WriteGrantsPinned advances for the grant it then writes on toID. The pin
		// relation itself is never Checked, but its resource id IS the instance a
		// later grant Check keys on, so advancing the floor for it is harmless and
		// keeps the affected-resource set complete.
		floored := make([]authz.Relation, 0, len(revoke)+2)
		floored = append(floored, toPin, fromPin)
		floored = append(floored, revoke...)
		w.advanceFloor(floored, resp)
		return nil
	}
	if status.Code(err) == codes.FailedPrecondition {
		return fmt.Errorf("move pin %s:%s -> %s:%s for %s: %w", resourceType, fromID, resourceType, toID, scope, authz.ErrSlotPinned)
	}
	return fmt.Errorf("move pin %s:%s -> %s:%s for %s: %w", resourceType, fromID, resourceType, toID, scope, err)
}

// ReadPin implements authz.SlotPinner. Fully consistent, since both of its
// callers need the current truth: EnsurePin's post-conflict resolution, and
// any caller using this directly for advisory purposes (card rendering,
// PriorID derivation) that would rather pay the consistency cost than render
// a pin that was just moved. Returns ("", nil) for zero pins — that is the
// ordinary "nothing bound yet" answer here, unlike EnsurePin's stricter
// zero-after-conflict case. Two or more pins is never valid for a
// single-occupancy slot; rather than pick one, this fails closed.
func (w *RelationWriter) ReadPin(ctx context.Context, resourceType string, scope authz.SessionRef) (string, error) {
	stream, err := w.c.cl.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency:        &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: pinFilter(resourceType, "", scope),
	})
	if err != nil {
		return "", fmt.Errorf("read pin %s for %s: %w", resourceType, scope, err)
	}
	var ids []string
	for {
		msg, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return "", fmt.Errorf("read pin %s for %s: %w", resourceType, scope, rerr)
		}
		ids = append(ids, msg.GetRelationship().GetResource().GetObjectId())
	}
	switch len(ids) {
	case 0:
		return "", nil
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("read pin %s for %s: found %d pins, expected at most one: %v", resourceType, scope, len(ids), ids)
	}
}

// ListGrantsFor implements authz.SlotPinner. It reads back the session's
// slot-grant tuples on ONE instance — a server-side filter on
// (resourceType, resourceID, subject=agentsession:<scope>), narrowed
// client-side to the slot_grant_ relations the way ListSlotGrants/ListSlotPins
// narrow their own subject-only sweeps (a per-permission relation name cannot
// be expressed server-side). The tuples are returned verbatim, not rebuilt
// from a permission list, so the caller revokes exactly what the instance
// holds.
//
// Fully consistent, for the same reason ReadPin is: its one caller is about to
// repoint a single-occupancy pin and revoke the displaced instance's grants,
// and a stale read here would either leave a just-written grant behind or
// delete one already gone.
func (w *RelationWriter) ListGrantsFor(ctx context.Context, resourceType, resourceID string, scope authz.SessionRef) ([]authz.Relation, error) {
	stream, err := w.c.cl.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       resourceType,
			OptionalResourceId: resourceID,
			OptionalSubjectFilter: &v1.SubjectFilter{
				SubjectType:       pinSubjectType,
				OptionalSubjectId: scope.String(),
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("list grants for %s:%s pinned by %s: %w", resourceType, resourceID, scope, err)
	}
	var out []authz.Relation
	for {
		msg, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("list grants for %s:%s pinned by %s: %w", resourceType, resourceID, scope, rerr)
		}
		rel := msg.GetRelationship()
		if !isSlotGrantRelation(rel.GetRelation()) {
			// The session is the subject of the instance's other relations too;
			// only the per-permission slot grants are revocable here.
			continue
		}
		res := rel.GetResource()
		subj := rel.GetSubject().GetObject()
		out = append(out, authz.Relation{
			ResourceType: res.GetObjectType(),
			ResourceID:   res.GetObjectId(),
			Relation:     rel.GetRelation(),
			SubjectType:  subj.GetObjectType(),
			SubjectID:    subj.GetObjectId(),
		})
	}
	return out, nil
}

var _ authz.SlotPinner = (*RelationWriter)(nil)
