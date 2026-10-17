//go:build integration

// Watch-wiring integration test for the BrowserToolbox controller.
//
// BrowserToolbox validation cross-checks spec.access.identity against a live
// AgentIdentity (identityCheck in Reconcile). Without an AgentIdentity watch,
// a toolbox that reconciled before its identity existed -- or before the
// identity carried a usable credential -- would stay parked at Valid=False
// until the next unrelated reconcile of the BrowserToolbox itself, since
// nothing about the toolbox's own spec/generation changes when the identity
// it names does.
//
// Each test drives the controller through a real manager (not a direct
// Reconcile call) so a pass is proof the watch fires, not that a test helper
// happened to call Reconcile again.
package browsertoolbox_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/browsertoolbox"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// startToolboxManager wires the BrowserToolbox reconciler to a real manager
// (so reconciles arrive via watches, not direct calls) and blocks until the
// cache has synced.
func startToolboxManager(t *testing.T, env *testenv.Env) {
	t.Helper()
	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:         env.Scheme,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		Controller:     ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
		LeaderElection: false,
	})
	require.NoError(t, err, "ctrl.NewManager")
	require.NoError(t, (&browsertoolbox.Reconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr), "browsertoolbox.SetupWithManager")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
	require.True(t, mgr.GetCache().WaitForCacheSync(ctx), "cache sync")
}

// eventuallyToolboxValid polls the named BrowserToolbox until its Valid
// condition matches. An empty wantReason matches any reason.
func eventuallyToolboxValid(t *testing.T, c client.Client, name string, want metav1.ConditionStatus, wantReason string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	var last *metav1.Condition
	for time.Now().Before(deadline) {
		var got spiceboxv1alpha1.BrowserToolbox
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &got); err == nil {
			cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.BrowserToolboxConditionValid)
			last = cond
			if cond != nil && cond.Status == want && (wantReason == "" || cond.Reason == wantReason) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("BrowserToolbox %s never reached Valid=%s reason=%s within %v; last=%+v", name, want, wantReason, d, last)
}

// toolboxWithIdentity creates a BrowserToolbox naming identityName.
func toolboxWithIdentity(t *testing.T, c client.Client, name, identityName string) *spiceboxv1alpha1.BrowserToolbox {
	t.Helper()
	btb := &spiceboxv1alpha1.BrowserToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.BrowserToolboxSpec{
			Access: spiceboxv1alpha1.BrowserAccess{
				Mode:     "allowlist",
				Sites:    []spiceboxv1alpha1.BrowserSite{{Host: "example.com"}},
				Identity: identityName,
			},
		},
	}
	require.NoError(t, c.Create(context.Background(), btb), "create BrowserToolbox %s", name)
	return btb
}

// identityWithDirectCredentials creates an AgentIdentity carrying one
// directCredentials credential and stamps it Valid=validStatus -- the shape
// identityCheck accepts needs BOTH the block and the identity's own
// Valid=True, and the agentidentity reconciler is not on this manager, so
// the condition is stamped directly.
func identityWithDirectCredentials(t *testing.T, c client.Client, name string, validStatus metav1.ConditionStatus, reason string) *spiceboxv1alpha1.AgentIdentity {
	t.Helper()
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "site-login",
				Type: "directCredentials",
				DirectCredentials: &spiceboxv1alpha1.DirectCredentialsSource{
					Source:    "secret",
					SecretRef: spiceboxv1alpha1.DirectCredentialSecretRef{Name: "dummy", Key: "password"},
				},
			}},
		},
	}
	require.NoError(t, c.Create(context.Background(), ai), "create AgentIdentity %s", name)
	ai.Status = spiceboxv1alpha1.AgentIdentityStatus{Conditions: []metav1.Condition{{
		Type: spiceboxv1alpha1.AgentIdentityConditionValid, Status: validStatus,
		Reason: reason, LastTransitionTime: metav1.Now(),
	}}}
	require.NoError(t, c.Status().Update(context.Background(), ai), "stamp AgentIdentity %s Valid=%s", name, validStatus)
	return ai
}

