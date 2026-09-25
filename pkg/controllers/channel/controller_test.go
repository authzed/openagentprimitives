//go:build integration

package channel_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento" // register bento kind (UserAttributable=false, SpawnsSessionOnInbound=true)
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"  // register fake kind for ValidateSpec/RequiredSecretKeys
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local" // register local kind (does NOT implement ChannelHistoryReader)
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack" // register slack kind for ValidateSpec/RequiredSecretKeys
	"github.com/authzed/openagentprimitives/pkg/controllers/channel"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func newReconciler(te *testenv.Env) *channel.Reconciler {
	return &channel.Reconciler{
		Client:       te.Client,
		SecretReader: adoptguard.NewSecretReader(te.Client, te.Client, adoptguard.Warn, func(types.NamespacedName) bool { return false }),
	}
}

// adoptSecret pre-stamps the AdoptedLabel on a secret so the guard permits
// reads via SecretReader in tests that bypass the real adoptkit.AdoptSecret
// server-side-apply path.
func adoptSecret(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &sec),
		"get secret before adoption stamp")
	adoptguard.WithAdoptedLabel(&sec)
	require.NoError(t, c.Update(context.Background(), &sec), "stamp AdoptedLabel on secret")
}

func reconcileOnce(t *testing.T, r *channel.Reconciler, ns, name string) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: name},
	})
	require.NoError(t, err, "Reconcile %s/%s", ns, name)
}

func getChannel(t *testing.T, c client.Client, ns, name string) *spiceboxv1alpha1.Channel {
	t.Helper()
	got := &spiceboxv1alpha1.Channel{}
	require.NoError(t,
		c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, got),
		"Get Channel %s/%s", ns, name)
	return got
}

func validCondition(ch *spiceboxv1alpha1.Channel) *metav1.Condition {
	return meta.FindStatusCondition(ch.Status.Conditions, spiceboxv1alpha1.ChannelConditionValid)
}

// createValidAgentClass creates an AgentClass and stamps Valid=True via the
// status subresource. Returns the persisted object.
func createValidAgentClass(t *testing.T, te *testenv.Env, ns, name string) *spiceboxv1alpha1.AgentClass {
	t.Helper()
	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model:        &spiceboxv1alpha1.ModelConfig{Provider: "anthropic", Name: "claude-3", APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "s", Key: "k"}},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "hi"},
			Budget:       &spiceboxv1alpha1.BudgetConfig{MaxTurns: 1, MaxTokens: 1, MaxDuration: metav1.Duration{Duration: 1}},
		},
	}
	require.NoError(t, te.Client.Create(context.Background(), class), "create AgentClass %s", name)
	class.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.AgentClassConditionValid,
		Status:             metav1.ConditionTrue,
		Reason:             "Valid",
		LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, te.Client.Status().Update(context.Background(), class),
		"stamp AgentClass %s Valid=True", name)
	return class
}

