package channel_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"  // register bento kind: role=input, no starting user
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"   // register fake kind for ValidateSpec/RequiredSecretKeys
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github" // register github kind: role=input, UserAttributable=false
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"  // register slack kind for ValidateSpec/RequiredSecretKeys
	"github.com/authzed/openagentprimitives/pkg/controllers/channel"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// newUnitReconciler constructs a channel.Reconciler with a SecretReader wired
// to the same fake client — suitable for unit tests that bypass envtest.
func newUnitReconciler(c client.Client) *channel.Reconciler {
	return &channel.Reconciler{
		Client:       c,
		SecretReader: adoptguard.NewSecretReader(c, c, adoptguard.Warn, func(types.NamespacedName) bool { return false }),
	}
}

// newValidAgentClass builds an AgentClass with Valid=True already stamped in
// Status.Conditions. The fake client preserves the full object (including
// status) from WithObjects, since AgentClass is not listed in
// WithStatusSubresource, so no separate status update is needed.
func newValidAgentClass(name string, mutate func(*spiceboxv1alpha1.AgentClass)) *spiceboxv1alpha1.AgentClass {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-opus-4-7",
				APIKey:   spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns:    50,
				MaxTokens:   100000,
				MaxDuration: metav1.Duration{Duration: time.Hour},
			},
		},
		Status: spiceboxv1alpha1.AgentClassStatus{
			Conditions: []metav1.Condition{{
				Type:               spiceboxv1alpha1.AgentClassConditionValid,
				Status:             metav1.ConditionTrue,
				Reason:             "Valid",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	if mutate != nil {
		mutate(ac)
	}
	return ac
}

func TestChannelValidate_MonitoringRole(t *testing.T) {
	scheme := testfixtures.NewScheme(t)

	monSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "mon-creds"},
		Data:       map[string][]byte{"bot-token": []byte("xoxb-test")},
	}
	adoptguard.WithAdoptedLabel(monSecret)

	t.Run("monitoring role, no AgentClass: Valid=True", func(t *testing.T) {
		ch := &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "mon"},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind:           "slack",
				Role:           spiceboxv1alpha1.ChannelRoleMonitoring,
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "mon-creds"},
				Slack: &spiceboxv1alpha1.SlackChannelConfig{
					OutputDefaults: &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C_MON"},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(ch, monSecret).
			WithStatusSubresource(&spiceboxv1alpha1.Channel{}).Build()
		r := newUnitReconciler(c)
		_, err := r.Reconcile(context.Background(), reconcile.Request{
			NamespacedName: client.ObjectKey{Namespace: "default", Name: "mon"},
		})
		require.NoError(t, err)

		var got spiceboxv1alpha1.Channel
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "mon"}, &got))
		cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ChannelConditionValid)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
	})

	t.Run("monitoring slack channel without channelId: Valid=False/SpecInvalid", func(t *testing.T) {
		// Slack ValidateSpec requires outputDefaults.channelId for monitoring channels.
		// The controller's validate() calls ValidateSpec before the monitoring early-
		// return, so this surfaces as ReasonChannelSpecInvalid. The controller also has
		// an explicit guard inside the monitoring block for defense-in-depth.
		ch := &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "mon-no-chan"},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind:           "slack",
				Role:           spiceboxv1alpha1.ChannelRoleMonitoring,
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "mon-creds"},
				Slack:          &spiceboxv1alpha1.SlackChannelConfig{
					// OutputDefaults intentionally absent — channelId required.
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(ch, monSecret).
			WithStatusSubresource(&spiceboxv1alpha1.Channel{}).Build()
		r := newUnitReconciler(c)
		_, err := r.Reconcile(context.Background(), reconcile.Request{
			NamespacedName: client.ObjectKey{Namespace: "default", Name: "mon-no-chan"},
		})
		require.NoError(t, err)

		var got spiceboxv1alpha1.Channel
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "mon-no-chan"}, &got))
		cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ChannelConditionValid)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, spiceboxv1alpha1.ReasonChannelSpecInvalid, cond.Reason)
		assert.Contains(t, cond.Message, "channelId", "message must mention the missing field")
	})

	t.Run("non-monitoring role, no AgentClass: Valid=False/SpecInvalid", func(t *testing.T) {
		ch := &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "bad"},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind:           "fake",
				Role:           spiceboxv1alpha1.ChannelRoleBoth,
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "mon-creds"},
				Fake:           &spiceboxv1alpha1.FakeChannelConfig{},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(ch, monSecret).
			WithStatusSubresource(&spiceboxv1alpha1.Channel{}).Build()
		r := newUnitReconciler(c)
		_, err := r.Reconcile(context.Background(), reconcile.Request{
			NamespacedName: client.ObjectKey{Namespace: "default", Name: "bad"},
		})
		require.NoError(t, err)

		var got spiceboxv1alpha1.Channel
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "bad"}, &got))
		cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ChannelConditionValid)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, spiceboxv1alpha1.ReasonChannelSpecInvalid, cond.Reason)
	})
}

