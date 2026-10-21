//go:build integration

// pkg/controllers/agentidentity/secret_arrival_prodcache_integration_test.go
//
// TestAgentIdentity_SecretArrivalAndRotation_UnderProductionCacheFilter proves
// the self-heal story secret_arrival_test.go documents, END TO END, against a
// manager whose Secret cache is filtered EXACTLY the way
// internal/cmd/operator/main.go filters it: a Secret is only cached/watched
// once it carries adoptguard.AdoptedLabel. Every other test in this package
// (startManager in controller_test.go) builds its manager with no such
// filter, so a never-adopted Secret IS visible to those tests' watch from the
// moment it is created — which is NOT what the real operator sees, and would
// mask exactly the gap this test exists to catch.
//
// There is no change in this commit to agentidentity/controller.go: the
// SetupWithManager Watches(&corev1.Secret{}, ...) call and the
// secretArrivalRequeue 30s poll (both already present before this task) are
// what makes this pass. This test is the missing end-to-end proof, under
// realistic cache conditions, that:
//
//  1. a Secret created AFTER its AgentIdentity converges on its own — no
//     manual Reconcile call, no unrelated reconcile, no nudge; and
//  2. once that Secret is adopted (and so IN the label-filtered cache), a
//     later rotation is caught by the real Secret WATCH, not by any timer —
//     secretArrivalRequeue only ever fires while Valid is already False, so
//     a True→False transition proves the watch fired.
package agentidentity_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentidentity"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// startManagerWithAdoptedLabelCache builds and starts a manager whose Secret
// informer is label-filtered on adoptguard.AdoptedLabel — the same
// cache.Options.ByObject shape internal/cmd/operator/main.go applies in
// production. startManager in controller_test.go deliberately does NOT do
// this (it needs an unfiltered cache for its own scenarios), which is why
// this helper lives in its own file rather than growing a parameter there.
func startManagerWithAdoptedLabelCache(t *testing.T, env *testenv.Env) {
	t.Helper()
	adoptedReq, err := labels.NewRequirement(adoptguard.AdoptedLabel, selection.Exists, nil)
	require.NoError(t, err, "build adopted-label requirement")
	adoptedSel := labels.NewSelector().Add(*adoptedReq)

	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:         env.Scheme,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		Controller:     ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
		LeaderElection: false,
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Secret{}: {Label: adoptedSel},
			},
		},
	})
	require.NoError(t, err, "manager")

	sr := adoptguard.NewSecretReader(mgr.GetClient(), mgr.GetAPIReader(), adoptguard.Warn, func(types.NamespacedName) bool { return false })
	require.NoError(t, (&agentidentity.Reconciler{
		Client:       mgr.GetClient(),
		APIReader:    mgr.GetAPIReader(),
		SecretReader: sr,
	}).SetupWithManager(mgr), "setup validity reconciler")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
	require.True(t, mgr.GetCache().WaitForCacheSync(ctx), "cache sync")
}

func TestAgentIdentity_SecretArrivalAndRotation_UnderProductionCacheFilter(t *testing.T) {
	env := testenv.Shared(t)
	startManagerWithAdoptedLabelCache(t, env)
	ctx := context.Background()

	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "prodcache-ai", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "demo-cred", Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "prodcache-secret", Key: "token"},
				},
			}},
		},
	}
	mustCreate(t, env.Client, ai)

	// Phase 1: the AgentIdentity is created BEFORE its Secret exists at all.
	// Valid=False/SecretMissing follows immediately — nothing to wait on the
	// watch for yet.
	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.AgentIdentity
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(ai), &got); err != nil {
			return false
		}
		return hasCondition(&got, spiceboxv1alpha1.AgentIdentityConditionValid, metav1.ConditionFalse, spiceboxv1alpha1.ReasonSecretMissing)
	})

	// Phase 2: the Secret arrives the documented way — `kubectl create secret
	// generic`, carrying NO AdoptedLabel. It is invisible to this manager's
	// label-filtered Secret cache, so the ONLY thing that can converge this
	// identity from here is secretArrivalRequeue's 30s poll (live Get + adopt).
	// The bound is generous above that 30s to absorb scheduling slack, not
	// because convergence should take anywhere near it.
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "prodcache-secret", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("demo-token-value")},
	}
	mustCreate(t, env.Client, sec)

	eventually(t, 45*time.Second, func() bool {
		var got spiceboxv1alpha1.AgentIdentity
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(ai), &got); err != nil {
			return false
		}
		return hasCondition(&got, spiceboxv1alpha1.AgentIdentityConditionValid, metav1.ConditionTrue, spiceboxv1alpha1.ReasonAllReferencesResolve)
	})

	var gotSec corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "prodcache-secret"}, &gotSec))
	_, adopted := gotSec.Labels[adoptguard.AdoptedLabel]
	require.True(t, adopted,
		"the converging reconcile must stamp AdoptedLabel — that is what puts the Secret into the label-filtered cache for the watch exercised next")

	// Phase 3: the Secret is now adopted, so it IS in the label-filtered
	// cache. secretArrivalRequeue's poll never fires while Valid=True — only
	// the Secret watch can re-trigger a reconcile from here. A tight bound
	// (well under the 30s poll interval) is what distinguishes "the watch
	// caught this" from "a timer eventually caught this."
	gotSec.Data = map[string][]byte{}
	require.NoError(t, env.Client.Update(ctx, &gotSec), "rotate secret to drop its required key")

	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.AgentIdentity
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(ai), &got); err != nil {
			return false
		}
		return hasCondition(&got, spiceboxv1alpha1.AgentIdentityConditionValid, metav1.ConditionFalse, spiceboxv1alpha1.ReasonSecretKeyMissing)
	})
}
