package agentsession_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	memorypkg "github.com/authzed/openagentprimitives/pkg/memory"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack" // contributes the slack_channel / slack_usergroup session links
)

// forkPair builds a parent carrying perm as its snapshotted interact policy and
// the child the fork would grant it on.
func forkPair(t *testing.T, perm string) (parent, child *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	parent = &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p", Namespace: "ns",
			Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice"},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{AppliedInteractPermission: perm},
	}
	child = &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{
		Name: "p-fk", Namespace: "ns",
		Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice"},
	}}
	return parent, child
}

// The live defect: an operator may legitimately write
// authz.session.interactPermission as a Slack channel's membership, and the
// AgentClass reconciler now DERIVES exactly that value for a class whose input
// carries no human. It snapshots onto status.appliedInteractPermission, and the
// fork must be able to grant it — the composed schema admits it, because slack's
// SessionRelationLinks puts slack_channel#member on agentsession#participant.
func TestWriteSpiceDBParticipants_AdmitsASlackChannelMembership(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	g := &fakeGranter{}
	parent, child := forkPair(t, "slack_channel:C0DEMO123#member")

	require.NoError(t, agentsession.WriteSpiceDBParticipants(ctx, g, parent, child),
		"a subject type the composed schema admits must survive a fork")
	assert.Equal(t, []string{"ns/p-fk|slack_channel:C0DEMO123#member"}, g.interactParts)
}

// The seam. The admissible set is DERIVED from the same registry lookup the
// guardian composer unions into the live schema, so every link this build's
// kinds declare is grantable, and a kind registered tomorrow is covered without
// editing anything here. Breaking the shared lookup reddens this test.
func TestWriteSpiceDBParticipants_AdmitsEverySessionLinkTheRegistryDeclares(t *testing.T) {
	links := registry.SessionRelationLinks()
	require.NotEmpty(t, links, "at least one registered kind must declare a session link")

	for _, link := range links {
		typ, relation, ok := strings.Cut(link, "#")
		require.Truef(t, ok, "link %q must be of the form <type>#<relation>", link)

		t.Run(link, func(t *testing.T) {
			ctx := memorypkg.WithSystemApproval(context.Background(), "test")
			g := &fakeGranter{}
			perm := typ + ":C0DEMO123#" + relation
			parent, child := forkPair(t, perm)

			require.NoError(t, agentsession.WriteSpiceDBParticipants(ctx, g, parent, child),
				"%q is unioned into agentsession#participant by the composer; the fork must admit it", perm)
			assert.Equal(t, []string{"ns/p-fk|" + perm}, g.interactParts)
		})
	}
}

// The fail-closed direction, and the one that matters most: widening to "a type
// the schema might have" would hand an unregistered object type an interact
// grant — and interact is read access to the transcript the fork just inherited.
// A type no registered kind contributes and the scaffold does not declare is
// refused, whatever its shape.
func TestWriteSpiceDBParticipants_RefusesASubjectTypeNothingRegistered(t *testing.T) {
	cases := []struct {
		name string
		perm string
	}{
		{
			name: "a plausible link type from a kind this build does not have: refused",
			perm: "linear_team:T0DEMO456#member",
		},
		{
			name: "a well-formed type the scaffold declares but not on participant: refused",
			perm: "agentsession:ns/other#owner",
		},
		{
			name: "a type that merely looks like a registered one: refused",
			perm: "slack_channel_admin:C0DEMO123#member",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := memorypkg.WithSystemApproval(context.Background(), "test")
			g := &fakeGranter{}
			parent, child := forkPair(t, tc.perm)

			err := agentsession.WriteSpiceDBParticipants(ctx, g, parent, child)
			require.Errorf(t, err, "interact permission %q must abort the fork", tc.perm)
			assert.Empty(t, g.interactParts, "no participant write on a refused subject type")
		})
	}
}

// The base vocabulary is the scaffold's, not a kind's, and deriving from the
// registry must not lose it: an ordinary group policy — every install that
// predates channel-contributed link types — still forks.
func TestWriteSpiceDBParticipants_StillAdmitsTheScaffoldGroupVocabulary(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	g := &fakeGranter{}
	parent, child := forkPair(t, "group:eng#member")

	require.NoError(t, agentsession.WriteSpiceDBParticipants(ctx, g, parent, child))
	assert.Equal(t, []string{"ns/p-fk|group:eng#member"}, g.interactParts)
}