// TestValidate_OwnerPolicy exercises the three owner-policy validation gates
// added to validate(): ownerless-coverage-gap, passthrough-breach, and
// ceiling-breach. All three use the fake-client path (no envtest).
//
// "fake" kind is used for ownerless/passthrough cases because it does NOT
// implement SessionOwnerProvider (no starter), so it reaches the
// ownerless-coverage gate without needing any real Slack credentials.
// "slack" kind is used for the ceiling-breach case because it DOES implement
// SessionOwnerProvider (has a starter), so the coverage gate passes and we
// reach the ceiling check.
// noStarterChannel builds the Channel used by the subtests whose subject is
// "a kind that provides no starting user".
//
// bento is that kind BY DESIGN, which is why it is the durable choice here: a
// cron firing has no human behind it, and Channel.spec.authzSubject exists
// precisely to name the subject such a session acts as. Contrast fake, which
// this used to use — fake hands every inbound a full ExternalIdentity and only
// LOOKED starter-less because it had not declared SessionOwnerProvider. When
// that was corrected these subtests silently stopped testing their own premise
// and failed. Do not swap in a kind whose missing starter is an oversight
// rather than its design.
func noStarterChannel(agentClass string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "bento",
			Role: spiceboxv1alpha1.ChannelRoleInput,
			// Set because a non-attributable kind without it is refused
			// EARLIER than the owner rules these subtests are about — leaving
			// it empty would have every one of them pass on the wrong message.
			AuthzSubject:   "service:tick-bot",
			AgentClass:     agentClass,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "fake-creds"},
			Bento: &spiceboxv1alpha1.BentoChannelConfig{
				Generate: &spiceboxv1alpha1.BentoGenerateConfig{Mapping: `root = "tick"`},
			},
		},
	}
}