// TestReconcile_FailureReasons collapses the original five reason-of-failure
// cases (SpecInvalid, SecretMissing, AgentClassMissing, AgentClassNotValid,
// AgentIdentityMissing) into one table-driven sweep. Each case configures
// the fixture (secret + AgentClass shape + Channel spec) the case requires,
// reconciles once, and asserts the Valid condition reason on the Channel.
//
// The shared shape is: create Channel under "default" ns; reconcile; read
// back; assert Valid condition. The variation is what other objects exist
// and how the Channel spec is shaped.
func TestReconcile_FailureReasons(t *testing.T) {
	const ns = "default"

	cases := []struct {
		name       string
		setup      func(t *testing.T, te *testenv.Env)
		chSpec     func() spiceboxv1alpha1.ChannelSpec
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:  "slack kind with no Slack block → Valid=False/ChannelSpecInvalid",
			setup: func(t *testing.T, te *testenv.Env) {},
			chSpec: func() spiceboxv1alpha1.ChannelSpec {
				return spiceboxv1alpha1.ChannelSpec{
					Kind:           "slack",
					AgentClass:     "any",
					CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
					// Slack: nil → SpecInvalid
				}
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonChannelSpecInvalid,
		},
		{
			name: "credentialsRef to non-existent secret → Valid=False/ChannelSecretMissing",
			setup: func(t *testing.T, te *testenv.Env) {
				// no secret created
			},
			chSpec: func() spiceboxv1alpha1.ChannelSpec {
				return spiceboxv1alpha1.ChannelSpec{
					Kind:           "fake",
					AgentClass:     "ac1",
					CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "missing"},
					Fake:           &spiceboxv1alpha1.FakeChannelConfig{},
				}
			},
			wantReason: spiceboxv1alpha1.ReasonChannelSecretMissing,
		},
		{
			name: "AgentClass ref to non-existent class → Valid=False/ChannelAgentClassMissing",
			setup: func(t *testing.T, te *testenv.Env) {
				require.NoError(t,
					te.Client.Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: ns}}),
					"create secret")
				adoptSecret(t, te.Client, ns, "s")
			},
			chSpec: func() spiceboxv1alpha1.ChannelSpec {
				return spiceboxv1alpha1.ChannelSpec{
					Kind:           "fake",
					AgentClass:     "missing-class",
					CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
					Fake:           &spiceboxv1alpha1.FakeChannelConfig{},
				}
			},
			wantReason: spiceboxv1alpha1.ReasonChannelAgentClassMissing,
		},
		{
			name: "AgentClass exists but Valid=False → Valid=False/ChannelAgentClassNotValid",
			setup: func(t *testing.T, te *testenv.Env) {
				require.NoError(t,
					te.Client.Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: ns}}),
					"create secret")
				adoptSecret(t, te.Client, ns, "s")
				class := &spiceboxv1alpha1.AgentClass{
					ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: ns},
					Spec: spiceboxv1alpha1.AgentClassSpec{
						Model:        &spiceboxv1alpha1.ModelConfig{Provider: "anthropic", Name: "claude-3", APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "s", Key: "k"}},
						SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "hi"},
						Budget:       &spiceboxv1alpha1.BudgetConfig{MaxTurns: 1, MaxTokens: 1, MaxDuration: metav1.Duration{Duration: 1}},
					},
				}
				require.NoError(t, te.Client.Create(context.Background(), class), "create AgentClass")
				class.Status.Conditions = []metav1.Condition{{
					Type:               spiceboxv1alpha1.AgentClassConditionValid,
					Status:             metav1.ConditionFalse,
					Reason:             spiceboxv1alpha1.ReasonChannelSecretMissing,
					LastTransitionTime: metav1.Now(),
				}}
				require.NoError(t, te.Client.Status().Update(context.Background(), class), "stamp Valid=False")
			},
			chSpec: func() spiceboxv1alpha1.ChannelSpec {
				return spiceboxv1alpha1.ChannelSpec{
					Kind:           "fake",
					AgentClass:     "ac1",
					CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
					Fake:           &spiceboxv1alpha1.FakeChannelConfig{},
				}
			},
			wantReason: spiceboxv1alpha1.ReasonChannelAgentClassNotValid,
		},
		{
			name: "AgentIdentity ref to non-existent identity → Valid=False/ChannelAgentIdentityMissing",
			setup: func(t *testing.T, te *testenv.Env) {
				require.NoError(t,
					te.Client.Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: ns}}),
					"create secret")
				adoptSecret(t, te.Client, ns, "s")
				createValidAgentClass(t, te, ns, "ac1")
			},
			chSpec: func() spiceboxv1alpha1.ChannelSpec {
				return spiceboxv1alpha1.ChannelSpec{
					Kind:           "fake",
					AgentClass:     "ac1",
					AgentIdentity:  "missing-identity",
					CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
					Fake:           &spiceboxv1alpha1.FakeChannelConfig{},
					// fake provides no starting user, so owner policy is required
					// to pass the owner gate and reach the AgentIdentity check.
					Owner: &spiceboxv1alpha1.ChannelOwnerPolicy{
						Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{FromOutputChannel: true},
					},
				}
			},
			wantReason: spiceboxv1alpha1.ReasonChannelAgentIdentityMissing,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			te := testenv.Shared(t)
			tc.setup(t, te)
			ch := &spiceboxv1alpha1.Channel{
				ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: ns},
				Spec:       tc.chSpec(),
			}
			require.NoError(t, te.Client.Create(context.Background(), ch), "create Channel")

			r := newReconciler(te)
			reconcileOnce(t, r, ns, "c1")
			got := getChannel(t, te.Client, ns, "c1")
			cond := validCondition(got)
			require.NotNil(t, cond, "Valid condition should be set")
			if tc.wantStatus != "" {
				assert.Equal(t, tc.wantStatus, cond.Status, "Valid condition status")
			}
			assert.Equal(t, tc.wantReason, cond.Reason,
				"Valid condition reason; msg=%q", cond.Message)
		})
	}
}

