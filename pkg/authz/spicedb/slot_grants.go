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

// DeleteSlotGrants removes every slot grant held by this session.
//
// The reciprocal of DeleteAgentSessionRelationships, and NOT covered by it: a
// slot grant has the session as its SUBJECT, while that call filters
// agentsession as the RESOURCE. Without this, every session that bound an
// instance leaves live authority behind on an external resource — the
// AgentSession CR is retained after completion, so slot_grant->interact keeps
// resolving indefinitely.
//
// Idempotent: succeeds when the session holds no grants.
func (c *Client) DeleteSlotGrants(ctx context.Context, ns, name string) error {
	// Listed then deleted, rather than swept by one filter. A subject-only
	// filter would also match the session's NON-slot relations, and
	// per-permission relation names cannot be expressed as one server-side
	// filter — so the read is what identifies the slot grants, and the delete
	// names exactly those.
	held, err := c.ListSlotGrants(ctx, ns, name)
	if err != nil {
		return fmt.Errorf("delete slot grants %s/%s: %w", ns, name, err)
	}
	if len(held) == 0 {
		return nil
	}
	if err := authz.RevokeSlots(ctx, c.Relations(), authz.SessionRef{Namespace: ns, Name: name}, held); err != nil {
		return fmt.Errorf("delete slot grants %s/%s: %w", ns, name, err)
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
	// Advance the caller's freshness floor to this write. Only after success, and
	// only with a real token — an empty one would be a no-op the callee ignores
	// anyway, but not calling keeps the contract ("onWrite means a durable
	// write") honest.
	if w.onWrite != nil {
		if tok := resp.GetWrittenAt().GetToken(); tok != "" {
			w.onWrite(rels, tok)
		}
	}
	return nil
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
