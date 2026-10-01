package spicedb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/accesstoken"
)

// wildcardSubjectID is the SpiceDB object id for a wildcard subject
// (`agentclass:*`), matching the ObjectId: "*" shape SyncArtifactOrgViewer
// already uses for `user:*` — a plain wildcard object id, not a typed
// relation-set wildcard.
const wildcardSubjectID = "*"

// AccessTokenGrant is the complete authorization shape of one access token:
// who it acts as, what role ladder it climbed, and which agentclasses it may
// touch (or Unfiltered for every class). WriteAccessTokenGrant and
// ReadAccessTokenGrant are the write/read halves of this same shape.
type AccessTokenGrant struct {
	TokenID      string
	Owner        identity.CanonicalUserID
	Role         string   // accesstoken.Role*
	ScopeClasses []string // agentclass object ids "ns/name"; ignored when Unfiltered
	Unfiltered   bool
	ExpiresAt    time.Time
}

// WriteAccessTokenGrant writes the token's complete tuple set in ONE
// WriteRelationships call: the role tuple (subject = owner) plus either the
// wildcard scope tuple or one scope tuple per class — all carrying the
// expiration trait, so an expired token fails every check with no cleanup
// job required. TOUCH keeps re-mints idempotent.
//
// TOUCH cuts the other way for a ROLE CHANGE: re-minting the SAME TokenID
// with a DIFFERENT role TOUCHes a second role relation alongside the first,
// and the ladder then grants the UNION of both roles. Callers must always
// use a fresh TokenID (the mint path does) or call DeleteAccessTokenTuples
// first — never re-grant an existing id with a new role.
func (c *Client) WriteAccessTokenGrant(ctx context.Context, g AccessTokenGrant) error {
	if g.TokenID == "" || g.Owner.IsZero() {
		return fmt.Errorf("access token grant for %q: requires token id and owner", g.TokenID)
	}
	if g.ExpiresAt.IsZero() {
		return fmt.Errorf("access token grant for %q: requires an expiry", g.TokenID)
	}
	roleRel, err := accesstoken.RoleRelation(g.Role)
	if err != nil {
		return fmt.Errorf("access token grant for %q: %w", g.TokenID, err)
	}
	if !g.Unfiltered && len(g.ScopeClasses) == 0 {
		return fmt.Errorf("access token grant for %q: requires scope classes or Unfiltered", g.TokenID)
	}

	expires := timestamppb.New(g.ExpiresAt)
	tokenRes := &v1.ObjectReference{ObjectType: accesstoken.ObjectType, ObjectId: g.TokenID}

	updates := []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{
			Resource:          tokenRes,
			Relation:          roleRel,
			Subject:           &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: g.Owner.String()}},
			OptionalExpiresAt: expires,
		},
	}}

	scopeSubjectIDs := g.ScopeClasses
	if g.Unfiltered {
		scopeSubjectIDs = []string{wildcardSubjectID}
	}
	for _, classID := range scopeSubjectIDs {
		updates = append(updates, &v1.RelationshipUpdate{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource:          tokenRes,
				Relation:          accesstoken.RelationScopeClass,
				Subject:           &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "agentclass", ObjectId: classID}},
				OptionalExpiresAt: expires,
			},
		})
	}

	if _, err := c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{Updates: updates}); err != nil {
		return fmt.Errorf("write access token grant for %s: %w", g.TokenID, err)
	}
	return nil
}

// DeleteAccessTokenTuples removes every relationship on the token resource —
// revocation, with the same "revoked on the next fully-consistent check"
// semantics externaltoken uses elsewhere in this package. Idempotent:
// deleting an absent token's tuples is a no-op.
func (c *Client) DeleteAccessTokenTuples(ctx context.Context, tokenID string) error {
	if tokenID == "" {
		return fmt.Errorf("delete access token tuples: empty token id")
	}
	if _, err := c.cl.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       accesstoken.ObjectType,
			OptionalResourceId: tokenID,
		},
	}); err != nil {
		return fmt.Errorf("delete access token tuples for %s: %w", tokenID, err)
	}
	return nil
}

// AccessTokenCheck is one authorized operation attempted through an access
// token: the token being presented, who it claims to act as, the permission
// the caller wants, and the resource (plus its owning agentclass) the
// permission is being checked against.
type AccessTokenCheck struct {
	TokenID      string
	Owner        identity.CanonicalUserID
	Permission   string // the $sameperm name, e.g. "read_transcript"
	ResourceType string // "agentsession" | "artifact" | "agentclass" | "memory_entry"
	ResourceID   string
	ClassID      string // agentclass object id "ns/name" for leg 2
}