// TestReconcileAllReferencesResolve covers the happy path: a fake-kind
// Channel with a present secret and a Valid=True AgentClass reaches
// Valid=True with reason=ChannelAllReferencesResolve, and stamps
// ResolvedAgentClassUID on its status.
func TestReconcileAllReferencesResolve(t *testing.T) {
	te := testenv.Shared(t)
	ns := "default"

	require.NoError(t,
		te.Client.Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: ns}}),
		"create secret")
	adoptSecret(t, te.Client, ns, "s")
	createValidAgentClass(t, te, ns, "ac1")

	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: ns},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "fake",
			AgentClass:     "ac1",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
			Fake:           &spiceboxv1alpha1.FakeChannelConfig{},
			// fake provides no starting user → owner policy required to be Valid.
			Owner: &spiceboxv1alpha1.ChannelOwnerPolicy{
				Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{FromOutputChannel: true},
			},
		},
	}
	require.NoError(t, te.Client.Create(context.Background(), ch), "create Channel")

	r := newReconciler(te)
	reconcileOnce(t, r, ns, "c1")
	got := getChannel(t, te.Client, ns, "c1")
	cond := validCondition(got)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "Valid status (happy path)")
	assert.Equal(t, spiceboxv1alpha1.ReasonChannelAllReferencesResolve, cond.Reason, "Valid reason")
	assert.NotEmpty(t, got.Status.ResolvedAgentClassUID, "ResolvedAgentClassUID should be frozen")
}

// A browser-kind Channel — the browser page's ephemeral channel — reaches
// Valid=True with a present secret + Valid AgentClass and, crucially, WITHOUT
// any spec.owner policy. Before browser implemented SessionOwner this exact
// Channel went Valid=False/ChannelSpecInvalid ("ownerless input requires
// spec.owner.explicit or spec.owner.ownerless"), which stranded every chat
// session — this test guards that regression.
//
// Rule 5 now has TWO ways of being satisfied here (it refuses only a kind that
// provides no starter AND spawns a session on inbound, and browser answers
// false to the second), so the reconcile below no longer isolates the property
// the name claims. The direct SessionOwnerProvider assertion at the end is
// what keeps it honest: without it this test would keep passing if browser
// lost SessionOwner entirely, which is the exact regression it was written for.
func TestReconcileBrowserKindValidWithoutOwnerPolicy(t *testing.T) {
	te := testenv.Shared(t)
	ns := "default"

	require.NoError(t,
		te.Client.Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "chat-creds", Namespace: ns}}),
		"create chat creds secret")
	adoptSecret(t, te.Client, ns, "chat-creds")
	createValidAgentClass(t, te, ns, "chat-ac")

	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "chat-chan", Namespace: ns},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           browser.KindName,
			Role:           spiceboxv1alpha1.ChannelRoleBoth,
			AgentClass:     "chat-ac",
			SessionScope:   "user",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "chat-creds"},
			// No spec.owner: browser provides the starting user itself.
		},
	}
	require.NoError(t, te.Client.Create(context.Background(), ch), "create browser Channel")

	r := newReconciler(te)
	reconcileOnce(t, r, ns, "chat-chan")
	got := getChannel(t, te.Client, ns, "chat-chan")
	cond := validCondition(got)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "a browser Channel must be Valid=True without an owner policy")
	assert.Equal(t, spiceboxv1alpha1.ReasonChannelAllReferencesResolve, cond.Reason)

	// The starter half, asserted on its own against the registered kind — the
	// same probe rule 5 makes.
	k, ok := chregistry.Get(browser.KindName)
	require.True(t, ok, "browser kind must be registered")
	so, ok := k.(channelkinds.SessionOwnerProvider)
	require.True(t, ok, "browser must implement channelkinds.SessionOwnerProvider")
	_, provides := so.SessionOwner(channelkinds.InboundEvent{
		ExternalIDs: channelkinds.ExternalIdentity{Kind: identity.Kind(browser.KindName), ExternalID: "probe"},
	})
	assert.True(t, provides,
		"browser must report a starting user for an identified inbound; the idp-authenticated sender IS the session's owner")
}

