//go:build integration

package spicedb

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/accesstoken"
)

func TestAccessTokenGrantRoundTrip(t *testing.T) {
	c := newIntegrationClient(t)
	ctx := context.Background()
	owner := uniqCanon(t, "alice")
	tokenID := uniq(t, "tok")
	classID := uniq(t, "default") + "/demo-agent"

	g := AccessTokenGrant{
		TokenID: tokenID, Owner: owner, Role: accesstoken.RoleRead,
		ScopeClasses: []string{classID}, ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, c.WriteAccessTokenGrant(ctx, g))

	// Read-role mirror grants read_transcript, refuses interact.
	ok, err := c.CheckAccessTokenMirror(ctx, tokenID, owner, "read_transcript", true)
	require.NoError(t, err)
	assert.True(t, ok, "read role must grant read_transcript mirror")
	ok, err = c.CheckAccessTokenMirror(ctx, tokenID, owner, "interact", true)
	require.NoError(t, err)
	assert.False(t, ok, "read role must not grant interact mirror")

	// Scope covers the named class only.
	covered, err := c.FilterAccessTokenCoveredClasses(ctx, tokenID, []string{classID, "default/other-agent"}, true)
	require.NoError(t, err)
	assert.True(t, covered[classID])
	assert.False(t, covered["default/other-agent"])

	// Read-back for display.
	got, found, err := c.ReadAccessTokenGrant(ctx, tokenID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, accesstoken.RoleRead, got.Role)
	assert.Equal(t, []string{classID}, got.ScopeClasses)
	assert.False(t, got.Unfiltered)

	// Delete revokes everything; read-back finds nothing.
	require.NoError(t, c.DeleteAccessTokenTuples(ctx, tokenID))
	ok, err = c.CheckAccessTokenMirror(ctx, tokenID, owner, "read_transcript", true)
	require.NoError(t, err)
	assert.False(t, ok, "deleted token must fail the mirror check")
	_, found, err = c.ReadAccessTokenGrant(ctx, tokenID)
	require.NoError(t, err)
	assert.False(t, found)
}

func TestAccessTokenWildcardAndFullRole(t *testing.T) {
	c := newIntegrationClient(t)
	ctx := context.Background()
	owner := uniqCanon(t, "bob")
	tokenID := uniq(t, "tok")

	g := AccessTokenGrant{
		TokenID: tokenID, Owner: owner, Role: accesstoken.RoleFull,
		Unfiltered: true, ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, c.WriteAccessTokenGrant(ctx, g))

	// Full role satisfies the whole ladder.
	for _, perm := range accesstoken.MirrorPermissions() {
		ok, err := c.CheckAccessTokenMirror(ctx, tokenID, owner, perm, true)
		require.NoError(t, err)
		assert.True(t, ok, "full role must grant %s", perm)
	}
	// Wildcard covers any class.
	covered, err := c.FilterAccessTokenCoveredClasses(ctx, tokenID, []string{"ns-a/x", "ns-b/y"}, true)
	require.NoError(t, err)
	assert.True(t, covered["ns-a/x"])
	assert.True(t, covered["ns-b/y"])
	// A different user is not the owner.
	ok, err := c.CheckAccessTokenMirror(ctx, tokenID, uniqCanon(t, "mallory"), "read_transcript", true)
	require.NoError(t, err)
	assert.False(t, ok, "mirror check binds to the owner subject")
}

func TestCheckAccessTokenOpThreeLegs(t *testing.T) {
	c := newIntegrationClient(t)
	ctx := context.Background()
	owner := uniqCanon(t, "carol")
	tokenID := uniq(t, "tok")
	ns, sess := uniq(t, "ns"), "demo-session"
	classID := uniq(t, "ns") + "/demo-agent"

	require.NoError(t, c.WriteAccessTokenGrant(ctx, AccessTokenGrant{
		TokenID: tokenID, Owner: owner, Role: accesstoken.RoleRead,
		ScopeClasses: []string{classID}, ExpiresAt: time.Now().Add(time.Hour),
	}))
	// Owner can interact (owner tuple on the session) — reuse an existing write helper.
	require.NoError(t, c.TouchSessionOwner(ctx, ns, sess, owner))

	dec, err := c.CheckAccessTokenOp(ctx, AccessTokenCheck{
		TokenID: tokenID, Owner: owner, Permission: "read_transcript",
		ResourceType: "agentsession", ResourceID: ns + "/" + sess, ClassID: classID,
	}, true)
	require.NoError(t, err)
	assert.True(t, dec.TokenGrants)
	assert.True(t, dec.ScopeCovers)
	assert.True(t, dec.OwnerHas)
	assert.True(t, dec.Allowed())

	// Same op against an out-of-scope class: only leg 2 flips.
	dec, err = c.CheckAccessTokenOp(ctx, AccessTokenCheck{
		TokenID: tokenID, Owner: owner, Permission: "read_transcript",
		ResourceType: "agentsession", ResourceID: ns + "/" + sess, ClassID: "elsewhere/other",
	}, true)
	require.NoError(t, err)
	assert.False(t, dec.ScopeCovers)
	assert.False(t, dec.Allowed())
}
