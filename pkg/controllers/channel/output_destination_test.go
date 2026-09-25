// pkg/controllers/channel/output_destination_test.go
//
// Untagged (unit tier): a role=output Channel bound to an AgentClass whose
// input carries no human has nothing inbound to reply to, so it must carry its
// own destination. The refusal has to land at apply time: unrefused, such a
// Channel reports Valid=True with nowhere to post, and the failure waits until
// the agent finishes a review to show itself.
//
// The mirror-image rule on the OTHER half of the pairing — a role=input Channel
// whose reply target does not resolve — is TestValidate_OutputBindingForRoleInput
// in controller_unit_test.go. Both ask outputbind the same question about the
// same Channel; this one reports the answer on the object a human has to edit.
package channel_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// slackOutputCh builds the role=output Slack Channel of the reviewbot pairing:
// the half the agent's finished work is delivered into. A nil od reproduces the
// live misconfiguration — a Channel with no destination at all.
func slackOutputCh(t *testing.T, od *spiceboxv1alpha1.SlackOutputDefaults) *spiceboxv1alpha1.Channel {
	t.Helper()
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "slack",
			Role:           spiceboxv1alpha1.ChannelRoleOutput,
			AgentClass:     "agent-cls",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "slack-creds"},
			Slack:          &spiceboxv1alpha1.SlackChannelConfig{OutputDefaults: od},
		},
	}
}

// userlessClass builds the Valid=True AgentClass fixture with the derived
// status.userlessInput fact set as given. Set directly rather than reconciled:
// the AgentClass reconciler's own derivation is pinned in
// pkg/controllers/agentclass (TestReconcile_DerivesUserlessInput); what this
// file tests is that the Channel controller READS the published answer instead
// of re-deriving it.
func userlessClass(userless bool) *spiceboxv1alpha1.AgentClass {
	return newValidAgentClass("agent-cls", func(ac *spiceboxv1alpha1.AgentClass) {
		ac.Status.UserlessInput = userless
	})
}

func slackOutputCreds(t *testing.T) *corev1.Secret {
	t.Helper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "slack-creds"},
		Data: map[string][]byte{
			"bot-token": []byte("xoxb-test"),
			"app-token": []byte("xapp-test"),
		},
	}
	adoptguard.WithAdoptedLabel(sec)
	return sec
}

// reconcileChannelCondition reconciles the Channel named "ch" and returns its
// Valid condition.
func reconcileChannelCondition(t *testing.T, objects ...client.Object) *metav1.Condition {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&spiceboxv1alpha1.Channel{}).
		Build()
	r := newUnitReconciler(c)
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: "default", Name: "ch"},
	})
	require.NoError(t, err)
	var got spiceboxv1alpha1.Channel
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "ch"}, &got))
	return meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ChannelConditionValid)
}

// TestValidate_OutputChannelNeedsDestinationWhenInputIsUserless drives the one
// axis that decides the rule — the AgentClass's derived status.userlessInput —
// against a destination that is present, absent, or half-configured.
//
// The userless=false rows are the over-restriction control and matter as much
// as the refusals: an output Channel bound to a class whose input DOES carry a
// human is fine without a destination, because the inbound message supplies
// one.
func TestValidate_OutputChannelNeedsDestinationWhenInputIsUserless(t *testing.T) {
	cases := []struct {
		name       string
		userless   bool
		od         *spiceboxv1alpha1.SlackOutputDefaults
		wantStatus metav1.ConditionStatus
		wantReason string
		wantMsg    string
	}{
		{
			name:       "userless input + channelId set: Valid=True",
			userless:   true,
			od:         &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C1"},
			wantStatus: metav1.ConditionTrue,
			wantReason: spiceboxv1alpha1.ReasonChannelAllReferencesResolve,
		},
		{
			name:       "userless input + no outputDefaults: Valid=False naming channelId",
			userless:   true,
			od:         nil,
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonChannelOutputDestinationMissing,
			wantMsg:    "spec.slack.outputDefaults.channelId",
		},
		{
			name:       "userless input + empty channelId: Valid=False naming channelId",
			userless:   true,
			od:         &spiceboxv1alpha1.SlackOutputDefaults{ThreadStrategy: "new-thread-per-session"},
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonChannelOutputDestinationMissing,
			wantMsg:    "spec.slack.outputDefaults.channelId",
		},
		{
			// A channelId alone is not a destination under this strategy: the
			// anchor needs the thread it must post into.
			name:       "userless input + static-thread without staticThreadTs: Valid=False naming staticThreadTs",
			userless:   true,
			od:         &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C1", ThreadStrategy: "static-thread"},
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonChannelOutputDestinationMissing,
			wantMsg:    "spec.slack.outputDefaults.staticThreadTs",
		},
		{
			name:       "user-attributable input + no outputDefaults: Valid=True (the inbound supplies the destination)",
			userless:   false,
			od:         nil,
			wantStatus: metav1.ConditionTrue,
			wantReason: spiceboxv1alpha1.ReasonChannelAllReferencesResolve,
		},
		{
			name:       "user-attributable input + channelId set: Valid=True",
			userless:   false,
			od:         &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C1"},
			wantStatus: metav1.ConditionTrue,
			wantReason: spiceboxv1alpha1.ReasonChannelAllReferencesResolve,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cond := reconcileChannelCondition(t,
				userlessClass(tc.userless), slackOutputCh(t, tc.od), slackOutputCreds(t))
			require.NotNil(t, cond, "Valid condition must be set")
			assert.Equal(t, tc.wantStatus, cond.Status, "Valid status; msg=%q", cond.Message)
			assert.Equal(t, tc.wantReason, cond.Reason, "Valid reason; msg=%q", cond.Message)
			if tc.wantMsg != "" {
				assert.Contains(t, cond.Message, tc.wantMsg,
					"the refusal must name the field a human has to fill in")
			}
		})
	}
}

