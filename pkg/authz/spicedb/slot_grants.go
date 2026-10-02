package spicedb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// slotGrantSubjectType is the subject side of every slot grant. The tuple
// points from the RESOURCE at the SESSION — see authz.SlotGrantRelation for
// why that direction is the substance of the mechanism.
const slotGrantSubjectType = "agentsession"

// slotGrantsOfSession filters every slot grant held by one session, across
// EVERY resource type.
//
// The resource type is deliberately left unset. SpiceDB requires only that a
// relationship filter have at least one field set (a subject-only filter is
// valid), so one RPC covers every type a session touched — including types
// whose AgentClass has since stopped declaring them, which is precisely the
// population a teardown keyed on the current declaration would miss.
// The RELATION is deliberately left unset too, not just the resource type.
// Slot-grant relations are per-permission (slot_grant_read, slot_grant_write,
// …), so filtering on one name would sweep only that permission's grants — and
// a session may hold grants for permissions its class no longer declares. The
// prefix filter happens client-side in ListSlotGrants; DeleteSlotGrants cannot
// express it server-side and is handled separately.
func slotGrantsOfSession(ns, name string) *v1.RelationshipFilter {
	return &v1.RelationshipFilter{
		OptionalSubjectFilter: &v1.SubjectFilter{
			SubjectType:       slotGrantSubjectType,
			OptionalSubjectId: ns + "/" + name,
		},
	}
}

// isSlotGrantRelation reports whether a relation name is a slot grant.
func isSlotGrantRelation(rel string) bool {
	return strings.HasPrefix(rel, authz.SlotGrantRelationPrefix)
}

// isSlotPinRelation reports whether a relation name is the slot pin relation.
// Unlike slot grants there is only one name to match — a pin is per TYPE, not
// per permission.
func isSlotPinRelation(rel string) bool {
	return rel == authz.SlotPinRelationName
}

// permissionOfSlotGrant recovers the permission a slot-grant relation carries.
func permissionOfSlotGrant(rel string) string {
	return strings.TrimPrefix(rel, authz.SlotGrantRelationPrefix)
}

// ListSlotGrants returns every instance bound into a slot for this session.
//
// Fully consistent: the caller is deciding what the agent may reach, and a
// stale read here either hides a grant a human just approved or surfaces one
// just revoked.
func (c *Client) ListSlotGrants(ctx context.Context, ns, name string) ([]authz.SlotBinding, error) {
	stream, err := c.cl.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency:        &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: slotGrantsOfSession(ns, name),
	})
	if err != nil {
		return nil, fmt.Errorf("read slot grants %s/%s: %w", ns, name, err)
	}
	var out []authz.SlotBinding
	for {
		msg, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("read slot grants stream %s/%s: %w", ns, name, rerr)
		}
		rel := msg.GetRelationship()
		if !isSlotGrantRelation(rel.GetRelation()) {
			// The session is the subject of other relations too; only slot
			// grants belong here.
			continue
		}
		res := rel.GetResource()
		out = append(out, authz.SlotBinding{
			ResourceType: res.GetObjectType(),
			// PERMANENT, not provisional: this id was read back OUT of SpiceDB,
			// so it is already canonical — it was written through NewObjectID in
			// the first place. Unlike the other TrustedObjectID call sites this
			// task adds, there is no derivation path to wire in later; the
			// provenance IS the guarantee.
			ResourceID: authz.TrustedObjectID(res.GetObjectId()),
			Permission: permissionOfSlotGrant(rel.GetRelation()),
		})
	}
	return out, nil
}