// AccessTokenDecision is the three-legged intersection CheckAccessTokenOp
// answers: does the token itself grant this permission (its role mirror),
// does its scope cover the resource's class, and does the owner it acts as
// actually hold the permission on the resource. All three must hold — a
// token narrows its owner's standing, it never widens it.
type AccessTokenDecision struct{ TokenGrants, ScopeCovers, OwnerHas bool }

// Allowed reports whether all three legs held.
func (d AccessTokenDecision) Allowed() bool { return d.TokenGrants && d.ScopeCovers && d.OwnerHas }

// CheckAccessTokenOp runs the three-legged intersection as one
// CheckBulkPermissions round trip. Responses are matched by echoed request
// identity (resource type/id + permission), never by index; anything
// unmatched or errored stays false — deny-on-mangle, the spicedbauthorizer
// discipline (pkg/memory/spicedbauthorizer/authorizer.go's filterAllowed).
func (c *Client) CheckAccessTokenOp(ctx context.Context, chk AccessTokenCheck, fullyConsistent bool) (AccessTokenDecision, error) {
	var dec AccessTokenDecision
	if chk.TokenID == "" || chk.Owner.IsZero() || chk.Permission == "" ||
		chk.ResourceType == "" || chk.ResourceID == "" || chk.ClassID == "" {
		return dec, fmt.Errorf("access token check for %q: requires token, owner, permission, resource, and class", chk.TokenID)
	}
	// Leg 1 and leg 2 are told apart below by permission name alone (both
	// address the token resource), so a check FOR "covers" itself would be
	// attributed to the wrong leg. "owner" is likewise an internal permission,
	// never a $sameperm mirror. Refuse both by intent rather than letting the
	// switch mis-attribute them by coincidence.
	if chk.Permission == accesstoken.PermissionCovers || chk.Permission == accesstoken.PermissionOwner {
		return dec, fmt.Errorf("access token check for %q: permission %q is internal to the token definition, not a checkable mirror", chk.TokenID, chk.Permission)
	}

	ownerSubj := &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: chk.Owner.String()}}
	items := []*v1.CheckBulkPermissionsRequestItem{
		{
			Resource:   &v1.ObjectReference{ObjectType: accesstoken.ObjectType, ObjectId: chk.TokenID},
			Permission: chk.Permission,
			Subject:    ownerSubj,
		},
		{
			Resource:   &v1.ObjectReference{ObjectType: accesstoken.ObjectType, ObjectId: chk.TokenID},
			Permission: accesstoken.PermissionCovers,
			Subject:    &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "agentclass", ObjectId: chk.ClassID}},
		},
		{
			Resource:   &v1.ObjectReference{ObjectType: chk.ResourceType, ObjectId: chk.ResourceID},
			Permission: chk.Permission,
			Subject:    ownerSubj,
		},
	}

	resp, err := c.cl.CheckBulkPermissions(ctx, &v1.CheckBulkPermissionsRequest{
		Consistency: consistencyFor(fullyConsistent),
		Items:       items,
	})
	if err != nil {
		return dec, fmt.Errorf("access token bulk check for %s: %w", chk.TokenID, err)
	}

	for _, pair := range resp.GetPairs() {
		req := pair.GetRequest()
		if perr := pair.GetError(); perr != nil {
			log.FromContext(ctx).Info("access token bulk check pair errored; leg denied",
				"tokenID", chk.TokenID, "resourceType", req.GetResource().GetObjectType(),
				"permission", req.GetPermission(), "code", perr.GetCode(), "err", perr.GetMessage())
			continue
		}
		item := pair.GetItem()
		if item == nil || item.GetPermissionship() != v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION {
			continue
		}
		switch {
		case req.GetResource().GetObjectType() == accesstoken.ObjectType &&
			req.GetResource().GetObjectId() == chk.TokenID &&
			req.GetPermission() == accesstoken.PermissionCovers:
			dec.ScopeCovers = true
		case req.GetResource().GetObjectType() == accesstoken.ObjectType &&
			req.GetResource().GetObjectId() == chk.TokenID:
			dec.TokenGrants = true
		case req.GetResource().GetObjectType() == chk.ResourceType &&
			req.GetResource().GetObjectId() == chk.ResourceID:
			dec.OwnerHas = true
		}
	}
	return dec, nil
}

// CheckAccessTokenMirror answers leg 1 alone (token#<permission>@user:<owner>)
// — used by enumeration ops (list_sessions) that filter legs 2/3 per result
// rather than per-call via CheckAccessTokenOp.
func (c *Client) CheckAccessTokenMirror(ctx context.Context, tokenID string, owner identity.CanonicalUserID, permission string, fullyConsistent bool) (bool, error) {
	return c.CheckOnResource(ctx, accesstoken.ObjectType, tokenID, permission, owner, fullyConsistent)
}

