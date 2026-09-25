//go:build integration

// Watch-wiring integration test for the SidecarToolbox controller.
//
// SidecarToolbox validation reads the referenced SpiceboxClass's Valid
// condition (classCheck in Reconcile). If SetupWithManager does not watch
// SpiceboxClass, a toolbox that reconciled before its class was stamped
// Valid=True keeps advertising Valid=False/ClassInvalid until the reconciler's
// own RevalidateInterval (5m in production) comes around — and because
// AgentClass mirrors the toolbox's Valid condition, the whole agent reads
// "not ready" with a stale, misleading message for that entire window.
//
// We hit this with an `oap agent install` bundle: the SpiceboxClass,
// SidecarToolbox and AgentClass are applied in the same instant, the toolbox
// won the race, and the agent sat at
// Valid=False/AgentClassSidecarToolboxInvalid pointing at a SpiceboxClass that
// was already Valid=True.
//
// The test pins RevalidateInterval far above the assertion window so a pass
// is proof of the watch rather than of the interval eventually firing.
package sidecartoolbox_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/sidecartoolbox"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// revalidateNeverInTest is the RevalidateInterval handed to the reconciler
// under test. It is deliberately orders of magnitude above every assertion
// window below so that the periodic requeue cannot be what makes a test pass —
// only a watch-driven re-reconcile can.
const revalidateNeverInTest = time.Hour

// startToolboxManager wires the SidecarToolbox reconciler to a real manager
// (so reconciles arrive via watches, not direct calls) and blocks until the
// cache has synced. SkipProbe is set because envtest schedules no pods.
func startToolboxManager(t *testing.T, env *testenv.Env) {
	t.Helper()
	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:         env.Scheme,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		Controller:     ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
		LeaderElection: false,
	})
	require.NoError(t, err, "ctrl.NewManager")
	require.NoError(t, (&sidecartoolbox.Reconciler{
		Client:             mgr.GetClient(),
		SkipProbe:          true,
		RevalidateInterval: revalidateNeverInTest,
	}).SetupWithManager(mgr), "sidecartoolbox.SetupWithManager")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
	require.True(t, mgr.GetCache().WaitForCacheSync(ctx), "cache sync")
}

// eventuallyToolboxValid polls the named SidecarToolbox until its Valid
// condition matches. An empty wantReason matches any reason.
func eventuallyToolboxValid(t *testing.T, c client.Client, name string, want metav1.ConditionStatus, wantReason string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	var last *metav1.Condition
	for time.Now().Before(deadline) {
		var got spiceboxv1alpha1.SidecarToolbox
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &got); err == nil {
			cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SidecarToolboxConditionValid)
			last = cond
			if cond != nil && cond.Status == want && (wantReason == "" || cond.Reason == wantReason) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("SidecarToolbox %s never reached Valid=%s reason=%s within %v; last=%+v", name, want, wantReason, d, last)
}

// classFixtureNotYetValid creates a SpiceboxClass with NO Valid condition —
// the shape a freshly-applied bundle class has before its own controller
// reconciles it.
func classFixtureNotYetValid(t *testing.T, c client.Client, name string) *spiceboxv1alpha1.SpiceboxClass {
	t.Helper()
	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image: "spicebox-sandbox:dev",
			Resources: spiceboxv1alpha1.SpiceboxResources{
				CPU:              resource.MustParse("100m"),
				Memory:           resource.MustParse("64Mi"),
				EphemeralStorage: resource.MustParse("16Mi"),
			},
			Tools: []spiceboxv1alpha1.SpiceboxTool{{Name: "sre", Command: []string{"/usr/local/bin/sre-tool"}}},
		},
	}
	require.NoError(t, c.Create(context.Background(), cls), "create SpiceboxClass %s", name)
	return cls
}

// toolboxFixture creates a SidecarToolbox that is valid in every respect
// except that its sandbox class may not be Valid=True yet.
func toolboxFixture(t *testing.T, c client.Client, name, className string) *spiceboxv1alpha1.SidecarToolbox {
	t.Helper()
	tb := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:         name,
			Version:      "1",
			Source:       spiceboxv1alpha1.SidecarToolboxSource{Image: "example.com/mcp:v1"},
			Sandbox:      spiceboxv1alpha1.SidecarToolboxSandbox{Class: className},
			Transport:    spiceboxv1alpha1.SidecarToolboxTransport{Port: 8080},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "github-pat"},
			Tools:        []spiceboxv1alpha1.MCPServerTool{{Name: "echo"}},
		},
	}
	require.NoError(t, c.Create(context.Background(), tb), "create SidecarToolbox %s", name)
	return tb
}

