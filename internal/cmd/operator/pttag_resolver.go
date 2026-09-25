package main

import (
	"context"

	spicedbv1 "github.com/authzed/authzed-go/proto/authzed/api/v1"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// newPtTagResolver builds the component that gates POST /memory/_pttag_resolve —
// the entitlement half of the content-resolve route. The route returns a
// datum's bytes, so unlike the mint (which writes provenance) and the verify
// (which confirms ids) it MUST gate on who may receive them.
//
// Entitlement is pt_tag:<tagID>#access for the caller's own session — the
// schema's `session + session->ancestor + granted_to`, the same permission the
// runner's derive path checks per input and the same one a data-slot binding
// writes (granted_to). Checked here again, operator-side, because a route that
// returns content cannot trust the caller to have filtered: the resolve handler
// reads pt_tag_content component-side, so without this check it would be a
// read-back channel for a kind deliberately kept out of a session's reach.
//
// FullyConsistent, not MinimizeLatency: a subagent's granted_to binding is
// written moments before its child runner starts and resolves its slots, so a
// stale revision would deny the child the datum its parent just bound. The
// derive path can tolerate MinimizeLatency (its tags were minted turns earlier);
// this path cannot.
func newPtTagResolver(spdb ptTagAccessSpiceDB) *ptTagResolver {
	return &ptTagResolver{spdb: spdb}
}

// ptTagAccessSpiceDB is the one SpiceDB capability the resolver needs.
type ptTagAccessSpiceDB interface {
	CheckPermission(ctx context.Context, in *spicedbv1.CheckPermissionRequest) (*spicedbv1.CheckPermissionResponse, error)
}

type ptTagResolver struct{ spdb ptTagAccessSpiceDB }

func (r *ptTagResolver) HasTagAccess(ctx context.Context, tagID string, session memory.NamespacedName) (bool, error) {
	resp, err := r.spdb.CheckPermission(ctx, &spicedbv1.CheckPermissionRequest{
		Resource:   &spicedbv1.ObjectReference{ObjectType: "pt_tag", ObjectId: tagID},
		Permission: "access",
		Subject: &spicedbv1.SubjectReference{Object: &spicedbv1.ObjectReference{
			ObjectType: "agentsession", ObjectId: session.Namespace + "/" + session.Name,
		}},
		Consistency: &spicedbv1.Consistency{
			Requirement: &spicedbv1.Consistency_FullyConsistent{FullyConsistent: true},
		},
	})
	if err != nil {
		return false, err
	}
	return resp.GetPermissionship() == spicedbv1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION, nil
}