// ListSlotPins returns every pin held by this session, verbatim.
//
// Mirrors ListSlotGrants's read shape exactly — one subject-only RPC, filtered
// client-side, for the same reason: a resource-type/relation filter cannot
// express "a slot_pin tuple naming agentsession:<ns>/<name> as its subject"
// any more precisely than the grant sweep can express its per-permission
// relation names server-side. Here the client-side predicate is
// isSlotPinRelation instead of the slot_grant_ prefix.
//
// Returned as plain Relation, not SlotBinding: a pin carries no permission and
// no occupancy to recover — it is the tuple itself that CopySlotGrants needs
// to copy byte-for-byte, re-targeting only the subject.
//
// Fully consistent, for the same reason as ListSlotGrants: a stale read here
// either hides a pin a write just moved or surfaces one that no longer holds.
func (c *Client) ListSlotPins(ctx context.Context, ns, name string) ([]authz.Relation, error) {
	stream, err := c.cl.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency:        &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: slotGrantsOfSession(ns, name),
	})
	if err != nil {
		return nil, fmt.Errorf("read slot pins %s/%s: %w", ns, name, err)
	}
	var out []authz.Relation
	for {
		msg, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("read slot pins stream %s/%s: %w", ns, name, rerr)
		}
		rel := msg.GetRelationship()
		if !isSlotPinRelation(rel.GetRelation()) {
			// The session is the subject of other relations too (slot grants,
			// the agentsession relations themselves); only the pin belongs here.
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

// CopySlotTuples writes rels verbatim in one batched TOUCH — no gate, and no
// expiry stamping of its own. The caller (authz.CopySlotGrants) has already
// re-targeted every relation's SubjectID at the child and, for a grant,
// stamped the child's expiry; a pin tuple carries none, exactly as EnsurePin
// writes it, so this never adds one.
func (c *Client) CopySlotTuples(ctx context.Context, rels []authz.Relation) error {
	if err := c.Relations().WriteRelationships(ctx, rels); err != nil {
		return fmt.Errorf("copy slot tuples: %w", err)
	}
	return nil
}

// DeleteSlotGrants removes every slot grant AND pin held by this session.
//
// The reciprocal of DeleteAgentSessionRelationships, and NOT covered by it: a
// slot grant (and a pin) has the session as its SUBJECT, while that call
// filters agentsession as the RESOURCE. Without this, every session that bound
// an instance leaves live authority behind on an external resource — the
// AgentSession CR is retained after completion, so slot_grant->interact keeps
// resolving indefinitely.
//
// Pins are included deliberately: unlike a grant, a pin carries no
// expiration, so a dropped pin-delete here is not a grant that simply lapses
// later — it is a PERMANENT leak. A session name that is reused after this one
// tears down would otherwise inherit a pin it never earned and be refused its
// own first bind (see the admission sweep in pkg/controllers/agentsession,
// which calls this same method for exactly that leftover).
//
// Two delete calls, not one: the grant delete routes through authz.RevokeSlots
// so the grant tuple is built by the sanctioned SlotGrantRelation call site
// (see slot_grant_guard_test.go — a hand-built slot_grant_/slot_pin tuple
// outside GrantSlots/RevokeSlots/the pinner trips that guard), while pins are
// already fully-built Relations straight from ListSlotPins and need no
// builder call at all. Both failures are returned, never swallowed — a
// dropped pin delete is the permanent leak above.
//
// Idempotent: succeeds when the session holds no grants or pins.
func (c *Client) DeleteSlotGrants(ctx context.Context, ns, name string) error {
	// Listed then deleted, rather than swept by one filter. A subject-only
	// filter would also match the session's NON-slot relations, and
	// per-permission relation names cannot be expressed as one server-side
	// filter — so the read is what identifies the slot grants and pins, and the
	// delete names exactly those.
	held, err := c.ListSlotGrants(ctx, ns, name)
	if err != nil {
		return fmt.Errorf("delete slot grants %s/%s: %w", ns, name, err)
	}
	pins, err := c.ListSlotPins(ctx, ns, name)
	if err != nil {
		return fmt.Errorf("delete slot grants %s/%s: list pins: %w", ns, name, err)
	}
	if len(held) == 0 && len(pins) == 0 {
		return nil
	}
	if len(held) > 0 {
		if err := authz.RevokeSlots(ctx, c.Relations(), authz.SessionRef{Namespace: ns, Name: name}, held); err != nil {
			return fmt.Errorf("delete slot grants %s/%s: %w", ns, name, err)
		}
	}
	if len(pins) > 0 {
		if err := c.Relations().DeleteRelationships(ctx, pins); err != nil {
			return fmt.Errorf("delete slot grants %s/%s: delete pins: %w", ns, name, err)
		}
	}
	return nil
}

// GrantSlots binds instances into this session's slots. The session-scoped
// convenience over authz.GrantSlots, shaped like the client's other
// (ctx, ns, name, …) methods so callers need not assemble a SessionRef.
func (c *Client) GrantSlots(ctx context.Context, ns, name string, bindings []authz.SlotBinding, expiresAt time.Time) error {
	return authz.GrantSlots(ctx, c.Relations(), authz.SessionRef{Namespace: ns, Name: name}, bindings, expiresAt)
}

// RelationWriter adapts *Client to authz.RelWriter, which speaks
// authz.Relation rather than the v1 request/response pair.
//
// A separate type rather than methods on Client because the names are already
// taken: Client.WriteRelationships / DeleteRelationships carry the raw v1
// signature that satisfies BootstrapWriter.
type RelationWriter struct {
	c *Client
	// onWrite, when set, is called after a successful WriteRelationships with the
	// relations written and the write's WrittenAt ZedToken. It exists so an
	// in-process caller (the runner) can advance its per-session freshness floor
	// to the write it just made, closing the read-your-writes race where the very
	// next permission check reads a snapshot that predates the grant. nil for
	// every caller that does not need it (Relations()).
	onWrite func(rels []authz.Relation, writtenAt string)
}

// Relations returns the client's authz.RelWriter view.
//
// Declared to return the interface, not *RelationWriter: pipeline.Authz
// declares Relations() authz.RelWriter, and Go's method-set matching is
// exact on return type, not covariant — a *RelationWriter-returning method
// would not satisfy that interface even though *RelationWriter itself
// implements authz.RelWriter (see the assertion at the bottom of this file).
func (c *Client) Relations() authz.RelWriter { return &RelationWriter{c: c} }

// RelationsWithFloor is Relations() plus a freshness-floor hook: after every
// successful write, onWrite receives the relations written and the SpiceDB
// WrittenAt ZedToken, so a caller holding a per-session ZedToken cache can Set
// that token as the at-least-as-fresh floor for the written resources. The
// runner wires this into its SlotBinder so a plan-gate approval's in-process
// slot-grant write is visible to the ToolCallAuthz check that immediately
// follows it — see Loop.AdvanceAuthzFloor.
func (c *Client) RelationsWithFloor(onWrite func(rels []authz.Relation, writtenAt string)) authz.RelWriter {
	return &RelationWriter{c: c, onWrite: onWrite}
}

// WriteRelationships TOUCHes every tuple in one batched, atomic RPC, so a
// caller that has decided on a set of grants either gets all of them or none.
func (w *RelationWriter) WriteRelationships(ctx context.Context, rels []authz.Relation) error {
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
	resp, err := w.c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{Updates: updates})
	if err != nil {
		return fmt.Errorf("write relationships: %w", err)
	}
	w.advanceFloor(rels, resp)
	return nil
}