// TestSidecarToolbox_ReReconcilesOnSpiceboxClassValid is the regression guard
// for the install-ordering race: the toolbox reconciles first and fails closed
// on a class that has not been stamped Valid=True yet. When the class then
// flips to Valid=True, nothing about the SidecarToolbox itself changes, so
// only a SpiceboxClass watch can re-enqueue it. Fails (times out) when
// SetupWithManager does not register that watch.
func TestSidecarToolbox_ReReconcilesOnSpiceboxClassValid(t *testing.T) {
	env := testenv.Shared(t)
	startToolboxManager(t, env)
	ctx := context.Background()

	cls := classFixtureNotYetValid(t, env.Client, "sre-sandbox-watch")
	toolboxFixture(t, env.Client, "k8s-mcp-watch", "sre-sandbox-watch")

	// Fail closed first — proves the controller reconciled and saw a class
	// that was not yet Valid=True.
	eventuallyToolboxValid(t, env.Client, "k8s-mcp-watch", metav1.ConditionFalse,
		spiceboxv1alpha1.ReasonSidecarToolboxClassInvalid, 5*time.Second)

	// Stamp the class Valid=True, as the spiceboxclass controller would.
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(cls), cls),
		"refresh SpiceboxClass before status flip")
	cls.Status = spiceboxv1alpha1.SpiceboxClassStatus{Conditions: []metav1.Condition{{
		Type:               spiceboxv1alpha1.SpiceboxClassConditionValid,
		Status:             metav1.ConditionTrue,
		Reason:             "Resolved",
		LastTransitionTime: metav1.Now(),
	}}}
	require.NoError(t, env.Client.Status().Update(ctx, cls), "flip SpiceboxClass to Valid=True")

	// 10s is ~1/360th of the pinned RevalidateInterval, so reaching Valid=True
	// inside it is evidence of the watch, not of the periodic requeue.
	eventuallyToolboxValid(t, env.Client, "k8s-mcp-watch", metav1.ConditionTrue,
		spiceboxv1alpha1.ReasonSidecarToolboxSpecOK, 10*time.Second)
}

// TestSidecarToolbox_ReReconcilesOnSpiceboxClassCreate covers the other half
// of the same race: the class does not exist at all when the toolbox first
// reconciles (ClassMissing rather than ClassInvalid). Creating it later must
// also re-enqueue the toolbox — a watch that only fires on update would leave
// this case stalled for a full revalidate interval.
func TestSidecarToolbox_ReReconcilesOnSpiceboxClassCreate(t *testing.T) {
	env := testenv.Shared(t)
	startToolboxManager(t, env)
	ctx := context.Background()

	toolboxFixture(t, env.Client, "k8s-mcp-late-class", "sre-sandbox-late")
	eventuallyToolboxValid(t, env.Client, "k8s-mcp-late-class", metav1.ConditionFalse,
		spiceboxv1alpha1.ReasonSidecarToolboxClassMissing, 5*time.Second)

	cls := classFixtureNotYetValid(t, env.Client, "sre-sandbox-late")
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(cls), cls), "refresh SpiceboxClass")
	cls.Status = spiceboxv1alpha1.SpiceboxClassStatus{Conditions: []metav1.Condition{{
		Type:               spiceboxv1alpha1.SpiceboxClassConditionValid,
		Status:             metav1.ConditionTrue,
		Reason:             "Resolved",
		LastTransitionTime: metav1.Now(),
	}}}
	require.NoError(t, env.Client.Status().Update(ctx, cls), "stamp SpiceboxClass Valid=True")

	eventuallyToolboxValid(t, env.Client, "k8s-mcp-late-class", metav1.ConditionTrue,
		spiceboxv1alpha1.ReasonSidecarToolboxSpecOK, 10*time.Second)
}
