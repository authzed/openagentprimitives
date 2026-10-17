//go:build integration

// pkg/controllers/agentsession/browsercatalog_transient_envtest_test.go
//
// Proves the Phase B final-review fix: a TRANSIENT failure resolving the
// browser catalog (a non-NotFound Get error on the bound AgentIdentity) must
// REQUEUE the reconcile rather than terminally failing the session via
// markBootFailed. Companion to TestBrowserWakeWithDeletedIdentityFailsClosed
// in browsercatalog_envtest_test.go, which pins the opposite (NotFound is
// terminal) half of the split in resolveBrowserCatalog / errBrowserCatalogTerminal
// (browserpod.go) and its caller in controller.go's browser block.
//
// Drives the real envtest apiserver through a SEPARATE client.WithWatch built
// from env.Cfg, wrapped with sigs.k8s.io/controller-runtime/pkg/client/interceptor
// so every Get except the one we want to fail passes straight through to the
// real apiserver -- precedent: pkg/controllers/internal/reconcile/load_test.go's
// fake.NewClientBuilder().WithInterceptorFuncs(...) table, adapted here to wrap
// a live client instead of a fake one, since the rest of Reconcile's many
// prerequisite Gets (AgentClass, AgentSession, BrowserToolbox, the browser Pod
// and its ConfigMap/NetworkPolicy) must still hit real envtest-backed objects.
package agentsession_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// newBrowserReconcilerWithClient is newBrowserReconciler, but wired entirely
// off the given client.Client instead of env.Client -- so a test can hand in
// an interceptor-wrapped client and have every reader/writer in the
// reconciler (including the SecretReader/ConfigMapReader adopt-guards and the
// runner factory) see the SAME intercepted Get calls.
func newBrowserReconcilerWithClient(t *testing.T, env *testenv.Env, c client.Client) *agentsession.Reconciler {
	t.Helper()
	allow := func(types.NamespacedName) bool { return true }
	sr := adoptguard.NewSecretReader(c, c, adoptguard.Warn, allow)
	cmr := adoptguard.NewConfigMapReader(c, c, adoptguard.Warn, allow)
	r := &agentsession.Reconciler{
		Client:          c,
		APIReader:       c,
		Tokens:          tokens.NewRegistry(),
		Memory:          memory.NewLocal(inmem.NewBackend()),
		SecretReader:    sr,
		ConfigMapReader: cmr,
	}
	r.RunnerFactory = &agentsession.PodRunnerFactory{
		Client:       c,
		RunnerImage:  "agentprimitives-runner:dev",
		OperatorURL:  "http://op:8082",
		NATSURL:      "nats://spicebox-nats.agentprimitives-system.svc:4222",
		SecretReader: sr,
	}
	r.BrowserImageRef = "ap-browser:dev"
	r.Netpol = agentsession.NetpolConfig{
		Enabled:           true,
		OperatorNamespace: "agentprimitives-system",
		NATSPort:          4222,
		SpiceDBPort:       50051,
		SpiceDBInCluster:  true,
	}
	_ = env // kept for signature symmetry with newBrowserReconciler; unused otherwise
	return r
}

// TestBrowserCatalogTransientIdentityGetErrorRequeues proves the transient
// half of the split: a non-NotFound Get error fetching the bound
// AgentIdentity must come back as a plain reconcile error (requeue), and must
// NOT mark the session Failed/BrowserBootFailed. Every other object Get
// (AgentClass, AgentSession, BrowserToolbox, …) passes through to the real
// envtest apiserver unmodified; only Gets for THIS identity's name are faulted.
func TestBrowserCatalogTransientIdentityGetErrorRequeues(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	const identityName = "btb-trans-id"

	require.NoError(t, env.Client.Create(ctx, browserLoginSecret("btb-trans-logins", "app.example.com", browserPassSentinel)), "create login Secret")
	require.NoError(t, env.Client.Create(ctx, browserLoginIdentity(identityName, "btb-trans-logins")), "create AgentIdentity")
	require.NoError(t, env.Client.Create(ctx, browserToolboxBoundTo("btb-trans", identityName)), "create BrowserToolbox")

	ac := classWithBrowsers("ac-browser-trans",
		spiceboxv1alpha1.AgentClassBrowserToolboxRef{Name: "web", Ref: "btb-trans"})
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-browser-trans", "ac-browser-trans")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	key := types.NamespacedName{Namespace: "default", Name: "s-browser-trans"}

	// A separate client.WithWatch over the SAME envtest apiserver, wrapped so
	// every Get for the target AgentIdentity's name returns a non-NotFound
	// apierror (a simulated rate-limit/503) and everything else passes through
	// to the real cluster unmodified.
	watchClient, err := client.NewWithWatch(env.Cfg, client.Options{Scheme: env.Scheme})
	require.NoError(t, err, "build a second WithWatch client over the shared envtest apiserver")
	intercepted := interceptor.NewClient(watchClient, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*spiceboxv1alpha1.AgentIdentity); ok && key.Name == identityName {
				return apierrors.NewServiceUnavailable("simulated transient identity lookup failure")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	r := newBrowserReconcilerWithClient(t, env, intercepted)

	// Drive several reconciles: earlier passes install the finalizer and
	// resolve prerequisites untouched by the interceptor; once the browser
	// block reaches resolveBrowserCatalog it hits the faulted identity Get on
	// every subsequent pass.
	var lastErr error
	sawError := false
	for i := 0; i < 8; i++ {
		_, lastErr = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		if lastErr != nil {
			sawError = true
			break
		}
	}
	require.True(t, sawError, "a transient identity Get error must eventually surface as a reconcile error (requeue), not be silently absorbed")
	assert.ErrorContains(t, lastErr, identityName, "the propagated error names the identity whose Get failed")
	assert.ErrorContains(t, lastErr, "resolve browser catalog", "the propagated error identifies the browser-catalog resolve step")

	// The session must NOT have been marked terminally Failed: a transient
	// Get blip is retried, never a permanent session death.
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &got), "get session after the faulted reconcile passes")
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase, "a transient catalog error must not move the session to Failed")
	assert.Empty(t, got.Status.FailureReason, "a transient catalog error must not set FailureReason")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
	if cond != nil {
		assert.NotEqual(t, metav1.ConditionTrue, cond.Status, "the Failed condition must not be set True by a transient catalog error")
	}
}
