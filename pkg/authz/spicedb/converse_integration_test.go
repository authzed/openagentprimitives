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

// TestConverse_BothDirectionsOfOneDelegationHop proves the whole point of
// agentsession#converse against a real SpiceDB: after TouchLineage, the two
// ends of a delegation may message each other and nothing else may message
// either.
//
// Both directions are asserted from ONE seeded edge, because they are one
// fact written by one atomic call — a test that seeded each separately could
// pass while the pair only ever half-lands.
func TestConverse_BothDirectionsOfOneDelegationHop(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)

	const (
		ns     = "ns"
		parent = "demo-converse-parent"
		child  = "demo-converse-child"
	)
	require.NoError(t, c.TouchLineage(ctx, ns, child, ns, parent))

	t.Run("parent -> child: the delegating session may drive its child's inbox", func(t *testing.T) {
		ok, err := c.CheckConverse(ctx, ns, child, ns, parent, true)
		require.NoError(t, err, "CheckConverse must not error")
		assert.True(t, ok)
	})

	t.Run("child -> parent: the delegated child may answer the session that spawned it", func(t *testing.T) {
		ok, err := c.CheckConverse(ctx, ns, parent, ns, child, true)
		require.NoError(t, err, "CheckConverse must not error")
		assert.True(t, ok, "without the downward `child` relation this direction is inexpressible")
	})
}

// TestConverse_RefusesEverySessionOutsideTheHop is the scoping proof: without
// it, "the parent may message the child" is indistinguishable from "any
// session may message any session".
//
// The tree is grandparent -> parent -> child, so the refusals are not
// strangers-only — a grandparent has genuine standing on the child through
// approve/hold/read_transcript and still may not TALK to it, because the
// transport is one Channel per hop and converse matches that reach exactly.
func TestConverse_RefusesEverySessionOutsideTheHop(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)

	const (
		ns          = "ns"
		grandparent = "demo-converse-gp"
		parent      = "demo-converse-p2"
		child       = "demo-converse-c2"
		sibling     = "demo-converse-sib"
		stranger    = "demo-converse-stranger"
	)
	require.NoError(t, c.TouchLineage(ctx, ns, parent, ns, grandparent))
	require.NoError(t, c.TouchLineage(ctx, ns, child, ns, parent))
	require.NoError(t, c.TouchLineage(ctx, ns, sibling, ns, parent))

	// Establish the grant is genuinely live on THIS object pair before
	// asserting anything is refused: every negative below would also hold if
	// TouchLineage had silently written nothing.
	live, err := c.CheckConverse(ctx, ns, child, ns, parent, true)
	require.NoError(t, err)
	require.True(t, live, "the one-hop grant must be live before the refusals mean anything")

	cases := []struct {
		name           string
		target, sender string
	}{
		{
			name:   "grandparent -> grandchild: refused, converse is one hop and does not close transitively",
			target: child, sender: grandparent,
		},
		{
			name:   "grandchild -> grandparent: refused in the upward direction too",
			target: grandparent, sender: child,
		},
		{
			name:   "sibling -> sibling: refused, a shared parent is not a channel between them",
			target: child, sender: sibling,
		},
		{
			name:   "unrelated session -> child: refused, no lineage at all",
			target: child, sender: stranger,
		},
		{
			name:   "child -> itself: refused, a session is not its own parent or child",
			target: child, sender: child,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := c.CheckConverse(ctx, ns, tc.target, ns, tc.sender, true)
			require.NoError(t, err, "CheckConverse must not error")
			assert.False(t, ok)
		})
	}
}

// TestConverse_DoesNotWidenInteractForASessionSubject pins the separation the
// whole design rests on: `converse` admits a session, `interact` never does.
//
// Both halves are checked against the SAME live lineage, so a refusal below is
// the separation holding and not a missing tuple.
func TestConverse_DoesNotWidenInteractForASessionSubject(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)

	const (
		ns     = "ns"
		parent = "demo-converse-parent3"
		child  = "demo-converse-child3"
	)
	require.NoError(t, c.TouchLineage(ctx, ns, child, ns, parent))

	converseOK, err := c.CheckConverse(ctx, ns, child, ns, parent, true)
	require.NoError(t, err)
	require.True(t, converseOK, "converse must answer for this pair before the refusals mean anything")

	// The real question: a genuine agentsession-typed subject asked about
	// interact. Every relation feeding interact is user | group#member, so this
	// must be a clean NO_PERMISSION — not an error, and never true.
	interactOK, err := c.check(ctx, &v1.CheckPermissionRequest{
		Resource:   &v1.ObjectReference{ObjectType: "agentsession", ObjectId: ns + "/" + child},
		Permission: "interact",
		Subject: &v1.SubjectReference{Object: &v1.ObjectReference{
			ObjectType: "agentsession", ObjectId: ns + "/" + parent,
		}},
		Consistency: consistencyFor(true),
	}, "interact")
	require.NoError(t, err, "an agentsession subject on interact is a well-formed question")
	assert.False(t, interactOK,
		"a session must never satisfy interact, which admits user | group#member only")
}

// TestInteract_RejectsASessionReferenceSmuggledInAsAUserID documents WHY the
// dispatch in authz.CheckSessionInbound has to happen before the check rather
// than inside it, and pins the pre-fix call shape as permanently unusable.
//
// The agent-channel acting subject used to be handed to CheckInteract, which
// builds `user:<canonicalID>` — producing a user object whose id was the
// literal string "agentsession:ns/<name>". SpiceDB does not merely fail to
// match that: object ids may not contain ':' at all, so the request is
// rejected as InvalidArgument. Through the Interact hook that surfaced as a
// TRANSIENT check failure, so channelsd took it as an infrastructure fault and
// dropped the inbound as an internal error — never as "not authorized", which
// is part of why the whole path read as mysteriously silent rather than denied.
func TestInteract_RejectsASessionReferenceSmuggledInAsAUserID(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)

	sessionAsUser := identity.CanonicalFromTrusted("agentsession:ns/demo-converse-parent3", "test fixture")
	ok, err := c.CheckInteract(ctx, "ns", "demo-converse-child3", sessionAsUser, true)
	assert.False(t, ok)
	require.Error(t, err, "SpiceDB must refuse a user id containing ':'")
	assert.Contains(t, err.Error(), "object_id",
		"the refusal must be the object-id validation, not some unrelated failure")
}

// TestConverse_LineageDeleteClosesBothDirections proves DeleteLineage is the
// inverse of TouchLineage. Removing only the upward tuple would leave a
// session that can no longer be talked TO but can still talk BACK.
func TestConverse_LineageDeleteClosesBothDirections(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)

	const (
		ns     = "ns"
		parent = "demo-converse-parent4"
		child  = "demo-converse-child4"
	)
	require.NoError(t, c.TouchLineage(ctx, ns, child, ns, parent))

	down, err := c.CheckConverse(ctx, ns, child, ns, parent, true)
	require.NoError(t, err)
	require.True(t, down, "parent -> child must be live before the delete")
	up, err := c.CheckConverse(ctx, ns, parent, ns, child, true)
	require.NoError(t, err)
	require.True(t, up, "child -> parent must be live before the delete")

	require.NoError(t, c.DeleteLineage(ctx, ns, child, ns, parent))

	down, err = c.CheckConverse(ctx, ns, child, ns, parent, true)
	require.NoError(t, err)
	assert.False(t, down, "parent -> child must be closed after the delete")
	up, err = c.CheckConverse(ctx, ns, parent, ns, child, true)
	require.NoError(t, err)
	assert.False(t, up, "child -> parent must be closed after the delete")
}
