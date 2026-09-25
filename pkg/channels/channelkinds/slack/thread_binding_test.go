package slack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// slackChannelCR builds a slack Channel CR with the two fields that decide
// whether two Channels are the same agent behind the same Slack app.
func slackChannelCR(t *testing.T, name, agentClass, credSecret string) *spiceboxv1alpha1.Channel {
	t.Helper()
	ch := &spiceboxv1alpha1.Channel{}
	ch.Namespace, ch.Name = "default", name
	ch.Spec.Kind = KindName
	ch.Spec.AgentClass = agentClass
	ch.Spec.CredentialsRef.SecretName = credSecret
	return ch
}

// cronSessionOn builds a cron-spawned AgentSession whose slack thread anchor
// lives on spec.outputChannel (the shape channelsd's outbound relay labels
// with LabelOutputChannelKey).
func cronSessionOn(t *testing.T, name, outChannelName, channelID, threadTS string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	key := "thread:" + channelID + ":" + threadTS
	sess := &spiceboxv1alpha1.AgentSession{}
	sess.Namespace, sess.Name = "default", name
	sess.Labels = map[string]string{
		spiceboxv1alpha1.LabelOutputChannelKey: channelkey.LabelValue(key),
	}
	sess.Spec.OutputChannel = &spiceboxv1alpha1.ChannelBinding{
		Name: outChannelName, Kind: KindName, Key: key,
		External: map[string]string{"channel_id": channelID, "thread_ts": threadTS},
	}
	return sess
}

// A thread belongs 1:1 to the Channel its session is bound to. Another agent
// must not claim it just because that agent's Slack app also happens to be a
// member of the same Slack conversation.
//
// Observed in a live cluster: three Slack apps (three Channel CRs, three
// different agents) were all members of one Slack channel. A cron agent posted
// a thread there; every reply in that thread was claimed by all three
// listeners, so one human message was delivered into the session three times
// and the thread's status line was overwritten with a different agent's name.
//
// The permitted exception is a *sibling* Channel: the same agent behind the
// same Slack app (the standard cron layout — an output-only Channel plus the
// interactive one). Slack delivers a given event to exactly one socket
// connection per app, so a sibling can never double-deliver.
func TestMayClaimThread(t *testing.T) {
	const (
		hubspotSecret = "slack-hubspot-companies-creds"
		demoSecret    = "demo-channel-creds"
	)

	cases := []struct {
		name  string
		own   *spiceboxv1alpha1.Channel
		bound *spiceboxv1alpha1.Channel
		want  bool
	}{
		{
			name:  "the bound Channel itself: claims its own thread",
			own:   slackChannelCR(t, "hubspot-cron-output", "hubspot-companies", hubspotSecret),
			bound: slackChannelCR(t, "hubspot-cron-output", "hubspot-companies", hubspotSecret),
			want:  true,
		},
		{
			name:  "sibling Channel, same agent + same Slack app: claims (the cron layout)",
			own:   slackChannelCR(t, "slack-hubspot-companies", "hubspot-companies", hubspotSecret),
			bound: slackChannelCR(t, "hubspot-cron-output", "hubspot-companies", hubspotSecret),
			want:  true,
		},
		{
			name:  "different agent, different Slack app: refused (the production bug)",
			own:   slackChannelCR(t, "demo-channel", "demo-agent", demoSecret),
			bound: slackChannelCR(t, "hubspot-cron-output", "hubspot-companies", hubspotSecret),
			want:  false,
		},
		{
			name:  "same Slack app fronting a different agent: refused",
			own:   slackChannelCR(t, "shared-app-other-agent", "codebot", hubspotSecret),
			bound: slackChannelCR(t, "hubspot-cron-output", "hubspot-companies", hubspotSecret),
			want:  false,
		},
		{
			name:  "same agent behind a second Slack app: refused (both apps would deliver)",
			own:   slackChannelCR(t, "hubspot-second-app", "hubspot-companies", demoSecret),
			bound: slackChannelCR(t, "hubspot-cron-output", "hubspot-companies", hubspotSecret),
			want:  false,
		},
		{
			name:  "both credentialsRef unset: refused, absence is not sameness",
			own:   slackChannelCR(t, "no-creds-a", "hubspot-companies", ""),
			bound: slackChannelCR(t, "no-creds-b", "hubspot-companies", ""),
			want:  false,
		},
		{
			name:  "both agentClass unset: refused, absence is not sameness",
			own:   slackChannelCR(t, "no-class-a", "", hubspotSecret),
			bound: slackChannelCR(t, "no-class-b", "", hubspotSecret),
			want:  false,
		},
		{
			name:  "bound Channel unresolvable: refused, fail closed",
			own:   slackChannelCR(t, "slack-hubspot-companies", "hubspot-companies", hubspotSecret),
			bound: nil,
			want:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mayClaimThread(tc.own, tc.bound))
		})
	}
}

