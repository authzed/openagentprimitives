// pkg/controllers/channel/owner_derivation_test.go
//
// Untagged (unit tier): a Channel whose kind names no starting user needs an
// owner from somewhere, and the Channel controller is where that is refused at
// apply time.
//
// The live shape: a freshly installed webhook-driven agent sat at
// Valid=False/ChannelSpecInvalid — "ownerless input requires spec.owner.explicit
// or spec.owner.ownerless" — until a human wrote fromOutputChannel: true by
// hand. Nothing in a checked-in bundle can carry that decision's INPUT (the
// Slack channel id is per-install), but the decision itself is derivable: the
// people in the channel the agent posts into are the people who own what it does
// there. This file pins that the controller reaches it, and every boundary at
// which it must not.
//
// The refusal's own coverage — that it still fires, in its existing words, when
// there is nothing to derive from — lives here too, because a derivation that
// quietly widens the accepted configurations is the failure mode that matters.
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
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// ownerlessRefusal is the message the rule has always produced. Asserted
// verbatim: "nothing to derive from" must land on the SAME refusal a human
// already knows how to act on, not a new one that reads like a different bug.
const ownerlessRefusal = "ownerless input requires spec.owner.explicit or spec.owner.ownerless (permission / fromOutputChannel)"

const derivedChannelOwner = "slack_channel:C0DEMO123#member"

// bentoInput is the Channel under test: role=input on a kind that provides no
// starting user, carrying whatever owner policy the case declares.
func bentoInput(owner *spiceboxv1alpha1.ChannelOwnerPolicy) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "bento",
			Role:           spiceboxv1alpha1.ChannelRoleInput,
			AgentClass:     "demo-cls",
			AuthzSubject:   "service:demo-cron",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "bento-creds"},
			Bento: &spiceboxv1alpha1.BentoChannelConfig{
				Generate: &spiceboxv1alpha1.BentoGenerateConfig{
					Mapping:  `root = "run the digest"`,
					Interval: "60s",
				},
			},
			Owner: owner,
		},
	}
}

// slackOutputSibling is the role=output half the reply lands in — the only
// per-install source of a membership subject-set. channelID "" omits
// outputDefaults, the state in which the kind yields no membership at all.
func slackOutputSibling(channelID string) *spiceboxv1alpha1.Channel {
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-out", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "slack",
			Role:           spiceboxv1alpha1.ChannelRoleOutput,
			AgentClass:     "demo-cls",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "slack-creds"},
			Slack:          &spiceboxv1alpha1.SlackChannelConfig{},
		},
	}
	if channelID != "" {
		ch.Spec.Slack.OutputDefaults = &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: channelID}
	}
	return ch
}

func bentoCreds(t *testing.T) *corev1.Secret {
	t.Helper()
	// bento's RequiredSecretKeys returns nil; an empty adopted Secret suffices.
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "bento-creds"},
		Data:       map[string][]byte{},
	}
	adoptguard.WithAdoptedLabel(sec)
	return sec
}

// reconcileOwnerDerivation reconciles the Channel named "ch" and returns it,
// so a case can read both the Valid condition and the derived-owner status
// field off the same object.
func reconcileOwnerDerivation(t *testing.T, objects ...client.Object) *spiceboxv1alpha1.Channel {
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
	return &got
}

