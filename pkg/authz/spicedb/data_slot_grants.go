package spicedb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// isDataSlotGrant reports whether a relationship is a DATA slot grant.
//
// Data slots write `granted_to` on pt_tag, not a `slot_grant_<perm>` relation,
// so isSlotGrantRelation does not match them and ListSlotGrants drops them on
// the floor. That is why this exists rather than the two sharing one sweep:
// slotGrantsOfSession's subject-only filter already returns BOTH kinds, and
// only the client-side predicate separates them.
func isDataSlotGrant(resourceType, relation string) bool {
	return resourceType == authz.DataSlotResourceType && relation == authz.DataSlotRelation
}

// ListDataSlotGrants returns every tag bound into this session's data slots.
//
// The Slot NAME is not recoverable: the tuple records which tag a session may
// read, not which slot it was bound into, so the returned bindings carry TagID
// alone. That is sufficient for revocation, which is what this is for. A
// caller needing slot names reads them from the delegation that made them.
func (c *Client) ListDataSlotGrants(ctx context.Context, ns, name string) ([]authz.DataSlotBinding, error) {
	stream, err := c.cl.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency:        &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: slotGrantsOfSession(ns, name),
	})
	if err != nil {
		return nil, fmt.Errorf("read data slot grants %s/%s: %w", ns, name, err)
	}
	var out []authz.DataSlotBinding
	for {
		msg, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("read data slot grants stream %s/%s: %w", ns, name, rerr)
		}
		rel := msg.GetRelationship()
		if !isDataSlotGrant(rel.GetResource().GetObjectType(), rel.GetRelation()) {
			continue
		}
		out = append(out, authz.DataSlotBinding{TagID: rel.GetResource().GetObjectId()})
	}
	return out, nil
}

// DeleteDataSlotGrants removes every data slot grant held by this session.
//
// The data column's DeleteSlotGrants, needed for exactly the same reason and
// reachable by neither of the existing sweeps. A data slot grant has the
// session as its SUBJECT and a pt_tag as its resource, so
// DeleteAgentSessionRelationships — which filters agentsession as the RESOURCE
// — does not see it, and DeleteSlotGrants skips it because `granted_to` is not
// a slot_grant relation.
//
// Without this, a child that was handed data keeps `access` on those tags after
// it ends. The AgentSession CR is retained after completion, so the grant would
// keep resolving indefinitely — the same leak DeleteSlotGrants exists to close
// on the authority side.
//
// Idempotent: succeeds when the session holds no data slots.
func (c *Client) DeleteDataSlotGrants(ctx context.Context, ns, name string) error {
	held, err := c.ListDataSlotGrants(ctx, ns, name)
	if err != nil {
		return fmt.Errorf("delete data slot grants %s/%s: %w", ns, name, err)
	}
	if len(held) == 0 {
		return nil
	}
	if err := authz.RevokeDataSlots(ctx, c.Relations(), authz.SessionRef{Namespace: ns, Name: name}, held); err != nil {
		return fmt.Errorf("delete data slot grants %s/%s: %w", ns, name, err)
	}
	return nil
}

// GrantDataSlots binds tags into this session's data slots. The
// session-scoped convenience over authz.GrantDataSlots, shaped like the
// client's other (ctx, ns, name, …) methods.
func (c *Client) GrantDataSlots(ctx context.Context, ns, name string, bindings []authz.DataSlotBinding, expiresAt time.Time) error {
	return authz.GrantDataSlots(ctx, c.Relations(), authz.SessionRef{Namespace: ns, Name: name}, bindings, expiresAt)
}
