package spicedb

import (
	"context"
	"errors"
	"fmt"
	"io"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory/pools"
)

// PoolsReader adapts *Client to pools.RelationshipReader.
//
// A separate type rather than a method directly on *Client because *Client
// already has a ReadRelationships method (the raw v1 pass-through at
// client.go:392, used by relsync and the guardian drift check) — Go has no
// method overloading, so pools.RelationshipReader's narrower signature needs
// its own receiver. Mirrors RelationWriter's shape: *Client stays the single
// connection, and a thin wrapper carries the interface-specific method set.
type PoolsReader struct{ c *Client }

// Pools returns the client's pools.RelationshipReader view, for wiring into
// pools.ForSession without pulling pools' dependency onto *Client itself.
func (c *Client) Pools() pools.RelationshipReader { return &PoolsReader{c: c} }

// ReadRelationships reads every relationship naming f.SubjectType/f.SubjectID
// as subject, across every resource type and relation — pools.ForSession
// itself narrows to slot_grant_* afterward, the same division of labor
// slotGrantsOfSession's callers already use (resource type and relation left
// unfiltered here; the client-side predicate does the rest).
//
// FULLY CONSISTENT: this drives which memory pools a session may reach, and a
// stale read here could hide a grant a human just approved or keep serving one
// just revoked.
//
// Drains the ENTIRE stream to io.EOF with no OptionalLimit, exactly like
// ListSlotGrants and ListAuthorizedTokens above — ReadRelationships is a
// server-streaming RPC, not a paged list call, so the complete result set IS
// what draining the stream to EOF returns. Setting OptionalLimit would be the
// thing that truncates; not setting it is what keeps this read complete. See
// pools.ForSession's godoc: a resource silently missing from this result is
// an authorization narrowing nothing else would report.
func (r *PoolsReader) ReadRelationships(ctx context.Context, f pools.RelFilter) ([]authz.Relation, error) {
	stream, err := r.c.cl.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: &v1.RelationshipFilter{
			OptionalSubjectFilter: &v1.SubjectFilter{
				SubjectType:       f.SubjectType,
				OptionalSubjectId: f.SubjectID,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("read relationships for %s:%s: %w", f.SubjectType, f.SubjectID, err)
	}
	var out []authz.Relation
	for {
		msg, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("read relationships stream for %s:%s: %w", f.SubjectType, f.SubjectID, rerr)
		}
		rel := msg.GetRelationship()
		res := rel.GetResource()
		subj := rel.GetSubject()
		out = append(out, authz.Relation{
			ResourceType: res.GetObjectType(),
			ResourceID:   res.GetObjectId(),
			Relation:     rel.GetRelation(),
			SubjectType:  subj.GetObject().GetObjectType(),
			SubjectID:    subj.GetObject().GetObjectId(),
			SubjectRel:   subj.GetOptionalRelation(),
		})
	}
	return out, nil
}

var _ pools.RelationshipReader = (*PoolsReader)(nil)
