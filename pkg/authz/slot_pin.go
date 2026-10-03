package authz

import (
	"context"
	"errors"
)

// SlotPinRelationName is the relation a single-occupancy slot writes to record
// the one instance that occupies it for a session's life. The tuple points
// resource -> session, the same direction as slot_grant:
//
//	<resourceType>:<resourceID>#slot_pin@agentsession:<ns>/<name>
//
// Unlike slot_grant, it carries NO expiration: a grant carries authority and
// must expire; the pin carries the identity of the commitment and must not.
const SlotPinRelationName = "slot_pin"

// SlotPinRelation builds the pin tuple binding one instance to one session.
func SlotPinRelation(resourceType, resourceID string, scope SessionRef) Relation {
	return Relation{
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Relation:     SlotPinRelationName,
		SubjectType:  "agentsession",
		SubjectID:    scope.String(),
	}
}

// ErrSlotPinned marks a refusal to bind a DIFFERENT instance to a filled
// single-occupancy slot. errors.Is-able so a caller can tell this
// authorization ruling from a store failure. Messages are user-visible —
// they become text the model reads — so they name the pinned instance and
// the route out.
var ErrSlotPinned = errors.New("slot already pinned to a different instance")

// SlotPinner is the precondition-capable surface the single-occupancy gate
// needs and plain RelWriter cannot express. A non-nil RelWriter that does not
// also implement it cannot bind a single-occupancy slot: the gate refuses
// rather than writing through as if unmarked.
type SlotPinner interface {
	// EnsurePin atomically writes the pin for resourceID unless the session
	// is already pinned to some instance of resourceType.
	//   held=false                        -> this call wrote the pin.
	//   held=true, pinnedID==resourceID   -> already pinned to the SAME instance.
	//   held=true, pinnedID!=resourceID   -> pinned to a DIFFERENT instance.
	// The conflict read is fully consistent and fails closed (error) if it
	// observes anything but exactly one pin.
	EnsurePin(ctx context.Context, resourceType, resourceID string, scope SessionRef) (held bool, pinnedID string, err error)

	// WriteGrantsPinned writes rels in one RPC guarded by MUST_MATCH that
	// pinnedID is still the pinned instance of resourceType for this session.
	// This is what stops a same-instance bind racing an approved move from
	// resurrecting the revoked instance's authority. A failed precondition
	// returns an error wrapping ErrSlotPinned.
	WriteGrantsPinned(ctx context.Context, rels []Relation, resourceType, pinnedID string, scope SessionRef) error

	// MovePin atomically repoints the pin fromID -> toID, guarded by
	// MUST_MATCH that fromID is still pinned, deleting fromID's grant
	// relations (revoke) in the same RPC. A failed precondition returns an
	// error wrapping ErrSlotPinned.
	MovePin(ctx context.Context, resourceType, fromID, toID string, revoke []Relation, scope SessionRef) error

	// ReadPin returns the currently pinned instance for resourceType, or ""
	// when none. Advisory (card rendering, PriorID derivation): the MUST_MATCH
	// preconditions above, not this read, are what make writes safe.
	ReadPin(ctx context.Context, resourceType string, scope SessionRef) (string, error)

	// ListGrantsFor returns the session's slot-grant tuples on ONE instance —
	// every slot_grant_<permission> relation naming resourceType:resourceID with
	// this session as subject — so a MovePin can revoke exactly the prior
	// instance's grants in the same RPC that repoints the pin. Returned as the
	// tuples themselves (read back, not rebuilt from a permission list), which
	// is what keeps the revoke set the instance's ACTUAL grants regardless of
	// which permissions the moving approval happens to name — the leak that a
	// permission-list reconstruction would reopen when the two instances' grant
	// sets differ. Returns no tuples (not an error) when the instance holds
	// none.
	ListGrantsFor(ctx context.Context, resourceType, resourceID string, scope SessionRef) ([]Relation, error)
}
