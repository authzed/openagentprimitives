//go:build integration

package spicedb

import (
	"context"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func TestLineage_ParentOwnerCanReadChildTranscript(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)

	alice := identity.CanonicalFromTrusted("YWxpY2VAZXhhbXBsZS5jb20", "test fixture")

	require.NoError(t, c.TouchOwner(ctx, "ns", "demo-parent", "user:"+alice.String()))
	require.NoError(t, c.TouchLineage(ctx, "ns", "demo-child", "ns", "demo-parent"))

	ok, err := c.CheckReadTranscript(ctx, "ns", "demo-child", alice, true)
	require.NoError(t, err, "CheckReadTranscript must not error")
	assert.True(t, ok, "a parent owner must be able to read a child's transcript")

	// approve inherits too, so the child's approvals route to the parent's owner.
	approve, err := c.CheckApprove(ctx, "ns", "demo-child", alice, true)
	require.NoError(t, err)
	assert.True(t, approve, "a parent owner must be able to approve for a child")
}

func TestLineage_DeniedOnChildBeatsInheritedApprove(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)

	bob := identity.CanonicalFromTrusted("Ym9iQGV4YW1wbGUuY29t", "test fixture")

	require.NoError(t, c.TouchOwner(ctx, "ns", "demo-parent2", "user:"+bob.String()))
	require.NoError(t, c.TouchLineage(ctx, "ns", "demo-child2", "ns", "demo-parent2"))

	// Establish the inherited grant is genuinely live on THIS object pair
	// before revoking it — without this, a no-op TouchLineage, a
	// resource/subject-reversed tuple, or a deleted `parent` relation would
	// still pass the assertions below, because approve/read_transcript with
	// no direct grant on the child reduce to EMPTY - {bob} = false regardless
	// of whether inheritance ever worked.
	preApprove, err := c.CheckApprove(ctx, "ns", "demo-child2", bob, true)
	require.NoError(t, err)
	assert.True(t, preApprove, "approve must be inherited through the parent arrow before any denial")

	preRead, err := c.CheckReadTranscript(ctx, "ns", "demo-child2", bob, true)
	require.NoError(t, err)
	assert.True(t, preRead, "read_transcript must be inherited through the parent arrow before any denial")

	require.NoError(t, c.TouchDeniedUser(ctx, "ns", "demo-child2", bob))

	approve, err := c.CheckApprove(ctx, "ns", "demo-child2", bob, true)
	require.NoError(t, err)
	assert.False(t, approve, "denied on the CHILD must beat approve inherited from the parent")

	read, err := c.CheckReadTranscript(ctx, "ns", "demo-child2", bob, true)
	require.NoError(t, err)
	assert.False(t, read, "denied on the CHILD must beat read_transcript inherited from the parent")
}

// TestReadTranscript_ParentSessionCanReadChildTranscript proves the `+ parent`
// arm added to read_transcript (agent-builder delegation framework, plan 4a
// task 3): the PARENT SESSION itself — not just the humans reachable through
// it via `parent->read_transcript` — may read its child's transcript
// directly, with no human in the loop. This is what will let a builder read
// the transcript of a child it spawned to test what it built.
//
// checkAsParent asks the raw CheckPermissionRequest rather than going through
// CheckReadTranscript, the same way TestConverse_DoesNotWidenInteractForASessionSubject
// does for interact: checkUser always builds a "user:<id>" subject, so an
// agentsession-typed subject is unaskable through it.
//
// The before/after shape (denied with no `parent` tuple, allowed once
// TouchLineage writes it) proves the arm is doing the work, not that the pair
// happened to be allowed some other way.
func TestReadTranscript_ParentSessionCanReadChildTranscript(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)

	const (
		ns      = "ns"
		builder = "demo-builder-x"
		child   = "demo-child-c"
	)

	checkAsParent := func(t *testing.T) bool {
		t.Helper()
		ok, err := c.check(ctx, &v1.CheckPermissionRequest{
			Resource:   &v1.ObjectReference{ObjectType: "agentsession", ObjectId: ns + "/" + child},
			Permission: "read_transcript",
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{
				ObjectType: "agentsession", ObjectId: ns + "/" + builder,
			}},
			Consistency: consistencyFor(true),
		}, "read_transcript")
		require.NoError(t, err, "an agentsession subject on read_transcript is a well-formed question")
		return ok
	}

	assert.False(t, checkAsParent(t),
		"without a parent relationship, the would-be parent session must not read the child's transcript")

	require.NoError(t, c.TouchLineage(ctx, ns, child, ns, builder))

	assert.True(t, checkAsParent(t),
		"once the parent relationship is written, the parent session must read its child's transcript directly")

	// The existing human-reader arm (parent->read_transcript) must still work
	// unchanged: a human accountable for the parent still reaches the child
	// through the parent, alongside the new `+ parent` arm rather than in place
	// of it.
	alice := identity.CanonicalFromTrusted("YWxpY2VAZXhhbXBsZS5jb20", "test fixture")
	require.NoError(t, c.TouchOwner(ctx, ns, builder, "user:"+alice.String()))
	humanOK, err := c.CheckReadTranscript(ctx, ns, child, alice, true)
	require.NoError(t, err, "CheckReadTranscript must not error")
	assert.True(t, humanOK, "a human accountable for the parent must still be able to read the child's transcript")
}
