//go:build integration

package spicedb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func TestWorkshopBuild_OnlyTheBoundSession(t *testing.T) {
	c := newIntegrationClient(t)
	ctx := context.Background()
	const ws = "ws-a1b2c3d4e5f6"
	starter := uniqCanon(t, "starter-")

	require.NoError(t, c.EnsureWorkshopSubjects(ctx, ws, "default", "builder-s1", starter))

	cases := []struct {
		name             string
		sessNS, sessName string
		grant            bool
	}{
		{"the bound session: build granted", "default", "builder-s1", true},
		{"another session, same namespace: refused", "default", "builder-s2", false},
		{"same name, another namespace: refused", "other", "builder-s1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := c.CheckWorkshopBuild(ctx, ws, tc.sessNS, tc.sessName)
			require.NoError(t, err)
			assert.Equal(t, tc.grant, ok)
		})
	}

	// Idempotent re-ensure, then teardown removes standing entirely.
	require.NoError(t, c.EnsureWorkshopSubjects(ctx, ws, "default", "builder-s1", starter))
	require.NoError(t, c.DeleteWorkshopRelationships(ctx, ws))
	ok, err := c.CheckWorkshopBuild(ctx, ws, "default", "builder-s1")
	require.NoError(t, err)
	assert.False(t, ok, "a deleted workshop confers nothing (fail closed after teardown)")
}

// TestWorkshopClose_StarterOrPlatformAdmin is the round trip behind the close
// gate: the tuples EnsureWorkshopSubjects writes are the ones CheckWorkshopClose
// reads, and teardown revokes both arms. The unit tests pin each half's shape;
// only this one proves they meet.
func TestWorkshopClose_StarterOrPlatformAdmin(t *testing.T) {
	c := newIntegrationClient(t)
	ctx := context.Background()
	const ws = "ws-b2c3d4e5f6a1"
	starter := uniqCanon(t, "starter-")
	admin := uniqCanon(t, "admin-")
	stranger := uniqCanon(t, "stranger-")

	require.NoError(t, c.EnsureWorkshopSubjects(ctx, ws, "default", "builder-s1", starter))
	require.NoError(t, c.TouchPlatformAdmin(ctx, admin))

	cases := []struct {
		name  string
		who   identity.CanonicalUserID
		grant bool
	}{
		{"the starter: close granted", starter, true},
		{"a platform admin: close granted", admin, true},
		{"another person: refused", stranger, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := c.CheckWorkshopClose(ctx, ws, tc.who)
			require.NoError(t, err)
			assert.Equal(t, tc.grant, ok)
		})
	}

	// Teardown deletes every relation on the workshop, so even the starter
	// loses standing — nothing may be closed twice.
	require.NoError(t, c.DeleteWorkshopRelationships(ctx, ws))
	ok, err := c.CheckWorkshopClose(ctx, ws, starter)
	require.NoError(t, err)
	assert.False(t, ok, "a deleted workshop confers nothing, to its starter least of all")
}

// TestWorkshopClose_NoStarterRecorded_AdminStillCloses covers the +optional
// spec.starterCanonical: provisioning must still succeed and still leave the
// workshop closable by an admin, because a workshop nobody can close is a slot
// nobody can free.
func TestWorkshopClose_NoStarterRecorded_AdminStillCloses(t *testing.T) {
	c := newIntegrationClient(t)
	ctx := context.Background()
	const ws = "ws-c3d4e5f6a1b2"
	admin := uniqCanon(t, "admin-")

	require.NoError(t, c.EnsureWorkshopSubjects(ctx, ws, "default", "builder-s1", identity.CanonicalUserID{}),
		"an unattributed workshop must still provision")
	require.NoError(t, c.TouchPlatformAdmin(ctx, admin))

	ok, err := c.CheckWorkshopBuild(ctx, ws, "default", "builder-s1")
	require.NoError(t, err)
	assert.True(t, ok, "the session tuple must survive an absent starter")

	ok, err = c.CheckWorkshopClose(ctx, ws, admin)
	require.NoError(t, err)
	assert.True(t, ok, "the platform arm is the only one left, and it must work")
}
