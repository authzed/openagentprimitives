//go:build integration

package spicedb

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func TestEnsureAgentClassStarters_SetIsExactlyTheDeclaredSet(t *testing.T) {
	c := newIntegrationClient(t)
	ctx := context.Background()
	const ns, name = "default", "gatebot-starters-exact"

	require.NoError(t, c.EnsureAgentClassStarters(ctx, ns, name, []string{"user:alice", "group:eng#member"}))
	got, err := c.ListAgentClassStarters(ctx, ns, name)
	require.NoError(t, err)
	assert.Equal(t, []string{"group:eng#member", "user:alice"}, got, "sorted, both written")

	// Shrinking the declared set DELETES the rest: the class is the source of
	// truth, and a starter removed from it must lose standing, not linger.
	require.NoError(t, c.EnsureAgentClassStarters(ctx, ns, name, []string{"user:alice"}))
	got, err = c.ListAgentClassStarters(ctx, ns, name)
	require.NoError(t, err)
	assert.Equal(t, []string{"user:alice"}, got)

	// Idempotent: the same set again is a no-op that still succeeds.
	require.NoError(t, c.EnsureAgentClassStarters(ctx, ns, name, []string{"user:alice"}))

	// Empty clears everything.
	require.NoError(t, c.EnsureAgentClassStarters(ctx, ns, name, nil))
	got, err = c.ListAgentClassStarters(ctx, ns, name)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestEnsureAgentClassStarters_RefusesUnrepresentableName(t *testing.T) {
	c := newIntegrationClient(t)
	err := c.EnsureAgentClassStarters(context.Background(), "default", "has.a.dot", []string{"user:alice"})
	require.ErrorIs(t, err, ErrUnrepresentableObjectID)
}

// TestEnsureAgentClassStarters_RefusesMalformedSubject documents WHY the
// AgentClass controller carries a shape rule of its own
// (agentclass.validateStarterShape) rather than leaning on
// authz.ValidateSubjectSet: the schema declares
// `relation starter: user | group#member`, and SpiceDB itself refuses anything
// else — including three shapes ValidateSubjectSet happily accepts.
//
// The refusal is what makes the rule load-bearing, because this write is ONE
// atomic WriteRelationships carrying the TOUCHes and the DELETEs together. See
// the final subtest: a single unholdable subject does not merely fail to be
// added, it keeps a REMOVED starter's standing alive.
func TestEnsureAgentClassStarters_RefusesMalformedSubject(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name    string
		subject string
	}{
		{"not a subject reference at all", "not-a-subject"},
		{"a group with no relation: the schema names group#member, not group", "group:eng"},
		{"a group with the wrong relation", "group:eng#owner"},
		{"a user as a subject-SET: the schema names the user type, not a set of them", "user:abc123#member"},
	}
	for _, tc := range cases {
		t.Run(tc.name+": refused, nothing written", func(t *testing.T) {
			c := newIntegrationClient(t)
			class := "gatebot-malformed-" + strings.NewReplacer(":", "-", "#", "-").Replace(tc.subject)

			err := c.EnsureAgentClassStarters(ctx, "default", class, []string{tc.subject})
			require.Error(t, err, "SpiceDB must refuse a subject its schema cannot hold on #starter")

			got, lerr := c.ListAgentClassStarters(ctx, "default", class)
			require.NoError(t, lerr)
			assert.Empty(t, got, "a malformed entry must fail the whole write, not half-apply it")
		})
	}

	t.Run("one unholdable subject strands a REMOVED starter's standing", func(t *testing.T) {
		c := newIntegrationClient(t)
		const class = "gatebot-malformed-strands"

		// A starter the class no longer declares.
		require.NoError(t, c.EnsureAgentClassStarters(ctx, "default", class, []string{"user:departed"}))

		// The class now declares one entry, and it is a shape the schema
		// cannot hold. The DELETE for "user:departed" rides in the SAME batch.
		err := c.EnsureAgentClassStarters(ctx, "default", class, []string{"group:eng"})
		require.Error(t, err)

		got, lerr := c.ListAgentClassStarters(ctx, "default", class)
		require.NoError(t, lerr)
		assert.Equal(t, []string{"user:departed"}, got,
			"THIS is why the controller drops an unholdable entry before the write: the batch's DELETE dies with it, "+
				"and a starter the class removed keeps standing while the class reports only StartersLinked=False")
	})
}

func TestCheckAgentClassStart_ExplicitExcludesPlatformAdmins(t *testing.T) {
	c := newIntegrationClient(t)
	ctx := context.Background()
	const ns, name = "default", "gatebot-check"
	admin := identity.CanonicalFromTrusted("admin-canonical", "test fixture")
	listed := identity.CanonicalFromTrusted("listed-canonical", "test fixture")
	stranger := identity.CanonicalFromTrusted("stranger-canonical", "test fixture")

	require.NoError(t, c.EnsureAgentClassPlatform(ctx, ns, name))
	require.NoError(t, c.TouchPlatformAdmin(ctx, admin))
	require.NoError(t, c.EnsureAgentClassStarters(ctx, ns, name, []string{"user:" + listed.String()}))

	cases := []struct {
		perm  string
		who   identity.CanonicalUserID
		role  string
		grant bool
	}{
		{"start_session", admin, "platform admin", true},
		{"start_explicit", admin, "platform admin", false},
		{"start_session", listed, "listed starter", true},
		{"start_explicit", listed, "listed starter", true},
		{"start_session", stranger, "stranger", false},
		{"start_explicit", stranger, "stranger", false},
	}
	for _, tc := range cases {
		verdict := "refused"
		if tc.grant {
			verdict = "granted"
		}
		t.Run(tc.perm+" for "+tc.role+": "+verdict, func(t *testing.T) {
			ok, err := c.CheckAgentClassStart(ctx, ns, name, tc.perm, tc.who)
			require.NoError(t, err)
			assert.Equal(t, tc.grant, ok, "%s#%s for %s", name, tc.perm, tc.who)
		})
	}
}