// advanceFloor invokes the freshness-floor hook for a SUCCESSFUL write, when one
// is wired. Nil-safe, and only fires with a real WrittenAt token — an empty one
// is a no-op the callee ignores anyway, but not calling keeps the contract
// ("onWrite means a durable write") honest.
//
// Shared by EVERY write on this RelationWriter that must advance the floor —
// the plain batched WriteRelationships here AND the pinned-grant / move writes
// in slot_pin.go — so a single-occupancy grant cannot silently skip the
// read-your-writes floor advance the plain path gets (the approve-then-stale-
// denied race the floor closes applies to pinned grants exactly as it does to
// unpinned ones). The one write that deliberately does NOT call this is
// EnsurePin: nothing ever Checks the slot_pin relation, so there is no floor a
// later check could need advanced.
func (w *RelationWriter) advanceFloor(rels []authz.Relation, resp *v1.WriteRelationshipsResponse) {
	if w.onWrite == nil {
		return
	}
	if tok := resp.GetWrittenAt().GetToken(); tok != "" {
		w.onWrite(rels, tok)
	}
}

// DeleteRelationships removes exactly the named tuples in one atomic RPC.
//
// Per-tuple OPERATION_DELETE rather than a filter sweep: the caller named the
// tuples, and a filter broad enough to express them would also match grants it
// did not name. Deleting an absent tuple is a no-op, so this is idempotent.
func (w *RelationWriter) DeleteRelationships(ctx context.Context, rels []authz.Relation) error {
	if len(rels) == 0 {
		return nil
	}
	updates := make([]*v1.RelationshipUpdate, 0, len(rels))
	for _, r := range rels {
		updates = append(updates, &v1.RelationshipUpdate{
			Operation:    v1.RelationshipUpdate_OPERATION_DELETE,
			Relationship: relationshipFor(r),
		})
	}
	if _, err := w.c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{Updates: updates}); err != nil {
		return fmt.Errorf("delete relationships: %w", err)
	}
	return nil
}

// relationshipFor renders one authz.Relation as a v1.Relationship.
func relationshipFor(r authz.Relation) *v1.Relationship {
	subject := &v1.SubjectReference{
		Object: &v1.ObjectReference{ObjectType: r.SubjectType, ObjectId: r.SubjectID},
	}
	if r.SubjectRel != "" {
		subject.OptionalRelation = r.SubjectRel
	}
	rel := &v1.Relationship{
		Resource: &v1.ObjectReference{ObjectType: r.ResourceType, ObjectId: r.ResourceID},
		Relation: r.Relation,
		Subject:  subject,
	}
	if !r.ExpiresAt.IsZero() {
		rel.OptionalExpiresAt = timestamppb.New(r.ExpiresAt)
	}
	return rel
}

var _ authz.RelWriter = (*RelationWriter)(nil)

// *Client is the fork path's SlotGrantCopier: ListSlotGrants + ListSlotPins +
// CopySlotTuples.
var _ authz.SlotGrantCopier = (*Client)(nil)