// FilterAccessTokenCoveredClasses bulk-answers leg 2 (token#covers@agentclass)
// for a class set in one round trip. Results are matched by the subject
// object id echoed on each pair's request, never by index.
func (c *Client) FilterAccessTokenCoveredClasses(ctx context.Context, tokenID string, classIDs []string, fullyConsistent bool) (map[string]bool, error) {
	out := make(map[string]bool, len(classIDs))
	if len(classIDs) == 0 {
		return out, nil
	}

	items := make([]*v1.CheckBulkPermissionsRequestItem, 0, len(classIDs))
	for _, classID := range classIDs {
		items = append(items, &v1.CheckBulkPermissionsRequestItem{
			Resource:   &v1.ObjectReference{ObjectType: accesstoken.ObjectType, ObjectId: tokenID},
			Permission: accesstoken.PermissionCovers,
			Subject:    &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "agentclass", ObjectId: classID}},
		})
	}

	resp, err := c.cl.CheckBulkPermissions(ctx, &v1.CheckBulkPermissionsRequest{
		Consistency: consistencyFor(fullyConsistent),
		Items:       items,
	})
	if err != nil {
		return nil, fmt.Errorf("filter covered classes for %s: %w", tokenID, err)
	}

	for _, pair := range resp.GetPairs() {
		if perr := pair.GetError(); perr != nil {
			log.FromContext(ctx).Info("filter covered classes bulk check pair errored; class denied",
				"tokenID", tokenID, "classID", pair.GetRequest().GetSubject().GetObject().GetObjectId(),
				"code", perr.GetCode(), "err", perr.GetMessage())
			continue
		}
		item := pair.GetItem()
		if item == nil || item.GetPermissionship() != v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION {
			continue
		}
		classID := pair.GetRequest().GetSubject().GetObject().GetObjectId()
		if classID == "" {
			continue
		}
		out[classID] = true
	}
	return out, nil
}

// ReadAccessTokenGrant reads the token's tuples back for display (the admin
// tokens page). found=false when no tuples exist (revoked or expired-out —
// SpiceDB drops expired relationships from reads once their expiration has
// elapsed).
func (c *Client) ReadAccessTokenGrant(ctx context.Context, tokenID string) (AccessTokenGrant, bool, error) {
	g := AccessTokenGrant{TokenID: tokenID}
	stream, err := c.cl.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency: consistencyFor(true),
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       accesstoken.ObjectType,
			OptionalResourceId: tokenID,
		},
	})
	if err != nil {
		return g, false, fmt.Errorf("read access token grant for %s: %w", tokenID, err)
	}

	found := false
	for {
		resp, rerr := stream.Recv()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return g, false, fmt.Errorf("read access token grant stream for %s: %w", tokenID, rerr)
		}
		rel := resp.GetRelationship()
		found = true
		switch rel.GetRelation() {
		case accesstoken.RelationRoleRead:
			g.Role = accesstoken.RoleRead
			g.Owner = identity.CanonicalFromTrusted(rel.GetSubject().GetObject().GetObjectId(), "read back from the accesstoken role tuple SpiceDB stores")
		case accesstoken.RelationRoleInteract:
			g.Role = accesstoken.RoleInteract
			g.Owner = identity.CanonicalFromTrusted(rel.GetSubject().GetObject().GetObjectId(), "read back from the accesstoken role tuple SpiceDB stores")
		case accesstoken.RelationRoleFull:
			g.Role = accesstoken.RoleFull
			g.Owner = identity.CanonicalFromTrusted(rel.GetSubject().GetObject().GetObjectId(), "read back from the accesstoken role tuple SpiceDB stores")
		case accesstoken.RelationScopeClass:
			id := rel.GetSubject().GetObject().GetObjectId()
			if id == wildcardSubjectID {
				g.Unfiltered = true
			} else {
				g.ScopeClasses = append(g.ScopeClasses, id)
			}
		}
		if exp := rel.GetOptionalExpiresAt(); exp != nil {
			g.ExpiresAt = exp.AsTime()
		}
	}
	return g, found, nil
}

// TouchSessionOwner writes agentsession:<ns>/<name>#owner@user:<canonical> —
// the typed convenience wrapper over TouchOwner for the common case of a
// bare canonical user owner, needed by this package's own integration tests
// and by Task 13's fixture (test/e2e/steelthread and friends) to stand up an
// owner tuple without constructing a "user:<id>" subjectRef string by hand.
func (c *Client) TouchSessionOwner(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) error {
	return c.TouchOwner(ctx, ns, name, "user:"+canonicalID.String())
}
