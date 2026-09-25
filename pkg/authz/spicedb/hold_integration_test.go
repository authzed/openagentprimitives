//go:build integration

package spicedb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// TestHold_OwnerMayHold proves the base case: a session's own owner may hold
// it directly, with no delegation involved.
func TestHold_OwnerMayHold(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)

	carol := identity.CanonicalFromTrusted("Y2Fyb2xAZXhhbXBsZS5jb20", "test fixture")

	require.NoError(t, c.TouchOwner(ctx, "ns", "demo-hold-owner", "user:"+carol.String()))

	ok, err := c.CheckHold(ctx, "ns", "demo-hold-owner", carol, true)
	require.NoError(t, err, "CheckHold must not error")
	assert.True(t, ok, "a session's own owner must be able to hold it")
}

// TestHold_NonOwnerNonParticipantMayNotHold proves the negative base case: a
// user with no relation to the session at all is refused.
func TestHold_NonOwnerNonParticipantMayNotHold(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)

	carol := identity.CanonicalFromTrusted("Y2Fyb2xAZXhhbXBsZS5jb20", "test fixture")
	dave := identity.CanonicalFromTrusted("ZGF2ZUBleGFtcGxlLmNvbQ", "test fixture")

	require.NoError(t, c.TouchOwner(ctx, "ns", "demo-hold-stranger", "user:"+carol.String()))

	ok, err := c.CheckHold(ctx, "ns", "demo-hold-stranger", dave, true)
	require.NoError(t, err, "CheckHold must not error")
	assert.False(t, ok, "a non-owner, non-participant must not be able to hold the session")
}

// TestHold_ParentOwnerMayHoldChild is the inheritance arm: without
// `parent->hold` in the permission, this would just be `owner`, and a parent's
// owner would be unable to freeze a delegated child for review.
func TestHold_ParentOwnerMayHoldChild(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)

	erin := identity.CanonicalFromTrusted("ZXJpbkBleGFtcGxlLmNvbQ", "test fixture")

	require.NoError(t, c.TouchOwner(ctx, "ns", "demo-hold-parent", "user:"+erin.String()))
	require.NoError(t, c.TouchLineage(ctx, "ns", "demo-hold-child", "ns", "demo-hold-parent"))

	// erin holds no direct relation on the child at all — only on the parent.
	ok, err := c.CheckHold(ctx, "ns", "demo-hold-child", erin, true)
	require.NoError(t, err, "CheckHold must not error")
	assert.True(t, ok, "a parent's owner must be able to hold a delegated child")
}

// TestHold_DeniedOnChildBeatsInheritedOwner proves `- denied` is subtracted
// AFTER the parent arrow: an owner of the parent who has been explicitly
// denied on the child must not retain the inherited hold grant. Same property
// approve's comment claims for itself.
func TestHold_DeniedOnChildBeatsInheritedOwner(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)

	frank := identity.CanonicalFromTrusted("ZnJhbmtAZXhhbXBsZS5jb20", "test fixture")

	require.NoError(t, c.TouchOwner(ctx, "ns", "demo-hold-parent2", "user:"+frank.String()))
	require.NoError(t, c.TouchLineage(ctx, "ns", "demo-hold-child2", "ns", "demo-hold-parent2"))

	// Establish the inherited grant is genuinely live on THIS object pair
	// before denying it — otherwise a no-op TouchLineage or a reversed tuple
	// would still pass the assertion below for the wrong reason: EMPTY -
	// {frank} is false regardless of whether inheritance ever worked.
	preHold, err := c.CheckHold(ctx, "ns", "demo-hold-child2", frank, true)
	require.NoError(t, err)
	assert.True(t, preHold, "hold must be inherited through the parent arrow before any denial")

	require.NoError(t, c.TouchDeniedUser(ctx, "ns", "demo-hold-child2", frank))

	ok, err := c.CheckHold(ctx, "ns", "demo-hold-child2", frank, true)
	require.NoError(t, err)
	assert.False(t, ok, "denied on the CHILD must beat hold inherited from the parent's owner")
}
