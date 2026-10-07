// Pure-Go unit tests (no build tag, no real SpiceDB) for the access-token
// client helpers. The integration file proves the SpiceDB semantics end to
// end; these pin what a fake can pin without Docker:
//
//   - WriteAccessTokenGrant's validation guards and tuple SHAPE — one
//     WriteRelationships call, every update a TOUCH, every relationship
//     carrying OptionalExpiresAt, the role relation matching the role, and
//     the wildcard scope tuple when Unfiltered.
//   - CheckAccessTokenOp's response matching by echoed request identity —
//     the deny-on-mangle discipline: an omitted pair, an errored pair, or a
//     mangled echo leaves its leg false instead of panicking or leaking.
//   - FilterAccessTokenCoveredClasses' covered-subset mapping, with
//     unmatched classes absent/false.
//
// Follows the fakePermClient idiom from client_artifact_write_test.go: embed
// the interface nil and override only the methods under test, so any other
// call panics rather than silently succeeding.
package spicedb

import (
	"context"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	authzed "github.com/authzed/authzed-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/accesstoken"
)

// fakeAccessTokenPermClient captures WriteRelationships requests and answers
// CheckBulkPermissions from a canned respond func. Only those two methods are
// overridden — anything else panics on the nil embed, keeping each test
// honest about which RPCs its code path issues.
type fakeAccessTokenPermClient struct {
	v1.PermissionsServiceClient
	capturedWrites []*v1.WriteRelationshipsRequest
	respond        func(req *v1.CheckBulkPermissionsRequest) *v1.CheckBulkPermissionsResponse
}

func (f *fakeAccessTokenPermClient) WriteRelationships(_ context.Context, in *v1.WriteRelationshipsRequest, _ ...grpc.CallOption) (*v1.WriteRelationshipsResponse, error) {
	f.capturedWrites = append(f.capturedWrites, in)
	return &v1.WriteRelationshipsResponse{}, nil
}

func (f *fakeAccessTokenPermClient) CheckBulkPermissions(_ context.Context, req *v1.CheckBulkPermissionsRequest, _ ...grpc.CallOption) (*v1.CheckBulkPermissionsResponse, error) {
	return f.respond(req), nil
}

func newAccessTokenFakeClient(t *testing.T) (*Client, *fakeAccessTokenPermClient) {
	t.Helper()
	fake := &fakeAccessTokenPermClient{}
	return &Client{cl: &authzed.Client{PermissionsServiceClient: fake}}, fake
}

// grantedPair echoes item back as a HAS_PERMISSION pair — what a well-behaved
// SpiceDB returns for an allowed check.
func grantedPair(item *v1.CheckBulkPermissionsRequestItem) *v1.CheckBulkPermissionsPair {
	return &v1.CheckBulkPermissionsPair{
		Request: item,
		Response: &v1.CheckBulkPermissionsPair_Item{Item: &v1.CheckBulkPermissionsResponseItem{
			Permissionship: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		}},
	}
}

func deniedPair(item *v1.CheckBulkPermissionsRequestItem) *v1.CheckBulkPermissionsPair {
	return &v1.CheckBulkPermissionsPair{
		Request: item,
		Response: &v1.CheckBulkPermissionsPair_Item{Item: &v1.CheckBulkPermissionsResponseItem{
			Permissionship: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
		}},
	}
}

func erroredPair(item *v1.CheckBulkPermissionsRequestItem) *v1.CheckBulkPermissionsPair {
	return &v1.CheckBulkPermissionsPair{
		Request:  item,
		Response: &v1.CheckBulkPermissionsPair_Error{Error: &rpcstatus.Status{Code: 13, Message: "backend mangled this item"}},
	}
}

