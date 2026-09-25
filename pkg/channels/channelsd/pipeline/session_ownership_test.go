package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack" // supplies OwnerGroupRef for the output Channel
)

// ownerlessSessionFixture returns the pieces resolveSessionOwnership needs for
// a session nobody started: a slack output Channel naming a membership, an
// input Channel that declared no owner policy at all, and a class carrying
// ceiling.
func ownerlessSessionFixture(t *testing.T, ceiling *spiceboxv1alpha1.OwnerCeiling) (
	client.Reader, *spiceboxv1alpha1.AgentSession, *spiceboxv1alpha1.Channel, *spiceboxv1alpha1.AgentClass,
) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	outCh := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "out", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "slack",
			Slack: &spiceboxv1alpha1.SlackChannelConfig{
				OutputDefaults: &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C0DEMO123"},
			},
		},
	}
	inCh := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "in", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "github"},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:         "ac1",
			OutputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "out", Kind: "slack"},
		},
	}
	ac := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"}}
	if ceiling != nil {
		ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{OwnerCeiling: ceiling}
	}

	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(outCh, inCh).Build()
	return cli, sess, inCh, ac
}

// The disclosure must be gated by the same thing the operator's write is: the
// class's ownerCeiling. Gating on the Channel's declared fromOutputChannel flag
// instead meant a session whose owner was DERIVED — a Channel that declared
// nothing, which is exactly the case the derivation exists for — had a
// whole-channel owner written by the operator and nothing said about it in the
// room. Silence is the safe direction, but it is still a room that was never
// told who owns the session announced in it.
func TestResolveSessionOwnership_DisclosesADerivedOutputChannelOwner(t *testing.T) {
	cli, sess, inCh, ac := ownerlessSessionFixture(t, nil)

	got := resolveSessionOwnership(context.Background(), cli, sess, inCh, ac)

	assert.Equal(t, "slack_channel:C0DEMO123#member", got.Subject,
		"the same subject pkg/controllers/agentsession's owner resolver writes for this session")
	assert.True(t, got.Collective, "a channel membership is a population, and the room must be told so")
}

// The ceiling is an admin veto the operator honours, so the disclosure must
// honour it too — and "honour" now means two different things depending on
// which ceiling it is.
//
// starterOnly admits exactly one owner, the starting user; this fixture has
// none, so there is no owner to claim and silence is correct.
//
// fixed NAMES the owner, so the honest disclosure is that owner. It used to be
// silence only because the resolver could not see the ceiling: it fell through
// to the starter (or, here, to a derived channel membership) and the caller
// suppressed the claim to avoid announcing something the operator had refused
// to write. The operator no longer refuses — it writes the pinned subject — so
// announcing it is what keeps the two from drifting, which is the whole
// contract of this file.
func TestResolveSessionOwnership_HonoursTheCeiling(t *testing.T) {
	cases := []struct {
		name           string
		ceiling        *spiceboxv1alpha1.OwnerCeiling
		wantSubject    string
		wantCollective bool
	}{
		{name: "starterOnly: no claim, since a session with no starter has no owner this ceiling admits",
			ceiling: &spiceboxv1alpha1.OwnerCeiling{StarterOnly: true}},
		{name: "fixed: the pinned owner IS the claim, matching what the operator writes",
			ceiling:     &spiceboxv1alpha1.OwnerCeiling{Fixed: "group:platform#member"},
			wantSubject: "group:platform#member",
			// Not flagged collective, matching how an explicit Channel owner
			// behaves: IsCollectiveOwnership keys on the SOURCE, and both
			// sources can name either an individual or a subject-set. That
			// under-warns for a pinned group, but it is pre-existing and
			// shared with explicit — worth its own change, not this one.
			wantCollective: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli, sess, inCh, ac := ownerlessSessionFixture(t, tc.ceiling)

			got := resolveSessionOwnership(context.Background(), cli, sess, inCh, ac)

			assert.Equal(t, tc.wantSubject, got.Subject)
			assert.Equal(t, tc.wantCollective, got.Collective)
		})
	}
}

// A starting user outranks both ownerless sources, so a kind that attributes
// its inbound to a person still discloses that person and not the room.
func TestResolveSessionOwnership_AStarterOutranksTheOutputChannel(t *testing.T) {
	cli, sess, inCh, ac := ownerlessSessionFixture(t, nil)
	sess.Annotations = map[string]string{
		spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice",
	}

	got := resolveSessionOwnership(context.Background(), cli, sess, inCh, ac)

	assert.Equal(t, "user:alice", got.Subject)
	assert.False(t, got.Collective, "an individual owner is not a population")
}