// TestBrowserToolbox_ReReconcilesOnAgentIdentityCreate is the regression
// guard for the install-ordering race: the toolbox reconciles first against
// an AgentIdentity that does not exist yet (IdentityMissing). Creating the
// identity afterward must re-enqueue the toolbox through the AgentIdentity
// watch -- nothing about the toolbox's own spec/generation changes, so only
// a watch (not the periodic requeue, which this reconciler does not even
// set) can flip it to Valid=True.
func TestBrowserToolbox_ReReconcilesOnAgentIdentityCreate(t *testing.T) {
	env := testenv.Shared(t)
	startToolboxManager(t, env)

	toolboxWithIdentity(t, env.Client, "btb-watch-create", "id-watch-create")
	eventuallyToolboxValid(t, env.Client, "btb-watch-create", metav1.ConditionFalse,
		spiceboxv1alpha1.ReasonBrowserToolboxIdentityMissing, 5*time.Second)

	identityWithDirectCredentials(t, env.Client, "id-watch-create", metav1.ConditionTrue, "OK")

	eventuallyToolboxValid(t, env.Client, "btb-watch-create", metav1.ConditionTrue,
		spiceboxv1alpha1.ReasonBrowserToolboxValid, 10*time.Second)
}

// TestBrowserToolbox_ReReconcilesOnAgentIdentityDelete covers the other
// direction: a toolbox that is Valid=True because its identity exists and is
// usable must fall back to Valid=False/IdentityMissing when that identity is
// deleted -- proving the watch fires on delete, not just create/update.
func TestBrowserToolbox_ReReconcilesOnAgentIdentityDelete(t *testing.T) {
	env := testenv.Shared(t)
	startToolboxManager(t, env)
	ctx := context.Background()

	ai := identityWithDirectCredentials(t, env.Client, "id-watch-delete", metav1.ConditionTrue, "OK")
	toolboxWithIdentity(t, env.Client, "btb-watch-delete", "id-watch-delete")

	eventuallyToolboxValid(t, env.Client, "btb-watch-delete", metav1.ConditionTrue,
		spiceboxv1alpha1.ReasonBrowserToolboxValid, 5*time.Second)

	require.NoError(t, env.Client.Delete(ctx, ai), "delete AgentIdentity id-watch-delete")

	eventuallyToolboxValid(t, env.Client, "btb-watch-delete", metav1.ConditionFalse,
		spiceboxv1alpha1.ReasonBrowserToolboxIdentityMissing, 10*time.Second)
}

// TestBrowserToolbox_ReReconcilesOnAgentIdentityStatusFlip proves the
// identity-validity half of the gate self-heals through the SAME watch: a
// right-shaped identity at Valid=False parks the toolbox at IdentityUnusable,
// and stamping the identity Valid=True (as its own reconciler would once the
// backing Secret appears) must flip the toolbox to Valid=True without
// anything about the toolbox itself changing -- only the AgentIdentity watch
// can deliver that reconcile.
func TestBrowserToolbox_ReReconcilesOnAgentIdentityStatusFlip(t *testing.T) {
	env := testenv.Shared(t)
	startToolboxManager(t, env)
	ctx := context.Background()

	ai := identityWithDirectCredentials(t, env.Client, "id-watch-flip", metav1.ConditionFalse, "SecretMissing")
	toolboxWithIdentity(t, env.Client, "btb-watch-flip", "id-watch-flip")

	eventuallyToolboxValid(t, env.Client, "btb-watch-flip", metav1.ConditionFalse,
		spiceboxv1alpha1.ReasonBrowserToolboxIdentityUnusable, 5*time.Second)

	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(ai), ai),
		"refresh AgentIdentity before status flip")
	ai.Status = spiceboxv1alpha1.AgentIdentityStatus{Conditions: []metav1.Condition{{
		Type: spiceboxv1alpha1.AgentIdentityConditionValid, Status: metav1.ConditionTrue,
		Reason: "OK", LastTransitionTime: metav1.Now(),
	}}}
	require.NoError(t, env.Client.Status().Update(ctx, ai), "flip AgentIdentity to Valid=True")

	eventuallyToolboxValid(t, env.Client, "btb-watch-flip", metav1.ConditionTrue,
		spiceboxv1alpha1.ReasonBrowserToolboxValid, 10*time.Second)
}