func TestAccessTokenWriteGrant_ValidationGuards(t *testing.T) {
	owner := identity.CanonicalFromTrusted("alice", "test fixture")
	valid := AccessTokenGrant{
		TokenID: "at-1", Owner: owner, Role: accesstoken.RoleRead,
		ScopeClasses: []string{"default/demo-agent"}, ExpiresAt: time.Now().Add(time.Hour),
	}

	cases := []struct {
		name    string
		mutate  func(g *AccessTokenGrant)
		wantErr string
	}{
		{
			name:    "missing token id: refused before any write",
			mutate:  func(g *AccessTokenGrant) { g.TokenID = "" },
			wantErr: "requires token id and owner",
		},
		{
			name:    "missing owner: refused before any write",
			mutate:  func(g *AccessTokenGrant) { g.Owner = identity.CanonicalUserID{} },
			wantErr: "requires token id and owner",
		},
		{
			name:    "missing expiry: refused before any write",
			mutate:  func(g *AccessTokenGrant) { g.ExpiresAt = time.Time{} },
			wantErr: "requires an expiry",
		},
		{
			name:    "unknown role: refused before any write",
			mutate:  func(g *AccessTokenGrant) { g.Role = "superadmin" },
			wantErr: "unknown access-token role",
		},
		{
			name:    "no scope classes and not Unfiltered: refused before any write",
			mutate:  func(g *AccessTokenGrant) { g.ScopeClasses = nil },
			wantErr: "requires scope classes or Unfiltered",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, fake := newAccessTokenFakeClient(t)
			g := valid
			tc.mutate(&g)
			err := c.WriteAccessTokenGrant(context.Background(), g)
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.wantErr)
			assert.Empty(t, fake.capturedWrites, "a refused grant must issue no WriteRelationships call")
		})
	}
}

func TestAccessTokenWriteGrant_ScopedTupleShape(t *testing.T) {
	c, fake := newAccessTokenFakeClient(t)
	owner := identity.CanonicalFromTrusted("alice", "test fixture")
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)

	require.NoError(t, c.WriteAccessTokenGrant(context.Background(), AccessTokenGrant{
		TokenID: "at-1", Owner: owner, Role: accesstoken.RoleInteract,
		ScopeClasses: []string{"default/demo-agent", "other-ns/second-agent"},
		ExpiresAt:    expiry,
	}))

	require.Len(t, fake.capturedWrites, 1, "the whole grant must be ONE WriteRelationships call")
	updates := fake.capturedWrites[0].GetUpdates()
	require.Len(t, updates, 3, "one role tuple plus one scope tuple per class")

	// Index by (relation, subject) so update order is not pinned.
	type tuple struct{ rel, sub string }
	got := map[tuple]*v1.Relationship{}
	for _, u := range updates {
		assert.Equal(t, v1.RelationshipUpdate_OPERATION_TOUCH, u.GetOperation(), "every update must be a TOUCH")
		rel := u.GetRelationship()
		assert.Equal(t, accesstoken.ObjectType, rel.GetResource().GetObjectType())
		assert.Equal(t, "at-1", rel.GetResource().GetObjectId())
		require.NotNil(t, rel.GetOptionalExpiresAt(), "every relationship must carry OptionalExpiresAt")
		assert.True(t, rel.GetOptionalExpiresAt().AsTime().Equal(expiry), "the expiry must be the grant's ExpiresAt")
		sub := rel.GetSubject().GetObject()
		got[tuple{rel.GetRelation(), sub.GetObjectType() + ":" + sub.GetObjectId()}] = rel
	}

	assert.Contains(t, got, tuple{accesstoken.RelationRoleInteract, "user:alice"},
		"the role relation must match the role (interact -> role_interact)")
	assert.Contains(t, got, tuple{accesstoken.RelationScopeClass, "agentclass:default/demo-agent"})
	assert.Contains(t, got, tuple{accesstoken.RelationScopeClass, "agentclass:other-ns/second-agent"})
}

func TestAccessTokenWriteGrant_UnfilteredWritesTheWildcardScopeTuple(t *testing.T) {
	c, fake := newAccessTokenFakeClient(t)
	owner := identity.CanonicalFromTrusted("bob", "test fixture")

	require.NoError(t, c.WriteAccessTokenGrant(context.Background(), AccessTokenGrant{
		TokenID: "at-2", Owner: owner, Role: accesstoken.RoleFull,
		// ScopeClasses deliberately set alongside Unfiltered: the wildcard
		// must REPLACE them, not join them, or a revoked class filter would
		// linger as extra tuples.
		ScopeClasses: []string{"default/demo-agent"},
		Unfiltered:   true,
		ExpiresAt:    time.Now().Add(time.Hour),
	}))

	require.Len(t, fake.capturedWrites, 1, "the whole grant must be ONE WriteRelationships call")
	updates := fake.capturedWrites[0].GetUpdates()
	require.Len(t, updates, 2, "one role tuple plus exactly one wildcard scope tuple")

	var scope *v1.Relationship
	for _, u := range updates {
		if u.GetRelationship().GetRelation() == accesstoken.RelationScopeClass {
			scope = u.GetRelationship()
		}
	}
	require.NotNil(t, scope, "the wildcard scope tuple must be written")
	assert.Equal(t, "agentclass", scope.GetSubject().GetObject().GetObjectType())
	assert.Equal(t, "*", scope.GetSubject().GetObject().GetObjectId(), "Unfiltered means the agentclass:* wildcard subject")
	assert.NotNil(t, scope.GetOptionalExpiresAt(), "the wildcard tuple expires with the grant too")
}