// TestValidate_OwnerDerivedFromOutputChannel drives the one rule against every
// combination of what the Channel declared, what the output sibling offers, and
// what the class's ownerCeiling permits.
func TestValidate_OwnerDerivedFromOutputChannel(t *testing.T) {
	cases := []struct {
		name string
		// owner is the Channel's declared spec.owner (nil = declares nothing).
		owner *spiceboxv1alpha1.ChannelOwnerPolicy
		// ceiling is the class's spec.authz.ownerCeiling.
		ceiling *spiceboxv1alpha1.OwnerCeiling
		// outChannelID configures the role=output sibling; "none" omits the
		// sibling entirely.
		outChannelID string
		omitSibling  bool
		wantStatus   metav1.ConditionStatus
		wantReason   string
		wantMsg      string
		// wantDerivedOwner is status.derivedSessionOwner; empty means the
		// derivation must not have fired.
		wantDerivedOwner string
	}{
		{
			name:             "declares nothing + a resolvable output channel: Valid, owner derived",
			outChannelID:     "C0DEMO123",
			wantStatus:       metav1.ConditionTrue,
			wantReason:       spiceboxv1alpha1.ReasonChannelAllReferencesResolve,
			wantDerivedOwner: derivedChannelOwner,
		},
		{
			// The regression direction. An operator who named an owner keeps it,
			// and nothing derived is published alongside to confuse the reading.
			name:         "declares explicit: Valid, and nothing is derived",
			owner:        &spiceboxv1alpha1.ChannelOwnerPolicy{Explicit: "group:demo-maintainers#member"},
			outChannelID: "C0DEMO123",
			wantStatus:   metav1.ConditionTrue,
			wantReason:   spiceboxv1alpha1.ReasonChannelAllReferencesResolve,
		},
		{
			name:         "declares ownerless.permission: Valid, and nothing is derived",
			owner:        &spiceboxv1alpha1.ChannelOwnerPolicy{Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{Permission: "team:reviewers#member"}},
			outChannelID: "C0DEMO123",
			wantStatus:   metav1.ConditionTrue,
			wantReason:   spiceboxv1alpha1.ReasonChannelAllReferencesResolve,
		},
		{
			name:         "declares ownerless.fromOutputChannel: Valid, and nothing is derived",
			owner:        &spiceboxv1alpha1.ChannelOwnerPolicy{Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{FromOutputChannel: true}},
			outChannelID: "C0DEMO123",
			wantStatus:   metav1.ConditionTrue,
			wantReason:   spiceboxv1alpha1.ReasonChannelAllReferencesResolve,
		},
		{
			// Nothing to derive FROM: no output Channel exists at all.
			name:        "no output sibling: the existing refusal, unchanged",
			omitSibling: true,
			wantStatus:  metav1.ConditionFalse,
			wantReason:  spiceboxv1alpha1.ReasonChannelSpecInvalid,
			wantMsg:     ownerlessRefusal,
		},
		{
			// The output Channel exists but its kind can name no membership,
			// which is the same missing field that leaves it with nowhere to
			// post. Refused on the owner rule first, in its own words.
			name:         "output sibling with no channelId: the existing refusal, unchanged",
			outChannelID: "",
			wantStatus:   metav1.ConditionFalse,
			wantReason:   spiceboxv1alpha1.ReasonChannelSpecInvalid,
			wantMsg:      ownerlessRefusal,
		},
		{
			// The admin veto. starterOnly means the owner must be the session's
			// starting user; this kind supplies none, so the Channel must be
			// refused rather than handed a whole channel's membership.
			name:         "ownerCeiling.starterOnly: refused, never derived around",
			ceiling:      &spiceboxv1alpha1.OwnerCeiling{StarterOnly: true},
			outChannelID: "C0DEMO123",
			wantStatus:   metav1.ConditionFalse,
			wantReason:   spiceboxv1alpha1.ReasonChannelSpecInvalid,
			wantMsg:      ownerlessRefusal,
		},
		{
			name:         "ownerCeiling.fixed: refused; the admin already named the owner",
			ceiling:      &spiceboxv1alpha1.OwnerCeiling{Fixed: "group:sec#member"},
			outChannelID: "C0DEMO123",
			wantStatus:   metav1.ConditionFalse,
			wantReason:   spiceboxv1alpha1.ReasonChannelSpecInvalid,
			wantMsg:      ownerlessRefusal,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cls := newValidAgentClass("demo-cls", func(ac *spiceboxv1alpha1.AgentClass) {
				if tc.ceiling != nil {
					ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{OwnerCeiling: tc.ceiling}
				}
			})
			objs := []client.Object{cls, bentoInput(tc.owner), bentoCreds(t)}
			if !tc.omitSibling {
				objs = append(objs, slackOutputSibling(tc.outChannelID))
			}

			got := reconcileOwnerDerivation(t, objs...)
			cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ChannelConditionValid)
			require.NotNil(t, cond, "Valid condition must be set")
			assert.Equal(t, tc.wantStatus, cond.Status, "Valid status; msg=%q", cond.Message)
			assert.Equal(t, tc.wantReason, cond.Reason, "Valid reason; msg=%q", cond.Message)
			if tc.wantMsg != "" {
				assert.Equal(t, tc.wantMsg, cond.Message,
					"a refusal with nothing to derive from must keep its existing words")
			}
			assert.Equal(t, tc.wantDerivedOwner, got.Status.DerivedSessionOwner,
				"status.derivedSessionOwner")
		})
	}
}

// TestValidate_OwnerDerivationSkipsKindsThatNameAStarter is the
// over-derivation control. A kind that attributes its inbound to a human needs
// no owner source at all, and publishing one would misstate who owns those
// sessions: the starter outranks the channel's membership at resolve time, so a
// derived value on status would contradict the tuple SpiceDB actually holds.
func TestValidate_OwnerDerivationSkipsKindsThatNameAStarter(t *testing.T) {
	slackCreds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "slack-creds"},
		Data: map[string][]byte{
			"bot-token": []byte("xoxb-test"),
			"app-token": []byte("xapp-test"),
		},
	}
	adoptguard.WithAdoptedLabel(slackCreds)

	slackBoth := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "slack",
			Role:           spiceboxv1alpha1.ChannelRoleBoth,
			AgentClass:     "demo-cls",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "slack-creds"},
			Slack: &spiceboxv1alpha1.SlackChannelConfig{
				OutputDefaults: &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C0DEMO123"},
			},
		},
	}

	got := reconcileOwnerDerivation(t, newValidAgentClass("demo-cls", nil), slackBoth, slackCreds)
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ChannelConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "msg=%q", cond.Message)
	assert.Empty(t, got.Status.DerivedSessionOwner,
		"a kind that names a starting user needs no derived owner, and must not advertise one")
}