func TestValidate_OwnerPolicy(t *testing.T) {
	scheme := testfixtures.NewScheme(t)

	// fakeCreds satisfies the fake kind (RequiredSecretKeys returns nil).
	fakeCreds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "fake-creds"},
		Data:       map[string][]byte{},
	}
	adoptguard.WithAdoptedLabel(fakeCreds)

	// slackCreds satisfies the slack kind's RequiredSecretKeys for socket mode.
	slackCreds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "slack-creds"},
		Data: map[string][]byte{
			"bot-token": []byte("xoxb-test"),
			"app-token": []byte("xapp-test"),
		},
	}
	adoptguard.WithAdoptedLabel(slackCreds)

	reconcileAndGetCondition := func(t *testing.T, objects ...client.Object) *metav1.Condition {
		t.Helper()
		c := fake.NewClientBuilder().
			WithScheme(scheme).
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

	t.Run("ownerless input, no owner policy → Valid=False, message contains 'ownerless input'", func(t *testing.T) {
		// bento kind: no SessionOwnerProvider → no starter.
		// AgentClass: agent-mode (no IdentityMode), no ownerCeiling.
		// Channel: no Owner policy at all → coverage gap.
		ac := newValidAgentClass("agent-cls", nil)
		ch := noStarterChannel("agent-cls")
		cond := reconcileAndGetCondition(t, ac, ch, fakeCreds)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, spiceboxv1alpha1.ReasonChannelSpecInvalid, cond.Reason)
		assert.Contains(t, cond.Message, "ownerless input")
	})

	t.Run("passthrough AgentClass with no-starter kind → Valid=False, message contains 'passthrough'", func(t *testing.T) {
		// bento kind: no SessionOwnerProvider → no starter.
		// AgentClass: IdentityMode=userPassthrough → requires starter; bento provides none → reject.
		ac := newValidAgentClass("passthrough-cls", func(a *spiceboxv1alpha1.AgentClass) {
			a.Spec.IdentityMode = spiceboxv1alpha1.IdentityModeUserPassthrough
		})
		ch := noStarterChannel("passthrough-cls")
		cond := reconcileAndGetCondition(t, ac, ch, fakeCreds)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, spiceboxv1alpha1.ReasonChannelSpecInvalid, cond.Reason)
		assert.Contains(t, cond.Message, "passthrough")
	})

	t.Run("ownerCeiling.starterOnly with explicit owner override → Valid=False, message contains 'ownerCeiling'", func(t *testing.T) {
		// slack kind: implements SessionOwnerProvider → has a starter, so the
		// ownerless-coverage gate passes.
		// AgentClass: ownerCeiling.starterOnly=true → forbids explicit/ownerless.
		// Channel: Owner.Explicit set → ceiling breach.
		ac := newValidAgentClass("ceiling-cls", func(a *spiceboxv1alpha1.AgentClass) {
			a.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
				OwnerCeiling: &spiceboxv1alpha1.OwnerCeiling{StarterOnly: true},
			}
		})
		ch := &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default"},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind:           "slack",
				AgentClass:     "ceiling-cls",
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "slack-creds"},
				Slack:          &spiceboxv1alpha1.SlackChannelConfig{},
				Owner:          &spiceboxv1alpha1.ChannelOwnerPolicy{Explicit: "user:alice"},
			},
		}
		cond := reconcileAndGetCondition(t, ac, ch, slackCreds)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, spiceboxv1alpha1.ReasonChannelSpecInvalid, cond.Reason)
		assert.Contains(t, cond.Message, "ownerCeiling")
	})
}

// A role=input Channel delivers its reply into a DIFFERENT Channel. That
// target must resolve at apply time — not at 09:00 on a Monday when the cron
// fires — so an unresolvable binding is reported as Valid=False here.
func TestValidate_OutputBindingForRoleInput(t *testing.T) {
	scheme := testfixtures.NewScheme(t)

	// bento's RequiredSecretKeys returns nil; an empty adopted Secret suffices.
	bentoCreds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "bento-creds"},
		Data:       map[string][]byte{},
	}
	adoptguard.WithAdoptedLabel(bentoCreds)

	// inputCh is a valid role=input bento Channel: it clears the kind's
	// ValidateSpec (needs spec.bento.generate) and the ownerless gate (bento
	// provides no starting user), so reconcile reaches the output-binding check.
	inputCh := func() *spiceboxv1alpha1.Channel {
		return &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default"},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind:           "bento",
				Role:           spiceboxv1alpha1.ChannelRoleInput,
				AgentClass:     "agent-cls",
				AuthzSubject:   "service:demo-cron",
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "bento-creds"},
				Bento: &spiceboxv1alpha1.BentoChannelConfig{
					Generate: &spiceboxv1alpha1.BentoGenerateConfig{
						Mapping:  `root = "run the digest"`,
						Interval: "60s",
					},
				},
				Owner: &spiceboxv1alpha1.ChannelOwnerPolicy{
					Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{Permission: "slack_channel:C1#member"},
				},
			},
		}
	}

	// outputCh is a role=output slack sibling. An empty channelID omits
	// outputDefaults entirely, making the Channel unanchorable.
	outputCh := func(name, channelID string) *spiceboxv1alpha1.Channel {
		ch := &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind:           "slack",
				Role:           spiceboxv1alpha1.ChannelRoleOutput,
				AgentClass:     "agent-cls",
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "slack-creds"},
				Slack:          &spiceboxv1alpha1.SlackChannelConfig{},
			},
		}
		if channelID != "" {
			ch.Spec.Slack.OutputDefaults = &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: channelID}
		}
		return ch
	}

	reconcileAndGetCondition := func(t *testing.T, objects ...client.Object) *metav1.Condition {
		t.Helper()
		c := fake.NewClientBuilder().
			WithScheme(scheme).
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

	cases := []struct {
		name       string
		siblings   []client.Object
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "one anchorable role=output sibling: Valid=True",
			siblings:   []client.Object{outputCh("test-output", "C1")},
			wantStatus: metav1.ConditionTrue,
			wantReason: spiceboxv1alpha1.ReasonChannelAllReferencesResolve,
		},
		{
			name:       "no role=output sibling: Valid=False/OutputBindingUnresolvable",
			siblings:   nil,
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonChannelOutputBindingUnresolvable,
		},
		{
			name:       "two role=output siblings: Valid=False/OutputBindingUnresolvable",
			siblings:   []client.Object{outputCh("out-a", "C1"), outputCh("out-b", "C2")},
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonChannelOutputBindingUnresolvable,
		},
		{
			name:       "role=output sibling with no outputDefaults: Valid=False/OutputBindingUnresolvable",
			siblings:   []client.Object{outputCh("test-output", "")},
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonChannelOutputBindingUnresolvable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{newValidAgentClass("agent-cls", nil), inputCh(), bentoCreds}
			objs = append(objs, tc.siblings...)
			cond := reconcileAndGetCondition(t, objs...)
			require.NotNil(t, cond, "Valid condition should be set")
			assert.Equal(t, tc.wantStatus, cond.Status, "Valid status; msg=%q", cond.Message)
			assert.Equal(t, tc.wantReason, cond.Reason, "Valid reason; msg=%q", cond.Message)
		})
	}

	// No-regression: a role=both Channel is self-contained and asserts nothing
	// about output siblings, so it stays Valid with none present.
	t.Run("role=both with no output sibling: Valid=True (self-contained)", func(t *testing.T) {
		fakeCreds := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "fake-creds"},
			Data:       map[string][]byte{},
		}
		adoptguard.WithAdoptedLabel(fakeCreds)
		both := &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default"},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind:           "fake",
				Role:           spiceboxv1alpha1.ChannelRoleBoth,
				AgentClass:     "agent-cls",
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "fake-creds"},
				Fake:           &spiceboxv1alpha1.FakeChannelConfig{},
				Owner: &spiceboxv1alpha1.ChannelOwnerPolicy{
					Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{Permission: "slack_channel:C1#member"},
				},
			},
		}
		cond := reconcileAndGetCondition(t, newValidAgentClass("agent-cls", nil), both, fakeCreds)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status, "msg=%q", cond.Message)
		assert.Equal(t, spiceboxv1alpha1.ReasonChannelAllReferencesResolve, cond.Reason)
	})
}