// checkFixture is the valid three-leg check the CheckAccessTokenOp tests
// perturb per case.
func checkFixture() AccessTokenCheck {
	return AccessTokenCheck{
		TokenID:      "at-1",
		Owner:        identity.CanonicalFromTrusted("carol", "test fixture"),
		Permission:   "read_transcript",
		ResourceType: "agentsession",
		ResourceID:   "default/demo-session",
		ClassID:      "default/demo-agent",
	}
}

func TestAccessTokenCheckOp_ResponseMatching(t *testing.T) {
	cases := []struct {
		name    string
		respond func(req *v1.CheckBulkPermissionsRequest) *v1.CheckBulkPermissionsResponse
		want    AccessTokenDecision
	}{
		{
			name: "all three legs HAS_PERMISSION: Allowed, each leg attributed to its own field",
			respond: func(req *v1.CheckBulkPermissionsRequest) *v1.CheckBulkPermissionsResponse {
				resp := &v1.CheckBulkPermissionsResponse{}
				for _, item := range req.GetItems() {
					resp.Pairs = append(resp.Pairs, grantedPair(item))
				}
				return resp
			},
			want: AccessTokenDecision{TokenGrants: true, ScopeCovers: true, OwnerHas: true},
		},
		{
			name: "covers pair omitted from the response: ScopeCovers stays false (deny-on-mangle)",
			respond: func(req *v1.CheckBulkPermissionsRequest) *v1.CheckBulkPermissionsResponse {
				resp := &v1.CheckBulkPermissionsResponse{}
				for _, item := range req.GetItems() {
					if item.GetPermission() == accesstoken.PermissionCovers {
						continue
					}
					resp.Pairs = append(resp.Pairs, grantedPair(item))
				}
				return resp
			},
			want: AccessTokenDecision{TokenGrants: true, ScopeCovers: false, OwnerHas: true},
		},
		{
			name: "mirror pair carries a pair-level error: TokenGrants stays false, other legs unaffected",
			respond: func(req *v1.CheckBulkPermissionsRequest) *v1.CheckBulkPermissionsResponse {
				resp := &v1.CheckBulkPermissionsResponse{}
				for _, item := range req.GetItems() {
					if item.GetResource().GetObjectType() == accesstoken.ObjectType &&
						item.GetPermission() != accesstoken.PermissionCovers {
						resp.Pairs = append(resp.Pairs, erroredPair(item))
						continue
					}
					resp.Pairs = append(resp.Pairs, grantedPair(item))
				}
				return resp
			},
			want: AccessTokenDecision{TokenGrants: false, ScopeCovers: true, OwnerHas: true},
		},
		{
			name: "resource leg NO_PERMISSION: only OwnerHas false — a token cannot outrank its owner",
			respond: func(req *v1.CheckBulkPermissionsRequest) *v1.CheckBulkPermissionsResponse {
				resp := &v1.CheckBulkPermissionsResponse{}
				for _, item := range req.GetItems() {
					if item.GetResource().GetObjectType() == "agentsession" {
						resp.Pairs = append(resp.Pairs, deniedPair(item))
						continue
					}
					resp.Pairs = append(resp.Pairs, grantedPair(item))
				}
				return resp
			},
			want: AccessTokenDecision{TokenGrants: true, ScopeCovers: true, OwnerHas: false},
		},
		{
			name: "a granted pair echoing an unrecognized resource: no leg flips (deny-on-mangle)",
			respond: func(req *v1.CheckBulkPermissionsRequest) *v1.CheckBulkPermissionsResponse {
				return &v1.CheckBulkPermissionsResponse{Pairs: []*v1.CheckBulkPermissionsPair{
					grantedPair(&v1.CheckBulkPermissionsRequestItem{
						Resource:   &v1.ObjectReference{ObjectType: "agentsession", ObjectId: "elsewhere/other"},
						Permission: "read_transcript",
					}),
				}}
			},
			want: AccessTokenDecision{},
		},
		{
			name: "empty response: nothing granted",
			respond: func(_ *v1.CheckBulkPermissionsRequest) *v1.CheckBulkPermissionsResponse {
				return &v1.CheckBulkPermissionsResponse{}
			},
			want: AccessTokenDecision{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, fake := newAccessTokenFakeClient(t)
			fake.respond = tc.respond
			dec, err := c.CheckAccessTokenOp(context.Background(), checkFixture(), true)
			require.NoError(t, err)
			assert.Equal(t, tc.want, dec)
			assert.Equal(t, tc.want.TokenGrants && tc.want.ScopeCovers && tc.want.OwnerHas, dec.Allowed())
		})
	}
}