// TestReconcileAgentKindValidWithoutOwnerPolicy: a kind=agent Channel — the
// surface a conversational subagent talks to its parent over — reaches
// Valid=True with no spec.owner, no credentials Secret, and a kind that
// provides no starting user at all.
//
// It is the case rule 5's SpawnsSessionOnInbound skip exists for. agent
// provides no starter (no SessionOwnerProvider) and, before the skip, that
// alone made the Channel permanently Valid=False for want of an owner policy
// it is not the answer to: nothing is ever born on this Channel. The
// SubagentRequest controller pre-creates the child session, resolves its owner
// from the started-by annotations it inherits from its parent, and only then
// binds this Channel to it. A Valid=False here takes the whole feature down —
// the child has no surface, so the parent can neither speak to it nor hear
// back.
//
// The spec is buildAgentChannel's, verbatim: role=both, an "agentsession:"
// authzSubject naming the parent, and an empty credentialsRef (the
// counterparty is in-cluster, so the kind declares no required Secret keys and
// validate skips the Secret checks on an empty secretName).
func TestReconcileAgentKindValidWithoutOwnerPolicy(t *testing.T) {
	te := testenv.Shared(t)
	ns := "default"

	createValidAgentClass(t, te, ns, "child-ac")

	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "child-inbox", Namespace: ns},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:         agent.KindName,
			Role:         spiceboxv1alpha1.ChannelRoleBoth,
			AgentClass:   "child-ac",
			AuthzSubject: "agentsession:" + ns + "/parent-1",
			SessionScope: agent.Kind{}.DefaultSessionScope(),
			// No spec.owner and no credentialsRef.secretName: neither is what
			// this Channel is for.
		},
	}
	require.NoError(t, te.Client.Create(context.Background(), ch), "create agent Channel")

	r := newReconciler(te)
	reconcileOnce(t, r, ns, "child-inbox")
	got := getChannel(t, te.Client, ns, "child-inbox")
	cond := validCondition(got)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"an agent Channel must be Valid=True without an owner policy; msg=%q", cond.Message)
	assert.Equal(t, spiceboxv1alpha1.ReasonChannelAllReferencesResolve, cond.Reason)
}