// TestValidate_OutputDestinationGatesOwnerGroupRef pins two consequences of the
// same missing field together, because they are one bug and were found as two.
//
// The slack kind's OwnerGroupRef — the source behind
// spec.owner.ownerless.fromOutputChannel — returns nothing unless
// outputDefaults.channelId is set. So a destination-less output Channel did not
// merely have nowhere to post: it also silently disabled itself as an owner
// source, with no refusal anywhere. The new validation makes that state
// unreachable, and this test is what would notice if the two ever came apart
// (a refusal keyed off a field OwnerGroupRef does not read, or vice versa).
func TestValidate_OutputDestinationGatesOwnerGroupRef(t *testing.T) {
	t.Run("no channelId: refused AND no owner group ref", func(t *testing.T) {
		ch := slackOutputCh(t, nil)
		cond := reconcileChannelCondition(t, userlessClass(true), ch, slackOutputCreds(t))
		require.NotNil(t, cond)
		require.Equal(t, metav1.ConditionFalse, cond.Status, "msg=%q", cond.Message)
		assert.Equal(t, spiceboxv1alpha1.ReasonChannelOutputDestinationMissing, cond.Reason)
		assert.Empty(t, chregistry.OwnerGroupRefForChannel(ch),
			"the same missing field also disables fromOutputChannel as an owner source")
	})

	t.Run("channelId set: Valid AND an owner group ref resolves", func(t *testing.T) {
		ch := slackOutputCh(t, &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C1"})
		cond := reconcileChannelCondition(t, userlessClass(true), ch, slackOutputCreds(t))
		require.NotNil(t, cond)
		require.Equal(t, metav1.ConditionTrue, cond.Status, "msg=%q", cond.Message)
		assert.Equal(t, "slack_channel:C1#member", chregistry.OwnerGroupRefForChannel(ch),
			"a Channel that passes the destination rule always has an owner group ref")
	})
}

// TestValidate_OutputDestinationRuleIsNotWidened guards the two roles the rule
// deliberately does not reach.
//
// role=both is self-contained — it is its own origin and destination, and the
// inbound it receives carries the thread to reply into — so it asserts nothing
// about a configured destination even when the class also has a userless input.
// role=monitoring has its own destination rule for its own reason (a
// framework-event sink bound to no agent, so nothing inbound ever exists); it
// is covered by TestChannelValidate_MonitoringRole and must keep its own
// reason and message rather than being folded into this one.
func TestValidate_OutputDestinationRuleIsNotWidened(t *testing.T) {
	t.Run("role=both with no outputDefaults on a userless-input class: Valid=True", func(t *testing.T) {
		both := slackOutputCh(t, nil)
		both.Spec.Role = spiceboxv1alpha1.ChannelRoleBoth
		cond := reconcileChannelCondition(t, userlessClass(true), both, slackOutputCreds(t))
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status, "msg=%q", cond.Message)
	})

	t.Run("role=monitoring with no outputDefaults keeps its own reason and message", func(t *testing.T) {
		mon := slackOutputCh(t, nil)
		mon.Spec.Role = spiceboxv1alpha1.ChannelRoleMonitoring
		mon.Spec.AgentClass = ""
		cond := reconcileChannelCondition(t, userlessClass(true), mon, slackOutputCreds(t))
		require.NotNil(t, cond)
		require.Equal(t, metav1.ConditionFalse, cond.Status, "msg=%q", cond.Message)
		assert.Equal(t, spiceboxv1alpha1.ReasonChannelSpecInvalid, cond.Reason)
		assert.Contains(t, cond.Message, `role="monitoring"`)
	})
}