// TestAccessTokenCheckOp_ResourcePermissionDecouplesLeg3 pins the
// ResourcePermission contract: leg 1 (the token mirror) keeps chk.Permission,
// leg 3 (the resource check) uses ResourcePermission when set, and each
// granted pair is still attributed to its own leg — the "view" mirror whose
// agentsession-level truth is read_transcript (mcpfront's authorizeSessionOp
// mapping) is the motivating caller.
func TestAccessTokenCheckOp_ResourcePermissionDecouplesLeg3(t *testing.T) {
	legPermissions := func(req *v1.CheckBulkPermissionsRequest) (mirror, resource string) {
		for _, item := range req.GetItems() {
			switch {
			case item.GetResource().GetObjectType() == accesstoken.ObjectType &&
				item.GetPermission() != accesstoken.PermissionCovers:
				mirror = item.GetPermission()
			case item.GetResource().GetObjectType() != accesstoken.ObjectType:
				resource = item.GetPermission()
			}
		}
		return mirror, resource
	}

	t.Run("view mirror + read_transcript resource perm: legs carry different names, each attributed correctly", func(t *testing.T) {
		c, fake := newAccessTokenFakeClient(t)
		var captured *v1.CheckBulkPermissionsRequest
		fake.respond = func(req *v1.CheckBulkPermissionsRequest) *v1.CheckBulkPermissionsResponse {
			captured = req
			resp := &v1.CheckBulkPermissionsResponse{}
			for _, item := range req.GetItems() {
				resp.Pairs = append(resp.Pairs, grantedPair(item))
			}
			return resp
		}

		chk := checkFixture()
		chk.Permission = "view"
		chk.ResourcePermission = "read_transcript"
		dec, err := c.CheckAccessTokenOp(context.Background(), chk, true)
		require.NoError(t, err)
		assert.Equal(t, AccessTokenDecision{TokenGrants: true, ScopeCovers: true, OwnerHas: true}, dec)

		require.NotNil(t, captured)
		require.Len(t, captured.GetItems(), 3)
		mirror, resource := legPermissions(captured)
		assert.Equal(t, "view", mirror, "leg 1 must keep the token mirror name")
		assert.Equal(t, "read_transcript", resource, "leg 3 must check the resource's own permission")
	})

	t.Run("only the resource leg granted: OwnerHas alone flips, no cross-leg bleed", func(t *testing.T) {
		c, fake := newAccessTokenFakeClient(t)
		fake.respond = func(req *v1.CheckBulkPermissionsRequest) *v1.CheckBulkPermissionsResponse {
			resp := &v1.CheckBulkPermissionsResponse{}
			for _, item := range req.GetItems() {
				if item.GetResource().GetObjectType() == "agentsession" {
					resp.Pairs = append(resp.Pairs, grantedPair(item))
					continue
				}
				resp.Pairs = append(resp.Pairs, deniedPair(item))
			}
			return resp
		}

		chk := checkFixture()
		chk.Permission = "view"
		chk.ResourcePermission = "read_transcript"
		dec, err := c.CheckAccessTokenOp(context.Background(), chk, true)
		require.NoError(t, err)
		assert.Equal(t, AccessTokenDecision{OwnerHas: true}, dec)
		assert.False(t, dec.Allowed())
	})

	t.Run("empty ResourcePermission: leg 3 defaults to Permission, exactly the pre-field behavior", func(t *testing.T) {
		c, fake := newAccessTokenFakeClient(t)
		var captured *v1.CheckBulkPermissionsRequest
		fake.respond = func(req *v1.CheckBulkPermissionsRequest) *v1.CheckBulkPermissionsResponse {
			captured = req
			resp := &v1.CheckBulkPermissionsResponse{}
			for _, item := range req.GetItems() {
				resp.Pairs = append(resp.Pairs, grantedPair(item))
			}
			return resp
		}

		dec, err := c.CheckAccessTokenOp(context.Background(), checkFixture(), true)
		require.NoError(t, err)
		assert.True(t, dec.Allowed())

		require.NotNil(t, captured)
		mirror, resource := legPermissions(captured)
		assert.Equal(t, "read_transcript", mirror)
		assert.Equal(t, "read_transcript", resource, "an unset ResourcePermission must leave leg 3 on Permission")
	})
}