// TestValidate_NonAttributableKindRequiresAuthzSubject covers the rule the
// github wizard's missing subject screen ran into: a kind whose
// Kind.UserAttributable() is false has no per-user identity to attribute an
// inbound message to, so the Channel must declare the service subject its
// sessions act as.
//
// The rule is checked off the registry predicate rather than per-kind, so the
// table drives BOTH non-attributable kinds (bento, github) and an attributable
// control (fake) through one code path. A regression that special-cased one
// kind would leave the other row red.
func TestValidate_NonAttributableKindRequiresAuthzSubject(t *testing.T) {
	scheme := testfixtures.NewScheme(t, rbacv1.AddToScheme) // the github row stamps the per-Channel webhook-secret RBAC

	adoptedSecret := func(name string, data map[string][]byte) *corev1.Secret {
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
			Data:       data,
		}
		adoptguard.WithAdoptedLabel(sec)
		return sec
	}

	// The owner policy is set on every fixture so the ownerless-coverage gate
	// (which fires later, for the same non-attributable kinds) cannot be what
	// a "Valid=False" row is actually observing.
	ownerless := &spiceboxv1alpha1.ChannelOwnerPolicy{
		Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{Permission: "slack_channel:C1#member"},
	}

	bentoCh := func(subject string) *spiceboxv1alpha1.Channel {
		return &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default"},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind:           "bento",
				Role:           spiceboxv1alpha1.ChannelRoleInput,
				AgentClass:     "agent-cls",
				AuthzSubject:   subject,
				Owner:          ownerless,
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "bento-creds"},
				Bento: &spiceboxv1alpha1.BentoChannelConfig{
					Generate: &spiceboxv1alpha1.BentoGenerateConfig{Mapping: `root = "tick"`},
				},
			},
		}
	}
	githubCh := func(subject string) *spiceboxv1alpha1.Channel {
		return &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default"},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind:           "github",
				Role:           spiceboxv1alpha1.ChannelRoleInput,
				AgentClass:     "agent-cls",
				AuthzSubject:   subject,
				Owner:          ownerless,
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "gh-creds"},
				GitHub:         &spiceboxv1alpha1.GitHubChannelConfig{AppSlug: "demo-reviewbot"},
			},
		}
	}
	// fake is the attributable control: UserAttributable() is true, so an
	// empty subject is legitimate and must stay Valid=True.
	fakeCh := func(subject string) *spiceboxv1alpha1.Channel {
		return &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default"},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind:           "fake",
				Role:           spiceboxv1alpha1.ChannelRoleInput,
				AgentClass:     "agent-cls",
				AuthzSubject:   subject,
				Owner:          ownerless,
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "fake-creds"},
				Fake:           &spiceboxv1alpha1.FakeChannelConfig{},
			},
		}
	}

	// The paired role=output Channel every input Channel needs to resolve its
	// output binding — the Slack half of the reviewbot pairing this blocker's
	// empty subject would have taken down along with the input half.
	outputSibling := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "out", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "slack",
			Role:           spiceboxv1alpha1.ChannelRoleOutput,
			AgentClass:     "agent-cls",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "slack-creds"},
			Slack: &spiceboxv1alpha1.SlackChannelConfig{
				OutputDefaults: &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C1"},
			},
		},
	}

	fixtures := []client.Object{
		outputSibling,
		adoptedSecret("bento-creds", map[string][]byte{}),
		adoptedSecret("fake-creds", map[string][]byte{}),
		adoptedSecret("slack-creds", map[string][]byte{
			"bot-token": []byte("xoxb-test"),
			"app-token": []byte("xapp-test"),
		}),
		adoptedSecret("gh-creds", map[string][]byte{
			"app-id":          []byte("12345"),
			"private-key":     []byte("-----BEGIN RSA PRIVATE KEY-----\nZmFrZQ==\n-----END RSA PRIVATE KEY-----\n"),
			"webhook-secret":  []byte("whsec_demo"),
			"installation-id": []byte("67890"),
		}),
	}

	cases := []struct {
		name       string
		channel    *spiceboxv1alpha1.Channel
		wantStatus metav1.ConditionStatus
		wantMsg    string
	}{
		{
			name:       "kind=github, no authzSubject: Valid=False naming spec.authzSubject",
			channel:    githubCh(""),
			wantStatus: metav1.ConditionFalse,
			wantMsg:    `kind "github" cannot attribute inbound messages to a user; spec.authzSubject is required`,
		},
		{
			name:       "kind=github, service subject: Valid=True",
			channel:    githubCh("service:demo-reviewbot-github"),
			wantStatus: metav1.ConditionTrue,
		},
		{
			name:       "kind=bento, no authzSubject: Valid=False naming spec.authzSubject",
			channel:    bentoCh(""),
			wantStatus: metav1.ConditionFalse,
			wantMsg:    `kind "bento" cannot attribute inbound messages to a user; spec.authzSubject is required`,
		},
		{
			name:       "kind=bento, service subject: Valid=True",
			channel:    bentoCh("service:tick-bot"),
			wantStatus: metav1.ConditionTrue,
		},
		{
			name:       "kind=fake (attributable), no authzSubject: Valid=True",
			channel:    fakeCh(""),
			wantStatus: metav1.ConditionTrue,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := append([]client.Object{newValidAgentClass("agent-cls", nil), tc.channel}, fixtures...)
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objs...).
				WithStatusSubresource(&spiceboxv1alpha1.Channel{}).
				Build()
			r := newUnitReconciler(c)
			_, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: client.ObjectKey{Namespace: "default", Name: "ch"},
			})
			require.NoError(t, err)

			var got spiceboxv1alpha1.Channel
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "ch"}, &got))
			cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ChannelConditionValid)
			require.NotNil(t, cond, "Valid condition must be set")
			assert.Equal(t, tc.wantStatus, cond.Status, "Valid status; msg=%q", cond.Message)
			if tc.wantMsg != "" {
				assert.Equal(t, spiceboxv1alpha1.ReasonChannelSpecInvalid, cond.Reason)
				assert.Contains(t, cond.Message, tc.wantMsg,
					"the message must name the field a human has to fill in")
			}
		})
	}
}
