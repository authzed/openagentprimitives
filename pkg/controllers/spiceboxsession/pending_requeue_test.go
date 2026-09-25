package spiceboxsession

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
)

// statusScriptRuntime is a Runtime whose Status walks a scripted sequence of
// Statuses, one per call, sticking on the last. Ensure returns a fixed handle
// so the session is never on the firstBind path. Teardown/Executor panic:
// neither is on the path under test, so a test that reaches one fails loudly
// rather than passing on the wrong branch.
//
// Scripted rather than a single fixed Status, because the property under test
// is CONVERGENCE — that the polled state actually ends — and a runtime stuck
// on one answer forever can only ever demonstrate that the state is reported.
//
// Whole Statuses rather than bare Phases: the trigger under test is
// Status.RequiresPolling, and a Phase-only script could not express the case
// that matters most — a Pending that does NOT ask to be polled.
type statusScriptRuntime struct {
	mu     sync.Mutex
	script []sandboxkinds.Status
	calls  int
}

func (f *statusScriptRuntime) Ensure(context.Context, sandboxkinds.EnsureRequest) (sandboxkinds.Handle, error) {
	return sandboxkinds.Handle{Kind: "scripted-kind", Ref: "default/demo-session-pod", Prewarmed: true}, nil
}

func (f *statusScriptRuntime) Status(context.Context, sandboxkinds.Handle) (sandboxkinds.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	if i >= len(f.script) {
		i = len(f.script) - 1
	}
	f.calls++
	return f.script[i], nil
}

func (f *statusScriptRuntime) Teardown(context.Context, sandboxkinds.Handle) error {
	panic("statusScriptRuntime.Teardown: not on the path under test")
}

func (f *statusScriptRuntime) Executor(sandboxkinds.Handle) (exec.Executor, error) {
	panic("statusScriptRuntime.Executor: not on the path under test")
}

func (f *statusScriptRuntime) Watches() []sandboxkinds.Watch { return nil }

var _ sandboxkinds.Runtime = (*statusScriptRuntime)(nil)

// boundSession is a session past first bind: finalizer present, class snapshot
// frozen, and status.sandbox already set — so Reconcile reaches rt.Status
// rather than returning on the firstBind branch.
func boundSession(name string) *spiceboxv1alpha1.SpiceboxSession {
	return &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Finalizers: []string{spiceboxv1alpha1.FinalizerSpiceboxSession},
		},
		Spec: spiceboxv1alpha1.SpiceboxSessionSpec{Class: "demo-class"},
		Status: spiceboxv1alpha1.SpiceboxSessionStatus{
			ResolvedClass: &spiceboxv1alpha1.SpiceboxClassSpec{
				Image:   "demo.invalid/spicebox-sandbox:test",
				Sandbox: spiceboxv1alpha1.SandboxBackend{Kind: "scripted-kind"},
			},
			Sandbox: &spiceboxv1alpha1.SandboxHandle{
				Kind: "scripted-kind", Ref: "default/" + name + "-pod", Prewarmed: true,
			},
		},
	}
}

func pendingRequeueFixture(t *testing.T, rt sandboxkinds.Runtime, sess *spiceboxv1alpha1.SpiceboxSession) (*Reconciler, client.Client) {
	t.Helper()
	s := apiruntime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-class"},
		Spec:       spiceboxv1alpha1.SpiceboxClassSpec{Image: "demo.invalid/spicebox-sandbox:test"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sess, cls).
		WithStatusSubresource(&spiceboxv1alpha1.SpiceboxSession{}).Build()
	return &Reconciler{Client: c, Runtimes: sandboxkinds.Runtimes{"scripted-kind": rt}}, c
}

func reconcileSession(t *testing.T, r *Reconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
	})
	require.NoError(t, err)
	return res
}

// LIVENESS, the other half of a state no watch can end. Every backend watch is
// registered as Owns(...) (controller-owner mapping only), and neither an
// ADOPTED agent-sandbox Sandbox (controller-owned by the SandboxClaim) nor the
// POD under any Sandbox (controller-owned by the Sandbox) maps back to the
// SpiceboxSession. The session-owned claim DOES map back, but it goes Ready in
// the same pass that merely requests the pod change, so it fires at-or-before
// the thing being waited on and then has nothing further to say.
//
// Without a requeue that leaves exactly one free pass — the self-trigger from
// the status write — and then a stall until the manager's ~10h resync. A
// byte-identical re-write emits no event, so there is not even a second one.
func TestReconcile_SandboxThatAsksToBePolledRequeues(t *testing.T) {
	rt := &statusScriptRuntime{script: []sandboxkinds.Status{
		{Phase: sandboxkinds.PhasePending, Reason: "AwaitingSessionLabels", RequiresPolling: true},
	}}
	r, _ := pendingRequeueFixture(t, rt, boundSession("demo-session"))

	res := reconcileSession(t, r, "demo-session")
	assert.Positive(t, res.RequeueAfter,
		"a state no watch of the backend's can end MUST carry its own retry, or the "+
			"session stalls until the manager's ~10h resync")
	assert.LessOrEqual(t, res.RequeueAfter, sandboxPendingPollInterval,
		"the interval must stay in seconds: a pre-warmed session that took minutes to become "+
			"usable would be slower than a cold one")
}