// End-to-end through threadIsOwned against a fake client: the exact production
// shape. A different agent's listener must not claim the hubspot agent's cron
// thread — not on the first message in it, and not on any message after.
//
// The repeat matters twice over. Correctness: a refusal that did not stick
// would be re-decided per message, and any path that folded the thread into
// the index as OWNED would make every later reply skip the check entirely.
// Cost: the lookup behind it runs against an uncached client, so re-deciding
// per message means a List and a Get to the apiserver for every message in
// every foreign thread.
func TestThreadOwnership_ForeignAgentCannotClaimAnotherAgentsThread(t *testing.T) {
	const channelID, threadTS = "C0B0FCX9U7Q", "1785524177.433129"

	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	var lists, gets int
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		cronSessionOn(t, "hubspot-cron-input-f22819e3", "hubspot-cron-output", channelID, threadTS),
		slackChannelCR(t, "hubspot-cron-output", "hubspot-companies", "slack-hubspot-companies-creds"),
		slackChannelCR(t, "demo-channel", "demo-agent", "demo-channel-creds"),
	).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			lists++
			return c.List(ctx, l, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			gets++
			return c.Get(ctx, key, o, opts...)
		},
	}).Build()

	l := &slackListener{threads: newThreadIndex()}
	l.deps.K8sClient = cli
	l.deps.Channel = slackChannelCR(t, "demo-channel", "demo-agent", "demo-channel-creds")

	owned, _ := l.threadIsOwned(context.Background(), channelID, threadTS)
	assert.False(t, owned, "a different agent's Slack app must not claim this thread")

	resolved := lists + gets
	require.Positive(t, resolved, "precondition: the first decision consults the apiserver")

	for i := 0; i < 3; i++ {
		owned, _ = l.threadIsOwned(context.Background(), channelID, threadTS)
		require.False(t, owned, "the refusal must hold for every later message in the thread")
	}
	assert.Equal(t, resolved, lists+gets,
		"later messages must be answered from the index, not re-queried against the apiserver")
}

// handleChannelMessage registers the thread in the index BEFORE Deliver
// decides anything, so future message.channels events route to this listener.
// When the pipeline then answers "that thread belongs to another agent", the
// eager registration must be withdrawn.
//
// Left in place it is strictly worse than never having checked: a positive
// index entry is the fast path, so every later message in that thread
// short-circuits threadIsOwned and never reaches the ownership check at all.
func TestHandleChannelMessage_RefusedThreadIsWithdrawnFromTheIndex(t *testing.T) {
	const channelID, anchor = "C0B0FCX9U7Q", "1785524177.433129"

	api, _ := recordingSlackAPI(t)
	l := &slackListener{threads: newThreadIndex(), api: api}
	l.deps.Channel = slackChannelCR(t, "demo-channel", "demo-agent", "demo-channel-creds")
	l.deps.Inbound = fixedInbound{dec: channelkinds.InboundDecision{
		Outcome: channelkinds.OutcomeThreadOwnedByAnotherAgent,
	}}

	l.handleChannelMessage(context.Background(), "U1", channelID, anchor, "11.0", "a reply", nil, false, originHuman)

	_, refused, known := l.threads.lookup(channelID + ":" + anchor)
	require.True(t, known, "the thread was resolved, so the decision is remembered")
	assert.True(t, refused,
		"a thread the pipeline says belongs to another agent must be recorded as refused, not owned")

	owned, _ := l.threadIsOwned(context.Background(), channelID, anchor)
	assert.False(t, owned, "and later messages in it must not route to this listener")
}

// A threadIndex miss must NOT be treated as "not our thread" on its own — the
// sibling case c6040a35 fixed must keep working.
//
// The index is per-listener in-memory state, and a cron thread is written to
// exactly ONE listener: the one whose Channel is the session's OutputChannel
// (both SessionUpdated and repopulateThreadIndex walk 2 filter on
// out.Name == deps.Channel.Name). When several slack Channels share one Slack
// app — an output-only Channel plus the interactive one, the standard cron
// layout — the inbound is handled by whichever connection Slack delivers to.
// If that is not the listener holding the entry, a legitimate reply would be
// dropped as an "unowned thread".
//
// Observed in a live cluster as intermittent: the same thread routed once and
// was dropped minutes later with no restart in between. Routing correctness
// cannot depend on which of several equivalent listeners happens to receive
// the event, so an index miss falls back to the authoritative record — a
// session labelled with this thread's output key — and then checks that the
// listener is entitled to it.
func TestThreadOwnership_SiblingChannelStillClaims(t *testing.T) {
	const channelID, threadTS = "C0B0FCX9U7Q", "1785524177.433129"

	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		cronSessionOn(t, "hubspot-cron-input-f22819e3", "hubspot-cron-output", channelID, threadTS),
		slackChannelCR(t, "hubspot-cron-output", "hubspot-companies", "slack-hubspot-companies-creds"),
		slackChannelCR(t, "slack-hubspot-companies", "hubspot-companies", "slack-hubspot-companies-creds"),
	).Build()

	l := &slackListener{threads: newThreadIndex()}
	l.deps.K8sClient = cli
	l.deps.Channel = slackChannelCR(t, "slack-hubspot-companies", "hubspot-companies", "slack-hubspot-companies-creds")

	require.False(t, l.threads.has(channelID+":"+threadTS),
		"precondition: this listener has no index entry for the thread")

	owned, mode := l.threadIsOwned(context.Background(), channelID, threadTS)
	assert.True(t, owned,
		"a sibling Channel (same agent, same Slack app) must still recover the thread")
	assert.Equal(t, "", mode, "cron threads are bot-originated: default routing mode")
	assert.True(t, l.threads.has(channelID+":"+threadTS),
		"an accepted claim is folded into the index so the lookup is once per thread")
}