func TestAccessTokenCheckOp_RefusesIncompleteAndInternalPermissions(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(chk *AccessTokenCheck)
		wantErr string
	}{
		{
			name:    "missing class id: refused",
			mutate:  func(chk *AccessTokenCheck) { chk.ClassID = "" },
			wantErr: "requires token, owner, permission, resource, and class",
		},
		{
			name:    "missing owner: refused",
			mutate:  func(chk *AccessTokenCheck) { chk.Owner = identity.CanonicalUserID{} },
			wantErr: "requires token, owner, permission, resource, and class",
		},
		{
			name:    "permission \"covers\" is internal: refused, never mis-attributed to leg 2",
			mutate:  func(chk *AccessTokenCheck) { chk.Permission = accesstoken.PermissionCovers },
			wantErr: "internal to the token definition",
		},
		{
			name:    "permission \"owner\" is internal: refused",
			mutate:  func(chk *AccessTokenCheck) { chk.Permission = accesstoken.PermissionOwner },
			wantErr: "internal to the token definition",
		},
		{
			name:    "resource permission \"covers\" is internal: refused, the guard covers both fields",
			mutate:  func(chk *AccessTokenCheck) { chk.ResourcePermission = accesstoken.PermissionCovers },
			wantErr: "internal to the token definition",
		},
		{
			name:    "resource permission \"owner\" is internal: refused",
			mutate:  func(chk *AccessTokenCheck) { chk.ResourcePermission = accesstoken.PermissionOwner },
			wantErr: "internal to the token definition",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, fake := newAccessTokenFakeClient(t)
			fake.respond = func(_ *v1.CheckBulkPermissionsRequest) *v1.CheckBulkPermissionsResponse {
				t.Fatal("a refused check must never reach SpiceDB")
				return nil
			}
			chk := checkFixture()
			tc.mutate(&chk)
			dec, err := c.CheckAccessTokenOp(context.Background(), chk, true)
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.wantErr)
			assert.False(t, dec.Allowed())
		})
	}
}

func TestAccessTokenFilterCoveredClasses_CoveredSubsetMapping(t *testing.T) {
	c, fake := newAccessTokenFakeClient(t)
	fake.respond = func(req *v1.CheckBulkPermissionsRequest) *v1.CheckBulkPermissionsResponse {
		resp := &v1.CheckBulkPermissionsResponse{}
		for _, item := range req.GetItems() {
			switch item.GetSubject().GetObject().GetObjectId() {
			case "default/covered-agent":
				resp.Pairs = append(resp.Pairs, grantedPair(item))
			case "default/denied-agent":
				resp.Pairs = append(resp.Pairs, deniedPair(item))
			case "default/errored-agent":
				resp.Pairs = append(resp.Pairs, erroredPair(item))
			default:
				// default/omitted-agent: no pair at all — a mangled response.
			}
		}
		return resp
	}

	covered, err := c.FilterAccessTokenCoveredClasses(context.Background(), "at-1",
		[]string{"default/covered-agent", "default/denied-agent", "default/errored-agent", "default/omitted-agent"}, true)
	require.NoError(t, err)

	assert.True(t, covered["default/covered-agent"])
	assert.False(t, covered["default/denied-agent"], "NO_PERMISSION stays false")
	assert.False(t, covered["default/errored-agent"], "a pair-level error stays false (deny-on-mangle)")
	assert.False(t, covered["default/omitted-agent"], "an omitted pair stays false (deny-on-mangle)")
	assert.Len(t, covered, 1, "only covered classes land in the map")
}

func TestAccessTokenFilterCoveredClasses_EmptyInputSkipsTheRoundTrip(t *testing.T) {
	c, fake := newAccessTokenFakeClient(t)
	fake.respond = func(_ *v1.CheckBulkPermissionsRequest) *v1.CheckBulkPermissionsResponse {
		t.Fatal("an empty class set must never reach SpiceDB")
		return nil
	}
	covered, err := c.FilterAccessTokenCoveredClasses(context.Background(), "at-1", nil, true)
	require.NoError(t, err)
	assert.Empty(t, covered)
}
