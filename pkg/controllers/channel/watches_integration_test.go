//go:build integration

// Watch-wiring integration tests for the Channel controller.
//
// Channel validation reads three other objects: the credentials Secret, the
// bound AgentClass, and — when spec.agentIdentity is set — the AgentIdentity.
// SetupWithManager watches the first two but not the third, so a Channel that
// reconciled before its AgentIdentity existed keeps advertising
// Valid=False/AgentIdentityMissing forever: the reconciler returns a bare
// ctrl.Result{} on that path (no RequeueAfter), and nothing else re-enqueues
// it. That is a permanent wedge, not a slow one — the only escape is a manual
// poke or an operator restart.
//
// This is the same class of bug the AgentClass controller was fixed for twice
// (MCPServer, then SpiceboxToolspec); see that package's
// watches_integration_test.go header.
package channel_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/channel"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// startChannelManager wires the Channel reconciler to a real manager so
// reconciles arrive through watches rather than direct calls, and blocks
// until the cache has synced.
func startChannelManager(t *testing.T, env *testenv.Env) {
	t.Helper()
	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:         env.Scheme,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		Controller:     ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
		LeaderElection: false,
	})
	require.NoError(t, err, "ctrl.NewManager")
	require.NoError(t, (&channel.Reconciler{
		Client: mgr.GetClient(),
		SecretReader: adoptguard.NewSecretReader(mgr.GetClient(), mgr.GetAPIReader(),
			adoptguard.Warn, func(types.NamespacedName) bool { return false }),
	}).SetupWithManager(mgr), "channel.SetupWithManager")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
	require.True(t, mgr.GetCache().WaitForCacheSync(ctx), "cache sync")
}

// eventuallyChannelValid polls the named Channel until its Valid condition
// matches. An empty wantReason matches any reason.
func eventuallyChannelValid(t *testing.T, c client.Client, name string, want metav1.ConditionStatus, wantReason string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	var last *metav1.Condition
	for time.Now().Before(deadline) {
		var got spiceboxv1alpha1.Channel
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &got); err == nil {
			cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ChannelConditionValid)
			last = cond
			if cond != nil && cond.Status == want && (wantReason == "" || cond.Reason == wantReason) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("Channel %s never reached Valid=%s reason=%s within %v; last=%+v", name, want, wantReason, d, last)
}

// TestChannel_ReReconcilesOnAgentIdentityCreate is the regression guard for
// the permanent wedge: a Channel naming an AgentIdentity that does not exist
// yet fails closed at AgentIdentityMissing, and creating that AgentIdentity
// must re-reconcile the Channel through a watch. Nothing about the Channel
// itself changes, and the Channel reconciler never requeues, so without an
// AgentIdentity watch this Channel stays Valid=False until a human intervenes.
func TestChannel_ReReconcilesOnAgentIdentityCreate(t *testing.T) {
	env := testenv.Shared(t)
	startChannelManager(t, env)
	ctx := context.Background()
	ns := "default"

	require.NoError(t, env.Client.Create(ctx,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s-idwatch", Namespace: ns}}),
		"create channel creds secret")
	adoptSecret(t, env.Client, ns, "s-idwatch")
	createValidAgentClass(t, env, ns, "ac-idwatch")

	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "c-idwatch", Namespace: ns},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "fake",
			AgentClass:     "ac-idwatch",
			AgentIdentity:  "id-arrives-late",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s-idwatch"},
			Fake:           &spiceboxv1alpha1.FakeChannelConfig{},
			Owner: &spiceboxv1alpha1.ChannelOwnerPolicy{
				Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{FromOutputChannel: true},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ch), "create Channel before its AgentIdentity")

	// Fail closed first — proves the controller reconciled and saw no identity.
	eventuallyChannelValid(t, env.Client, "c-idwatch", metav1.ConditionFalse,
		spiceboxv1alpha1.ReasonChannelAgentIdentityMissing, 5*time.Second)

	// The identity arrives (an operator finishes `oap identity setup`, or the
	// bundle's AgentIdentity lands a moment after its Channel).
	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "id-arrives-late", Namespace: ns},
		Spec:       spiceboxv1alpha1.AgentIdentitySpec{},
	}), "create AgentIdentity")

	eventuallyChannelValid(t, env.Client, "c-idwatch", metav1.ConditionTrue,
		spiceboxv1alpha1.ReasonChannelAllReferencesResolve, 10*time.Second)
}

// A role=input Channel is unresolvable until its role=output sibling exists —
// a real ordering hazard, since `kubectl apply -f dir/` and bundle installs do
// not guarantee the output Channel lands first. Nothing about the input
// Channel changes when the sibling arrives and the reconciler never requeues,
// so without a Channel→Channel watch the cron input stays Valid=False forever.
func TestChannel_ReReconcilesOnOutputChannelCreate(t *testing.T) {
	env := testenv.Shared(t)
	startChannelManager(t, env)
	ctx := context.Background()
	ns := "default"

	require.NoError(t, env.Client.Create(ctx,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s-outwatch", Namespace: ns}}),
		"create channel creds secret")
	adoptSecret(t, env.Client, ns, "s-outwatch")
	createValidAgentClass(t, env, ns, "ac-outwatch")

	in := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "c-outwatch-in", Namespace: ns},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "bento",
			Role:           spiceboxv1alpha1.ChannelRoleInput,
			AgentClass:     "ac-outwatch",
			AuthzSubject:   "service:demo-cron",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s-outwatch"},
			Bento: &spiceboxv1alpha1.BentoChannelConfig{
				Generate: &spiceboxv1alpha1.BentoGenerateConfig{
					Mapping:  `root = "run the digest"`,
					Interval: "168h",
				},
			},
			Owner: &spiceboxv1alpha1.ChannelOwnerPolicy{
				Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{Permission: "slack_channel:C1#member"},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, in), "create role=input Channel before its output sibling")

	// Fail closed first — proves the controller reconciled and found no target.
	eventuallyChannelValid(t, env.Client, "c-outwatch-in", metav1.ConditionFalse,
		spiceboxv1alpha1.ReasonChannelOutputBindingUnresolvable, 5*time.Second)

	// The output Channel arrives a moment later.
	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "c-outwatch-out", Namespace: ns},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "slack",
			Role:           spiceboxv1alpha1.ChannelRoleOutput,
			AgentClass:     "ac-outwatch",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s-outwatch"},
			Slack: &spiceboxv1alpha1.SlackChannelConfig{
				OutputDefaults: &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C1"},
			},
		},
	}), "create role=output Channel")

	eventuallyChannelValid(t, env.Client, "c-outwatch-in", metav1.ConditionTrue,
		spiceboxv1alpha1.ReasonChannelAllReferencesResolve, 10*time.Second)
}