// THE REGRESSION GUARD, and the reason the trigger is a backend flag rather
// than the phase. A COLD pod-backend sandbox is Pending for its entire startup
// — that is the overwhelmingly common state in any real cluster — and its
// Owns(&corev1.Pod{}) watch ends it. Polling on Pending generally turned every
// starting session in the cluster into a 3s timer, which the e2e suite caught
// as a suite-wide load regression against a previously clean baseline.
//
// A backend that does not ask must cost exactly nothing.
func TestReconcile_PendingSandboxThatDoesNotAskIsNotPolled(t *testing.T) {
	rt := &statusScriptRuntime{script: []sandboxkinds.Status{
		// Exactly what pkg/tools/sandboxkinds/pod reports while its pod schedules:
		// Pending, and watch-covered, so RequiresPolling is left false.
		{Phase: sandboxkinds.PhasePending, Reason: sandboxkinds.ReasonNotReady},
	}}
	r, _ := pendingRequeueFixture(t, rt, boundSession("demo-session"))

	res := reconcileSession(t, r, "demo-session")
	assert.Zero(t, res.RequeueAfter,
		"a Pending the backend's own watch will end must NOT be polled: every session in "+
			"the cluster is Pending while it starts, so polling on phase is a steady-state "+
			"load increase, not a backstop")
}

// A Ready sandbox must NOT poll: the work is done, and a permanent requeue on
// every healthy session would be a cluster-wide cost with nothing to observe.
func TestReconcile_ReadySandboxDoesNotRequeue(t *testing.T) {
	rt := &statusScriptRuntime{script: []sandboxkinds.Status{
		{Phase: sandboxkinds.PhaseReady, Reason: sandboxkinds.ReasonReady},
	}}
	r, _ := pendingRequeueFixture(t, rt, boundSession("demo-session"))

	res := reconcileSession(t, r, "demo-session")
	assert.Zero(t, res.RequeueAfter, "a Ready sandbox has nothing left to wait on")
}

// CONVERGENCE, not merely "reports Pending". The scripted runtime stands in for
// the agent-sandbox adoption window: the first observation finds the session
// label not yet patched onto the adopted pod (Pending), the second — the pass
// the requeue itself schedules — finds it landed (Ready).
//
// The second Reconcile here IS the requeued pass: nothing else would have
// driven it, which is exactly the point. The assertions walk the whole
// trajectory, so a fix that requeued forever (never clearing the requeue on
// Ready) or that reported Ready immediately would both fail.
func TestReconcile_PendingSandboxConvergesOnTheRequeuedPass(t *testing.T) {
	rt := &statusScriptRuntime{script: []sandboxkinds.Status{
		// The label has not reached the adopted pod yet, and no watch of this
		// backend's will say when it does.
		{Phase: sandboxkinds.PhasePending, Reason: "AwaitingSessionLabels", RequiresPolling: true},
		// The base Sandbox controller patched it.
		{Phase: sandboxkinds.PhaseReady, Reason: sandboxkinds.ReasonReady},
	}}
	r, c := pendingRequeueFixture(t, rt, boundSession("demo-session"))

	first := reconcileSession(t, r, "demo-session")
	require.Positive(t, first.RequeueAfter, "precondition: the Pending pass schedules its own retry")

	var mid spiceboxv1alpha1.SpiceboxSession
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "demo-session", Namespace: "default"}, &mid))
	assert.False(t, isReady(&mid), "precondition: the session is not Ready while Pending")

	// The pass the RequeueAfter above scheduled. Nothing else drives it.
	second := reconcileSession(t, r, "demo-session")
	assert.Zero(t, second.RequeueAfter, "once Ready, the poll must stop")

	var got spiceboxv1alpha1.SpiceboxSession
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "demo-session", Namespace: "default"}, &got))
	assert.True(t, isReady(&got),
		"the session must actually reach Ready on the requeued pass — reporting Pending "+
			"forever is the failure this requeue exists to prevent")
}

// isReady reads the Ready condition the way a consumer would.
func isReady(sess *spiceboxv1alpha1.SpiceboxSession) bool {
	for _, c := range sess.Status.Conditions {
		if c.Type == spiceboxv1alpha1.SpiceboxSessionConditionReady {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}