// TestReconcileBentoKindInvalidWithoutOwnerPolicy is what keeps the skip above
// honest, and it is the row to read before widening that predicate. bento is
// the OTHER kind reporting UserAttributable=false, and it does spawn a session
// on every inbound — a cron firing creates one — so the session it creates has
// no owner unless the Channel names one, and rule 5 must still refuse it.
//
// Written as the negative control for the agent case deliberately: if the skip
// were ever relaxed from "spawns nothing" to "not user-attributable", the agent
// test above would keep passing and every cron Channel in the cluster would
// silently start accepting an ownerless spec. This is the test that fails
// instead.
func TestReconcileBentoKindInvalidWithoutOwnerPolicy(t *testing.T) {
	te := testenv.Shared(t)
	ns := "default"

	require.NoError(t,
		te.Client.Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cron-creds", Namespace: ns}}),
		"create cron creds secret")
	adoptSecret(t, te.Client, ns, "cron-creds")
	createValidAgentClass(t, te, ns, "cron-ac")

	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "cron-chan", Namespace: ns},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "bento",
			Role: spiceboxv1alpha1.ChannelRoleInput,
			// bento is userless input, so rule 1b (authzSubject required) fires
			// before the owner rule this test is named for. Set it to get past.
			AuthzSubject:   "service:cron-bot",
			AgentClass:     "cron-ac",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "cron-creds"},
			Bento: &spiceboxv1alpha1.BentoChannelConfig{
				Generate: &spiceboxv1alpha1.BentoGenerateConfig{
					Mapping: `root = "send the weekly digest"`, Interval: "168h",
				},
			},
			// No spec.owner: for a kind that DOES spawn sessions, this is the
			// unanswered question rule 5 exists to catch.
		},
	}
	require.NoError(t, te.Client.Create(context.Background(), ch), "create bento Channel")

	r := newReconciler(te)
	reconcileOnce(t, r, ns, "cron-chan")
	got := getChannel(t, te.Client, ns, "cron-chan")
	cond := validCondition(got)
	require.NotNil(t, cond, "Valid condition")
	require.Equal(t, metav1.ConditionFalse, cond.Status,
		"a bento Channel with no owner policy must stay Valid=False; reason=%s msg=%q", cond.Reason, cond.Message)
	assert.Equal(t, spiceboxv1alpha1.ReasonChannelSpecInvalid, cond.Reason)
	assert.Contains(t, cond.Message, "ownerless input requires spec.owner.explicit or spec.owner.ownerless",
		"the message must name the field the operator has to set")
}

// TestReconcileSlackSecretMissingBotToken: a slack-kind Channel whose
// secret lacks the bot-token key fails validation with
// ChannelSecretKeyMissing and the message names the missing key.
func TestReconcileSlackSecretMissingBotToken(t *testing.T) {
	te := testenv.Shared(t)
	ns := "default"

	// Secret with only app-token, no bot-token.
	require.NoError(t, te.Client.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-creds", Namespace: ns},
		Data:       map[string][]byte{"app-token": []byte("xapp-x")},
	}), "create slack-creds secret")
	adoptSecret(t, te.Client, ns, "slack-creds")

	createValidAgentClass(t, te, ns, "ac1")

	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "c-slack", Namespace: ns},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "slack", AgentClass: "ac1",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "slack-creds"},
			Slack:          &spiceboxv1alpha1.SlackChannelConfig{Mode: "socket"},
		},
	}
	require.NoError(t, te.Client.Create(context.Background(), ch), "create slack Channel")

	r := newReconciler(te)
	reconcileOnce(t, r, ns, "c-slack")
	got := getChannel(t, te.Client, ns, "c-slack")
	cond := validCondition(got)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Valid status")
	assert.Equal(t, spiceboxv1alpha1.ReasonChannelSecretKeyMissing, cond.Reason, "Valid reason")
	assert.Contains(t, cond.Message, "bot-token", "message should name the missing key")
}

