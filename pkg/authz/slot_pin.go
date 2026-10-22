package authz

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// ErrSlotMoveDrifted marks an approved pin move whose MUST_MATCH failed: the
// pin no longer names the instance the card showed as displaced. Nothing moved
// and nothing was revoked; re-approving rebuilds the card from the current pin.
// Always wrapped together with ErrSlotPinned, so a caller that only knows the
// older sentinel still sees a pin refusal.
var ErrSlotMoveDrifted = errors.New("approved slot move found the pin had changed since the card was shown")

// ErrSlotMoveCommitted marks a BindApproved failure that happened AFTER an
// approved pin move landed: the pin now names the new instance and the
// displaced instance's grants are revoked, but the new grant was not written.
// errors.Is-able so the approval path can tell the approver what actually
// changed rather than reporting that nothing was granted.
var ErrSlotMoveCommitted = errors.New("approved slot move committed but the new grant was not written")

// ErrSlotStore marks a TRANSIENT STORE FAILURE inside GrantSlots — an EnsurePin,
// WriteGrantsPinned or plain-write that erred against SpiceDB — as distinct from
// an authorization ruling (ErrSlotPinned). The distinction is load-bearing in
// two places that would otherwise lie:
//
//   - A store blip gets errors.Join'd alongside a genuine ErrSlotPinned refusal
//     for a DIFFERENT type in the same call, so errors.Is(err, ErrSlotPinned)
//     turns true and the refusal would be reported as final ("re-approving will
//     not move it") when a retry would in fact bind the type that timed out.
//     Callers that route on the outcome walk the per-type SlotBindFailures
//     rather than testing the sentinels on the joined error, so the refusal
//     and the fault are each reported for their own type.
//   - The store cause carries raw endpoints and gRPC status text (dial tcp …,
//     Unavailable), which is operator detail, not something a model or a chat
//     reader can act on. User-facing surfaces quote only SlotPinRefusals and
//     leave the faults to the operator log.
//
// The raw cause is still wrapped (operators read it in logs); the sentinel only
// lets a reader tell "we could not reach the store" from "this is the answer".
var ErrSlotStore = errors.New("slot grant store failure")

// SlotFailureKind is what went wrong for one resource type inside GrantSlots.
type SlotFailureKind int

const (
	// SlotFailureRefused is an authorization RULING (wraps ErrSlotPinned): the
	// type is single-occupancy and committed elsewhere, or the call named two
	// instances of it. Its text is written for users and the model — it names the
	// pinned instance and the route out — and retrying will not change it.
	SlotFailureRefused SlotFailureKind = iota
	// SlotFailureFault is a fault on our side: a store error (wraps ErrSlotStore)
	// or a writer that cannot pin. Its text carries operator detail (endpoints,
	// gRPC status) and must stay in logs; a later attempt may succeed.
	SlotFailureFault
	// SlotFailureDrifted is an approved move whose MUST_MATCH failed because the
	// pin changed since the card was shown (wraps ErrSlotMoveDrifted). Nothing
	// moved and nothing was revoked.
	SlotFailureDrifted
	// SlotFailureMoveCommitted is an approved move that landed — the displaced
	// instance's grants are revoked — whose new grant then failed (wraps
	// ErrSlotMoveCommitted).
	SlotFailureMoveCommitted
)

// SlotBindFailure is ONE resource type's failed outcome inside GrantSlots.
// GrantSlots resolves each type independently and errors.Joins the failures, so
// a single call can carry a permanent refusal for one type beside a transient
// fault for another. The sentinels alone cannot tell a reader WHICH type each
// belongs to, or keep the fault's raw store text out of the refusal a user is
// shown; this type does both. Walk a returned error with SlotBindFailures.
//
// Error() is the wrapped cause's text unchanged, and Unwrap exposes it, so
// errors.Is against the sentinels keeps working for every existing reader.
type SlotBindFailure struct {
	// ResourceTypes the failure covers: exactly one for a single-occupancy
	// outcome; every multi-occupancy type in the call for a failed plain write.
	ResourceTypes []string
	// Rebind is the type's declared rebind mode ("" reads as the default,
	// "approval"). Meaningful for a refusal: it decides whether the route out is
	// an approved amendment or only a new session.
	Rebind string
	Kind   SlotFailureKind
	err    error
}

// NewSlotBindFailure builds one per-type failure. GrantSlots is the producer;
// this exists for writers and test doubles that stand in for it.
func NewSlotBindFailure(kind SlotFailureKind, rebind string, err error, resourceTypes ...string) *SlotBindFailure {
	return &SlotBindFailure{ResourceTypes: resourceTypes, Rebind: rebind, Kind: kind, err: err}
}

func (f *SlotBindFailure) Error() string {
	if f.err == nil {
		return fmt.Sprintf("slot bind failure on %s", strings.Join(f.ResourceTypes, ", "))
	}
	return f.err.Error()
}
func (f *SlotBindFailure) Unwrap() error { return f.err }

// SlotBindFailures returns every per-type failure in err's tree, in the order
// GrantSlots recorded them. Empty when err is nil or carries none (an error
// raised before GrantSlots ran, such as a session_scope write).
func SlotBindFailures(err error) []*SlotBindFailure {
	var out []*SlotBindFailure
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if f, ok := e.(*SlotBindFailure); ok {
			// A failure is a leaf for this walk: its own cause chain can hold an
			// ErrSlotPinned from the store (a drifted move) that is not a second
			// failure.
			out = append(out, f)
			return
		}
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			for _, c := range u.Unwrap() {
				walk(c)
			}
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		}
	}
	walk(err)
	return out
}

// SlotPinRefusals returns only the refusals (SlotFailureRefused) in err's tree:
// the part of a GrantSlots error that is a final answer a user or model can act
// on. Faults beside them stay out, so their raw store text never reaches a
// user-facing surface and they never mask a refusal.
func SlotPinRefusals(err error) []*SlotBindFailure {
	var out []*SlotBindFailure
	for _, f := range SlotBindFailures(err) {
		if f.Kind == SlotFailureRefused {
			out = append(out, f)
		}
	}
	return out
}

// SlotRefusalText renders refusals for a user- or model-facing surface: each
// refusal's own text, one per line. Never pass a whole GrantSlots error to such
// a surface — it may carry a fault's store detail beside the refusals.
func SlotRefusalText(refusals []*SlotBindFailure) string {
	parts := make([]string, 0, len(refusals))
	for _, r := range refusals {
		parts = append(parts, r.Error())
	}
	return strings.Join(parts, "\n")
}

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
	// relations (revoke) in the same RPC.
	//
	// IDEMPOTENT at the target: if the precondition fails because the pin
	// ALREADY names toID, the move is a no-op and returns nil, not an error. A
	// plan-scoped approval resolves one BindApproved call per covered phase and
	// each re-attempts the SAME move, so the first lands it and the rest find it
	// already landed — without this they would drift-fail and drop their grant.
	// The displaced instance's grants were revoked by whoever performed the real
	// move; a no-op does not re-revoke (a straggler lapses via the grant's
	// mandatory expiry, the same inherent window a real move leaves). A failed
	// precondition where the pin is NEITHER fromID nor toID is genuine drift and
	// returns an error wrapping ErrSlotPinned.
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
