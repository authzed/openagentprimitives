package slack

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
)

// countingClient records how many List calls reach the API server.
type countingClient struct {
	client.Client
	lists int
}

func (c *countingClient) List(ctx context.Context, l client.ObjectList, opts ...client.ListOption) error {
	c.lists++
	return c.Client.List(ctx, l, opts...)
}

// THE affordability property, and the reason the ordering inside
// maybeTriggerAmbient is not a style choice.
//
// The prefilter must gate the SESSION LOOKUP. Reversed, every Slack message in
// every thread with an active session costs a Kubernetes List to answer a
// question a few string comparisons answer for free — and the ambient trigger
// fires on every inbound turn, so that is the whole traffic of a busy channel.
//
// Asserted by COUNTING the lists, not by observing that nothing crashed. A
// first draft of this test used a nil client and NotPanics, which passes
// whichever order the code runs in: there is a nil-client guard after the
// prefilter that returns safely either way. It proved nothing.
func TestMaybeTriggerAmbient_ordinaryTurnsNeverReachTheSessionLookup(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	cc := &countingClient{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}

	l := &slackListener{}
	l.deps.K8sClient = cc
	l.deps.Channel = &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default"},
	}

	for _, turn := range []string{
		"thanks, that looks right",
		"what did the second test say?",
		"can you summarise the diff",
		"",
	} {
		l.maybeTriggerAmbient(context.Background(), "U1", "C1", "1.0", "1.0", turn)
	}
	assert.Zero(t, cc.lists,
		"an ordinary turn must be rejected by the prefilter BEFORE the session "+
			"lookup; otherwise every Slack message costs a Kubernetes List")
}

// The other half: a turn that DOES look like intent must reach the lookup, or
// the prefilter is simply refusing everything and the test above is vacuous.
func TestMaybeTriggerAmbient_anIntentTurnReachesTheSessionLookup(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	cc := &countingClient{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}

	l := &slackListener{}
	l.deps.K8sClient = cc
	l.deps.Channel = &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default"},
	}

	l.maybeTriggerAmbient(context.Background(), "U1", "C1", "1.0", "1.0",
		"you can also read the deploy logs")

	assert.Equal(t, 1, cc.lists, "a turn that looks like intent must be resolved")
}

// resolvedMetaagentTrigger is fail-safe at every absent link. A session whose
// settings have not resolved yet must NOT silently start classifying every
// turn — the narrowest value is the only safe default when the chain is
// incomplete.
func TestResolvedMetaagentTrigger_defaultsToMentionOnEveryAbsentLink(t *testing.T) {
	cases := []struct {
		name string
		sess *spiceboxv1alpha1.AgentSession
	}{
		{"nil session", nil},
		{"no effective settings", &spiceboxv1alpha1.AgentSession{}},
		{"no metaagent block", &spiceboxv1alpha1.AgentSession{
			Status: spiceboxv1alpha1.AgentSessionStatus{
				EffectiveSettings: &spiceboxv1alpha1.EffectiveSettings{},
			},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name+" resolves to mention", func(t *testing.T) {
			assert.Equal(t, spiceboxv1alpha1.MetaagentTriggerMention,
				resolvedMetaagentTrigger(tc.sess))
		})
	}
}

// The resolved values pass through, and an unrecognized one narrows.
func TestResolvedMetaagentTrigger_readsTheResolvedValue(t *testing.T) {
	withTrigger := func(v string) *spiceboxv1alpha1.AgentSession {
		return &spiceboxv1alpha1.AgentSession{
			Status: spiceboxv1alpha1.AgentSessionStatus{
				EffectiveSettings: &spiceboxv1alpha1.EffectiveSettings{
					Authz: spiceboxv1alpha1.EffectiveAuthz{
						Metaagent: &spiceboxv1alpha1.MetaagentConfig{Trigger: v},
					},
				},
			},
		}
	}

	assert.Equal(t, spiceboxv1alpha1.MetaagentTriggerShadow,
		resolvedMetaagentTrigger(withTrigger(spiceboxv1alpha1.MetaagentTriggerShadow)))
	assert.Equal(t, spiceboxv1alpha1.MetaagentTriggerInline,
		resolvedMetaagentTrigger(withTrigger(spiceboxv1alpha1.MetaagentTriggerInline)))
	assert.Equal(t, spiceboxv1alpha1.MetaagentTriggerMention,
		resolvedMetaagentTrigger(withTrigger("enabled")),
		"an unrecognized value must NARROW to mention, never widen to inline")
}