// TestReconcileSlackAllReferencesResolve: a slack-kind Channel with both
// bot-token and app-token present validates True.
func TestReconcileSlackAllReferencesResolve(t *testing.T) {
	te := testenv.Shared(t)
	ns := "default"

	require.NoError(t, te.Client.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-creds", Namespace: ns},
		Data: map[string][]byte{
			"bot-token": []byte("xoxb-x"),
			"app-token": []byte("xapp-x"),
		},
	}), "create slack-creds secret")
	adoptSecret(t, te.Client, ns, "slack-creds")

	createValidAgentClass(t, te, ns, "ac1")

	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "c-slack-ok", Namespace: ns},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "slack", AgentClass: "ac1",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "slack-creds"},
			Slack:          &spiceboxv1alpha1.SlackChannelConfig{Mode: "socket"},
		},
	}
	require.NoError(t, te.Client.Create(context.Background(), ch), "create slack Channel")

	r := newReconciler(te)
	reconcileOnce(t, r, ns, "c-slack-ok")
	got := getChannel(t, te.Client, ns, "c-slack-ok")
	cond := validCondition(got)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "Valid status (slack happy path)")
}

// TestReconcileChannelHistory covers the controller's capability check on
// spec.channelHistory.enabled: kinds implementing channelkinds.ChannelHistoryReader
// (slack) stay Valid=True; kinds that don't (local) are rejected with
// SpecInvalid and a message naming the missing capability.
func TestReconcileChannelHistory(t *testing.T) {
	t.Run("slack kind + channelHistory.enabled=true → Valid=True (slack implements ChannelHistoryReader)", func(t *testing.T) {
		te := testenv.Shared(t)
		ns := "default"

		require.NoError(t, te.Client.Create(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "slack-creds-hist", Namespace: ns},
			Data: map[string][]byte{
				"bot-token": []byte("xoxb-x"),
				"app-token": []byte("xapp-x"),
			},
		}), "create slack-creds secret")
		adoptSecret(t, te.Client, ns, "slack-creds-hist")

		createValidAgentClass(t, te, ns, "ac-hist-slack")

		ch := &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: "c-slack-hist", Namespace: ns},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind: "slack", AgentClass: "ac-hist-slack",
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "slack-creds-hist"},
				Slack:          &spiceboxv1alpha1.SlackChannelConfig{Mode: "socket"},
				ChannelHistory: &spiceboxv1alpha1.ChannelHistorySpec{Enabled: true},
			},
		}
		require.NoError(t, te.Client.Create(context.Background(), ch), "create slack Channel")

		r := newReconciler(te)
		reconcileOnce(t, r, ns, "c-slack-hist")
		got := getChannel(t, te.Client, ns, "c-slack-hist")
		cond := validCondition(got)
		require.NotNil(t, cond, "Valid condition")
		assert.Equal(t, metav1.ConditionTrue, cond.Status, "Valid status (slack + channelHistory happy path); msg=%q", cond.Message)
	})

	// local is used here (NOT fake) because a later e2e task makes fake
	// implement ChannelHistoryReader, which would silently turn this negative
	// case into a false pass. local does not, and is not planned to,
	// implement the interface. local's own ValidateSpec needs no
	// kind-specific config block, and this capability check runs before the
	// secret-existence/AgentClass checks in `validate`, so no secret or
	// AgentClass fixture is required to reach it.
	t.Run("local kind + channelHistory.enabled=true → Valid=False/SpecInvalid (local lacks ChannelHistoryReader)", func(t *testing.T) {
		te := testenv.Shared(t)
		ns := "default"

		ch := &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: "c-local-hist", Namespace: ns},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind:           "local",
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
				ChannelHistory: &spiceboxv1alpha1.ChannelHistorySpec{Enabled: true},
			},
		}
		require.NoError(t, te.Client.Create(context.Background(), ch), "create local Channel")

		r := newReconciler(te)
		reconcileOnce(t, r, ns, "c-local-hist")
		got := getChannel(t, te.Client, ns, "c-local-hist")
		cond := validCondition(got)
		require.NotNil(t, cond, "Valid condition")
		assert.Equal(t, metav1.ConditionFalse, cond.Status, "Valid status")
		assert.Equal(t, spiceboxv1alpha1.ReasonChannelSpecInvalid, cond.Reason, "Valid reason")
		assert.Contains(t, cond.Message, "does not support channel history", "message should name our capability check")
	})
}
