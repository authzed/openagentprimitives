package agentsandbox_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimepkg "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxextv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	execfake "github.com/authzed/openagentprimitives/pkg/tools/exec/fake"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/agentsandbox"
	// Registers the "image" toolchain delivery kind so BuildClassSpec can
	// render a mount's init container. Production reaches this registration
	// through internal/cmd/operator's own blank import; this package's tests need their
	// own since they call BuildClassSpec (via ReconcilePool/Ensure) directly
	// with non-empty toolchain mounts.
	_ "github.com/authzed/openagentprimitives/pkg/tools/toolchain/kinds/image"
)

// TRAP 1 (parent spec §5.2). The workspace PVC must be mounted through
// podTemplate. volumeClaimTemplates is StatefulSet-style PER-SANDBOX storage:
// using it gives every bundle its own empty workspace, and the symptom -- "the
// coding agent cannot see the checkout" -- points nowhere near the cause. The
// CRD also marks the field immutable, so a wrong value cannot be corrected in
// place. This trap is silent in production, so it gets an explicit assertion.
func TestEnsure_WorkspaceRidesInPodTemplateNotVolumeClaimTemplates(t *testing.T) {
	rt, c := newTestRuntime(t)
	sess := sessionWithSharedWorkspace(t, c)

	_, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{Session: sess, Class: testClass()})
	require.NoError(t, err)

	sb := getSandbox(t, c, sess)
	assert.Empty(t, sb.Spec.VolumeClaimTemplates,
		"volumeClaimTemplates must stay empty: it is per-sandbox storage and would "+
			"give each bundle its own empty workspace")

	var found bool
	for _, v := range sb.Spec.PodTemplate.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "demo-workspace" {
			found = true
		}
	}
	assert.True(t, found, "the shared workspace claim must be a volume on the pod template")
}

// TRAP 2 (parent spec §5.1). Lifecycle overlaps AP's TTL controller and
// OperatingMode overlaps idle-sleep/reap. AP's controllers stay authoritative;
// two controllers racing to delete the same sandbox is a bug.
//
// Asserted on the object WE BUILD, not on one read back from a real API
// server: OperatingMode carries +kubebuilder:default=Running, so a round trip
// through envtest would report "Running" and the assertion would pass for the
// wrong reason. The fake client applies no defaults, which is what makes this
// test meaningful.
func TestEnsure_LeavesLifecycleAndOperatingModeToAP(t *testing.T) {
	rt, c := newTestRuntime(t)
	sess := newSession("demo-session")

	_, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{Session: sess, Class: testClass()})
	require.NoError(t, err)

	sb := getSandbox(t, c, sess)
	assert.Empty(t, sb.Spec.OperatingMode, "AP's idle-sleep controller owns suspension")
	assert.Nil(t, sb.Spec.ShutdownTime, "AP's TTL controller owns expiry")
	assert.Nil(t, sb.Spec.ShutdownPolicy, "AP's TTL controller owns expiry")
}

// The status write that persists a handle can fail AFTER Ensure succeeds, so
// the next reconcile re-enters with no handle and must converge on the same
// sandbox rather than creating a second one.
func TestEnsure_IsIdempotent(t *testing.T) {
	rt, c := newTestRuntime(t)
	sess := newSession("demo-session")
	req := sandboxkinds.EnsureRequest{Session: sess, Class: testClass()}

	first, err := rt.Ensure(t.Context(), req)
	require.NoError(t, err)
	second, err := rt.Ensure(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, first, second, "a second Ensure must return the same handle")

	var list sandboxv1beta1.SandboxList
	require.NoError(t, c.List(t.Context(), &list))
	assert.Len(t, list.Items, 1, "a second Ensure must not create a second sandbox")
}

func TestEnsure_HandleIsOwnedByThisKind(t *testing.T) {
	rt, _ := newTestRuntime(t)
	h, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{
		Session: newSession("demo-session"), Class: testClass()})
	require.NoError(t, err)
	assert.Equal(t, agentsandbox.KindName, h.Kind)
	// podspec.PodNameFor, not the bare session name: the same helper the pod
	// kind uses, so a session's sandbox has one predictable name regardless of
	// which backend renders it (verified against pod/kind_test.go, which
	// asserts the identical "<session>-pod" convention for the pod kind).
	assert.Equal(t, "default/demo-session-pod", h.Ref)
}

// A missing workspace claim is a prerequisite AP itself is still creating, not
// a failure. Returning ErrPreconditionPending makes the session controller
// report Progressing and requeue instead of failing the session outright.
func TestEnsure_WaitsForAnAbsentWorkspaceClaim(t *testing.T) {
	rt, _ := newTestRuntime(t) // no PVC created
	sess := newSession("demo-session")
	sess.Spec.Workspace = v1alpha1.WorkspaceConfig{
		Mode: v1alpha1.WorkspaceShared, SharedClaimName: "demo-workspace",
	}

	_, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{Session: sess, Class: testClass()})
	require.Error(t, err)
	assert.ErrorIs(t, err, sandboxkinds.ErrPreconditionPending)
}

func TestEnsure_RequiresASession(t *testing.T) {
	rt, _ := newTestRuntime(t)
	_, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{Class: testClass()})
	require.Error(t, err)
}

func newSession(name string) *v1alpha1.SpiceboxSession {
	return &v1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: types.UID(name + "-uid"),
		},
		Spec: v1alpha1.SpiceboxSessionSpec{Class: "demo-class"},
	}
}

func testClass() v1alpha1.SpiceboxClassSpec {
	return v1alpha1.SpiceboxClassSpec{Image: "demo.invalid/spicebox-sandbox:test"}
}

func getSandbox(t *testing.T, c client.Client, sess *v1alpha1.SpiceboxSession) *sandboxv1beta1.Sandbox {
	t.Helper()
	var sb sandboxv1beta1.Sandbox
	// podspec.PodNameFor, not the bare session name: Ensure names the Sandbox
	// with the same helper the pod kind uses (see the Ref assertion in
	// TestEnsure_HandleIsOwnedByThisKind).
	require.NoError(t, c.Get(t.Context(),
		types.NamespacedName{Namespace: sess.Namespace, Name: podspec.PodNameFor(sess)}, &sb))
	return &sb
}

func sessionWithSharedWorkspace(t *testing.T, c client.Client) *v1alpha1.SpiceboxSession {
	t.Helper()
	sess := newSession("demo-session")
	sess.Spec.Workspace = v1alpha1.WorkspaceConfig{
		Mode: v1alpha1.WorkspaceShared, SharedClaimName: "demo-workspace",
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-workspace", Namespace: "default"},
	}
	require.NoError(t, c.Create(t.Context(), pvc))
	return sess
}

// newTestRuntime builds a runtime with NO MonitoringPublish, deliberately: a
// nil PublishFunc is what an operator without a NATS bus supplies, so every
// test in this file that never mentions monitoring is also continuously
// exercising the nil-guard on the emit path.
func newTestRuntime(t *testing.T) (sandboxkinds.Runtime, client.Client) {
	t.Helper()
	rt, c, _ := newRuntime(t, nil)
	return rt, c
}

// newTestRuntimeWithMonitoring is newTestRuntime plus a recorder standing in
// for the operator's NATS connection.
func newTestRuntimeWithMonitoring(t *testing.T) (sandboxkinds.Runtime, client.Client, *monitoringRecorder) {
	t.Helper()
	mon := &monitoringRecorder{}
	rt, c, _ := newRuntime(t, mon.publish)
	return rt, c, mon
}

func newRuntime(
	t *testing.T, publish channelevents.PublishFunc,
) (sandboxkinds.Runtime, client.Client, *runtimepkg.Scheme) {
	t.Helper()
	s := newScheme(t) // defined in kind_test.go — same package
	c := fake.NewClientBuilder().WithScheme(s).WithRESTMapper(mapperWithSandbox(t, s)).
		WithStatusSubresource(&sandboxv1beta1.Sandbox{}, &sandboxextv1beta1.SandboxClaim{}).Build()
	rt, err := agentsandbox.Kind{}.NewRuntime(sandboxkinds.Deps{
		Client: c, ExecFor: execfake.New().For, MonitoringPublish: publish,
	})
	require.NoError(t, err)
	return rt, c, s
}

// newRuntimeWithoutPrewarmCRDs builds a runtime for a cluster with the base
// agent-sandbox CRD and no extensions bundle.
func newRuntimeWithoutPrewarmCRDs(t *testing.T) (sandboxkinds.Runtime, client.Client) {
	t.Helper()
	s := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithRESTMapper(baseOnlyMapper(t)).
		WithStatusSubresource(&sandboxv1beta1.Sandbox{}).
		WithInterceptorFuncs(refuseExtensionsGroup()).Build()
	rt, err := agentsandbox.Kind{}.NewRuntime(sandboxkinds.Deps{
		Client: c, ExecFor: execfake.New().For,
	})
	require.NoError(t, err, "the base CRD is present, so the backend itself is available")
	return rt, c
}

// monitoringRecorder captures what a Runtime fans out to the monitoring
// channel, standing in for the operator's NATS connection. It decodes rather
// than storing raw bytes so assertions read against the published struct, and
// it asserts the subject, which nothing else covers: an event published on the
// wrong subject reaches no monitoring Channel at all.
type monitoringRecorder struct {
	mu     sync.Mutex
	events []channelevents.MonitoringEvent
	errs   []error
}

func (m *monitoringRecorder) publish(subject string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if subject != channelevents.MonitoringEventSubject {
		m.errs = append(m.errs, fmt.Errorf("published on %q, want %q",
			subject, channelevents.MonitoringEventSubject))
		return nil
	}
	var ev channelevents.MonitoringEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		m.errs = append(m.errs, err)
		return nil
	}
	m.events = append(m.events, ev)
	return nil
}

func (m *monitoringRecorder) all() []channelevents.MonitoringEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]channelevents.MonitoringEvent(nil), m.events...)
}

// Phase is the contract; Reason is backend detail. Consumers switch on Phase
// only, so this mapping is where a backend's vocabulary becomes AP's.
func TestStatus_MapsBackendConditionsToPhases(t *testing.T) {
	cases := []struct {
		name       string
		conditions []metav1.Condition
		deleting   bool
		wantPhase  sandboxkinds.Phase
		wantReason string
	}{
		{
			name:      "no conditions yet: Pending/Creating",
			wantPhase: sandboxkinds.PhasePending, wantReason: sandboxkinds.ReasonCreating,
		},
		{
			name:       "Ready=True: Ready",
			conditions: []metav1.Condition{cond("Ready", metav1.ConditionTrue, "DependenciesReady")},
			wantPhase:  sandboxkinds.PhaseReady, wantReason: sandboxkinds.ReasonReady,
		},
		{
			name:       "dependencies not ready: Pending/WaitingForPrerequisite",
			conditions: []metav1.Condition{cond("Ready", metav1.ConditionFalse, "DependenciesNotReady")},
			wantPhase:  sandboxkinds.PhasePending, wantReason: sandboxkinds.ReasonWaitingForPrereq,
		},
		{
			// Any other Ready=False reason falls through to the generic
			// not-ready mapping, distinct from the DependenciesNotReady case
			// above.
			name:       "Ready=False with an unrecognized reason: Pending/NotReady",
			conditions: []metav1.Condition{cond("Ready", metav1.ConditionFalse, "SomeOtherReason")},
			wantPhase:  sandboxkinds.PhasePending, wantReason: sandboxkinds.ReasonNotReady,
		},
		{
			name:       "Finished/PodFailed: Failed/Crashed",
			conditions: []metav1.Condition{cond("Finished", metav1.ConditionTrue, "PodFailed")},
			wantPhase:  sandboxkinds.PhaseFailed, wantReason: sandboxkinds.ReasonCrashed,
		},
		{
			// Expiry is the sandbox reaching its end, not failing at its job.
			name:       "Finished/SandboxExpired: Gone, not Failed",
			conditions: []metav1.Condition{cond("Finished", metav1.ConditionTrue, "SandboxExpired")},
			wantPhase:  sandboxkinds.PhaseGone, wantReason: sandboxkinds.ReasonGone,
		},
		{
			name:       "Finished/PodSucceeded: Gone",
			conditions: []metav1.Condition{cond("Finished", metav1.ConditionTrue, "PodSucceeded")},
			wantPhase:  sandboxkinds.PhaseGone, wantReason: sandboxkinds.ReasonGone,
		},
		{
			// A terminating sandbox will never accept another exec. Reporting
			// its last live phase would let a caller dispatch a tool call into
			// something shutting down, so deletion outranks every condition.
			name:       "deleting outranks Ready=True: Gone/Terminating",
			conditions: []metav1.Condition{cond("Ready", metav1.ConditionTrue, "DependenciesReady")},
			deleting:   true,
			wantPhase:  sandboxkinds.PhaseGone, wantReason: sandboxkinds.ReasonTerminating,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, c := newTestRuntime(t)
			sess := newSession("demo-session")
			h, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{Session: sess, Class: testClass()})
			require.NoError(t, err)

			sb := getSandbox(t, c, sess)
			sb.Status.Conditions = tc.conditions
			require.NoError(t, c.Status().Update(t.Context(), sb))
			if tc.deleting {
				// The fake client deletes an object outright on Delete unless it
				// carries a finalizer; without one DeletionTimestamp is never
				// observed and this subtest would silently exercise the
				// not-found path instead of the terminating one.
				sb.Finalizers = []string{"test/hold"}
				require.NoError(t, c.Update(t.Context(), sb))
				require.NoError(t, c.Delete(t.Context(), sb))
			}

			got, err := rt.Status(t.Context(), h)
			require.NoError(t, err)
			assert.Equal(t, tc.wantPhase, got.Phase)
			assert.Equal(t, tc.wantReason, got.Reason)
			// THE NEGATIVE HALF of Status.RequiresPolling, asserted across every
			// cold Status return this table reaches. Only the adopted
			// awaiting-labels state may ask to be polled; everything here is a
			// Sandbox condition, which Owns(&Sandbox{}) already observes. A
			// backend that set the flag per-PHASE rather than per-STATE would
			// turn every starting sandbox in the cluster into a timer — the
			// suite-wide load regression the e2e gate caught.
			assert.False(t, got.RequiresPolling,
				"a state the backend's own Sandbox watch will end must not ask to be polled")
		})
	}
}

// A sandbox that no longer exists is Gone, not an error: a completed or reaped
// session re-enters Status on every reconcile and must not error each time.
func TestStatus_AbsentSandboxIsGoneNotAnError(t *testing.T) {
	rt, _ := newTestRuntime(t)
	got, err := rt.Status(t.Context(), sandboxkinds.Handle{
		Kind: agentsandbox.KindName, Ref: "default/never-created"})
	require.NoError(t, err)
	assert.Equal(t, sandboxkinds.PhaseGone, got.Phase)
	assert.False(t, got.RequiresPolling, "a terminal state has nothing to poll for")
}

func TestStatus_RejectsAForeignHandle(t *testing.T) {
	rt, _ := newTestRuntime(t)
	_, err := rt.Status(t.Context(), sandboxkinds.Handle{Kind: "pod", Ref: "default/x"})
	require.Error(t, err, "a handle from another kind must never be interpreted here")
}

func cond(t string, s metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{
		Type: t, Status: s, Reason: reason,
		LastTransitionTime: metav1.Now(), ObservedGeneration: 0,
	}
}

func TestTeardown_DeletesTheSandbox(t *testing.T) {
	rt, c := newTestRuntime(t)
	sess := newSession("demo-session")
	h, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{Session: sess, Class: testClass()})
	require.NoError(t, err)

	// podspec.PodNameFor, not the bare session name: Ensure names the Sandbox
	// with the same helper the pod kind uses (see getSandbox).
	key := types.NamespacedName{Namespace: sess.Namespace, Name: podspec.PodNameFor(sess)}
	var sb sandboxv1beta1.Sandbox
	// Prove the sandbox is really there first: asserting only that it is
	// absent afterwards cannot distinguish a working Teardown from one that
	// never ran.
	require.NoError(t, c.Get(t.Context(), key, &sb), "precondition: Ensure created the sandbox")

	require.NoError(t, rt.Teardown(t.Context(), h))

	err = c.Get(t.Context(), key, &sb)
	assert.True(t, apierrors.IsNotFound(err), "Teardown must delete the sandbox")
}

// Teardown runs from a finalizer, which re-enters on every reconcile until it
// succeeds. An already-absent sandbox must be success or the finalizer wedges
// and the session can never be deleted.
func TestTeardown_AlreadyAbsentIsSuccess(t *testing.T) {
	rt, _ := newTestRuntime(t)
	require.NoError(t, rt.Teardown(t.Context(), sandboxkinds.Handle{
		Kind: agentsandbox.KindName, Ref: "default/never-created"}))
}

// The agent-sandbox controller stamps the pod name as an annotation, which is
// a direct read rather than a label-selector parse plus a pod List.
func TestExecutor_ResolvesThePodFromTheAnnotation(t *testing.T) {
	rt, c := newTestRuntime(t)
	sess := newSession("demo-session")
	h, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{Session: sess, Class: testClass()})
	require.NoError(t, err)

	sb := getSandbox(t, c, sess)
	sb.Annotations = map[string]string{"agents.x-k8s.io/pod-name": "demo-session-abc12"}
	require.NoError(t, c.Update(t.Context(), sb))

	ex, err := rt.Executor(h)
	require.NoError(t, err)
	require.NotNil(t, ex)
}

// No annotation means the controller has not produced a pod yet. That must be
// an error, never an executor bound to an empty pod name -- the latter would
// dispatch a tool call at nothing and fail somewhere far less legible than the
// real cause.
func TestExecutor_ErrorsWhenNoPodYet(t *testing.T) {
	rt, _ := newTestRuntime(t)
	h, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{
		Session: newSession("demo-session"), Class: testClass()})
	require.NoError(t, err)

	_, err = rt.Executor(h)
	require.Error(t, err)
}

// The Sandbox watch is UNCONDITIONAL — it is how a cold sandbox's readiness
// reaches the session controller, and every session on this backend has one
// whether or not the cluster can pre-warm. Only the claim watch is gated (see
// the two tests below), so this asserts the base watch survives BOTH
// configurations rather than just the one the other tests happen to build.
func TestWatches_ContributesTheSandboxWatchInEveryConfiguration(t *testing.T) {
	withPrewarm, _ := newTestRuntime(t)
	withoutPrewarm, _ := newRuntimeWithoutPrewarmCRDs(t)

	for name, rt := range map[string]sandboxkinds.Runtime{
		"prewarming available":   withPrewarm,
		"prewarming unavailable": withoutPrewarm,
	} {
		t.Run(name, func(t *testing.T) {
			w := rt.Watches()
			require.NotEmpty(t, w)
			assert.IsType(t, &sandboxv1beta1.Sandbox{}, w[0].Object)
		})
	}
}

// --- Pre-warm adoption -----------------------------------------------------
//
// A session whose rendered PodSpec is exactly its class's takes a ready
// sandbox out of the class warm pool instead of waiting for one to be created.
// Adoption happens entirely inside this backend's Ensure; the session path
// knows nothing about pooling, so every test here goes through the seam verbs.

// sessionLabelKey is the label AP's per-session sandbox NetworkPolicy selects
// on (pkg/controllers/agentsession/netpol.go). Spelled out here rather than
// imported so this file states the contract it is asserting.
const sessionLabelKey = "agentprimitives.authzed.com/session"

func listClaims(t *testing.T, c client.Client) []sandboxextv1beta1.SandboxClaim {
	t.Helper()
	var l sandboxextv1beta1.SandboxClaimList
	require.NoError(t, c.List(t.Context(), &l, client.InNamespace("default")))
	return l.Items
}

func listSandboxes(t *testing.T, c client.Client) []sandboxv1beta1.Sandbox {
	t.Helper()
	var l sandboxv1beta1.SandboxList
	require.NoError(t, c.List(t.Context(), &l, client.InNamespace("default")))
	return l.Items
}

// warmPoolFor creates className's SandboxWarmPool through ReconcilePool — the
// same call the spiceboxclass controller makes — and returns the class spec it
// was built from, so the caller's EnsureRequest renders an IDENTICAL PodSpec
// and the session is eligible.
//
// Deliberately not a hand-written SandboxWarmPool at a literal name.
// poolNameFor is unexported, and adoption claiming against a name that does
// not match the one ReconcilePool creates is the single most likely way this
// feature silently never fires — a cold start looks exactly like a cold
// cluster. Routing both sides through the real function makes them agree by
// construction instead of by a copied string.
func warmPoolFor(t *testing.T, rt sandboxkinds.Runtime, className string) v1alpha1.SpiceboxClassSpec {
	t.Helper()
	req := poolRequest() // defined in prewarm_test.go — same package
	req.ClassName = className
	require.NoError(t, requirePrewarmer(t, rt).ReconcilePool(t.Context(), req))
	return req.Class
}

// adoptSession builds the warm pool for "demo-class", Ensures an eligible
// session against it, and returns the session plus its (pre-warmed) handle.
func adoptSession(t *testing.T, rt sandboxkinds.Runtime) (*v1alpha1.SpiceboxSession, sandboxkinds.Handle) {
	t.Helper()
	class := warmPoolFor(t, rt, "demo-class")
	sess := newSession("demo-session")
	h, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{Session: sess, Class: class})
	require.NoError(t, err)
	require.True(t, h.Prewarmed, "precondition: an eligible session with a pool must adopt")
	return sess, h
}

func getClaim(t *testing.T, c client.Client, sess *v1alpha1.SpiceboxSession) *sandboxextv1beta1.SandboxClaim {
	t.Helper()
	var claim sandboxextv1beta1.SandboxClaim
	require.NoError(t, c.Get(t.Context(),
		types.NamespacedName{Namespace: sess.Namespace, Name: podspec.PodNameFor(sess)}, &claim))
	return &claim
}

// adoptedSandboxName is the name a bound claim reports. Deliberately NOT
// "<session>-pod": an adopted Sandbox is minted by the warm pool, so its name
// is pool-generated and unpredictable. A Status or Executor that ignored the
// claim and reused the handle's own name would resolve to nothing, and every
// assertion below would catch it.
const adoptedSandboxName = "demo-class-pool-x7k29"

// bindClaim stands in for the agent-sandbox controller handing a pooled
// sandbox over: it records that Sandbox's name on the claim's status.
func bindClaim(t *testing.T, c client.Client, sess *v1alpha1.SpiceboxSession, sandboxName string) {
	t.Helper()
	claim := getClaim(t, c, sess)
	claim.Status.SandboxStatus.Name = sandboxName
	require.NoError(t, c.Status().Update(t.Context(), claim))
}

// createAdoptedSandbox materializes the Sandbox a bound claim points at, with
// the given conditions and pod-name annotation.
func createAdoptedSandbox(t *testing.T, c client.Client, name string, conds []metav1.Condition, podName string) {
	t.Helper()
	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
	}
	if podName != "" {
		sb.Annotations = map[string]string{"agents.x-k8s.io/pod-name": podName}
	}
	require.NoError(t, c.Create(t.Context(), sb))
	if len(conds) > 0 {
		sb.Status.Conditions = conds
		require.NoError(t, c.Status().Update(t.Context(), sb))
	}
}

// Case 1: no warm pool for the class is an ordinary cold start, not a
// failure — a Sandbox created directly, Prewarmed false, and no claim left
// behind. This is also the ONLY shape that exists before a class opts into
// pre-warming, so it is the behaviour every session shipping today gets.
func TestEnsure_WithoutAWarmPoolCreatesTheSandboxDirectly(t *testing.T) {
	rt, c := newTestRuntime(t)
	sess := newSession("demo-session")

	h, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{Session: sess, Class: testClass()})
	require.NoError(t, err, "an absent pool must not be an error")

	assert.False(t, h.Prewarmed, "nothing was adopted, so the handle must not claim it was")
	assert.Empty(t, listClaims(t, c), "the cold path must not create a SandboxClaim")
	assert.Len(t, listSandboxes(t, c), 1, "the cold path still creates the Sandbox directly")
}

// Case 2: eligibility is decided by comparing the session's rendered PodSpec
// against podspec.BuildClassSpec(class), never by a hand-written list of
// session-specific features. A shared workspace adds a volume + mount + a
// working directory that a pool sandbox (built from the class spec alone)
// cannot have, so such a session must never adopt — it would silently receive
// a pod with no checkout mounted.
func TestEnsure_IneligibleSessionNeverClaimsEvenWithAPool(t *testing.T) {
	rt, c := newTestRuntime(t)
	class := warmPoolFor(t, rt, "demo-class")
	sess := sessionWithSharedWorkspace(t, c)

	h, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{Session: sess, Class: class})
	require.NoError(t, err)

	assert.False(t, h.Prewarmed)
	assert.Empty(t, listClaims(t, c),
		"a session whose rendered PodSpec differs from BuildClassSpec(class) must NOT adopt: "+
			"a pooled sandbox is built from the class spec alone, so it would be missing the "+
			"shared workspace volume entirely")
	assert.Len(t, listSandboxes(t, c), 1, "it takes the ordinary cold path instead")
}

// Case 3: an eligible session claims against the class's WARM POOL — not
// against a template. SandboxClaimSpec carries exactly one reference field,
// warmPoolRef; there is no template ref on a claim at all. That is why
// poolNameFor is a pure function of the class name and stays stable across
// class edits.
func TestEnsure_EligibleSessionClaimsAgainstTheClassWarmPool(t *testing.T) {
	rt, c := newTestRuntime(t)
	class := warmPoolFor(t, rt, "demo-class")
	sess := newSession("demo-session")

	h, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{Session: sess, Class: class})
	require.NoError(t, err)

	assert.True(t, h.Prewarmed, "an eligible session with a pool must adopt")
	assert.Equal(t, "default/demo-session-pod", h.Ref,
		"the handle names the CLAIM, whose name this backend chooses — the adopted "+
			"Sandbox's own name is pool-generated and cannot be predicted")

	claims := listClaims(t, c)
	require.Len(t, claims, 1, "exactly one claim")
	assert.Equal(t, podspec.PodNameFor(sess), claims[0].Name,
		"the claim takes the same predictable name the direct Sandbox would have, "+
			"which is what lets a re-entering Ensure find it")

	pools := listWarmPools(t, c)
	require.Len(t, pools, 1)
	assert.Equal(t, pools[0].Name, claims[0].Spec.WarmPoolRef.Name,
		"the claim must reference the pool ReconcilePool created for this class")

	assert.Empty(t, listSandboxes(t, c),
		"adoption creates no Sandbox of its own — the agent-sandbox controller hands one "+
			"over from the pool and names it itself")
}

// SECURITY. AP's per-session sandbox NetworkPolicy selects the sandbox POD by
// agentprimitives.authzed.com/session (pkg/controllers/agentsession/netpol.go).
// podspec.BuildClassSpec returns a bare PodSpec with NO ObjectMeta, so a
// warm-pool pod carries no such label; and the pool's template is deliberately
// NetworkPolicyManagement: Unmanaged, so AP's policy is the only one that could
// ever apply. Without this injection an adopted sandbox's pod would be selected
// by NO NetworkPolicy whatsoever — strictly less isolated than a cold one.
// additionalPodMetadata is the one field upstream lets a claim inject into the
// pod. Do not delete this as redundant metadata.
func TestEnsure_ClaimInjectsTheSessionLabelTheNetworkPolicySelects(t *testing.T) {
	rt, c := newTestRuntime(t)
	sess, _ := adoptSession(t, rt)

	claim := getClaim(t, c, sess)
	assert.Equal(t, sess.Name, claim.Spec.AdditionalPodMetadata.Labels[sessionLabelKey],
		"without this label AP's per-session NetworkPolicy selects the adopted pod NOT AT ALL")

	require.Len(t, claim.OwnerReferences, 1, "exactly one owner")
	assert.Equal(t, "SpiceboxSession", claim.OwnerReferences[0].Kind,
		"a deleted session must reclaim its claim even if Teardown never runs")
	assert.Equal(t, sess.Name, claim.OwnerReferences[0].Name)
	assert.Equal(t, sess.UID, claim.OwnerReferences[0].UID)

	// Upstream documents both as FORCING a cold start, so they buy nothing —
	// and the template's Disallowed policies would reject the claim outright.
	assert.Empty(t, claim.Spec.Env, "populating env would force a cold start and be rejected")
	assert.Empty(t, claim.Spec.VolumeClaimTemplates,
		"populating volumeClaimTemplates would force a cold start and be rejected")
	// AP's TTL controller owns expiry and its idle-sleep/reap controller owns
	// suspension, exactly as on the direct Sandbox path. Two controllers racing
	// to delete one sandbox is a bug.
	assert.Nil(t, claim.Spec.Lifecycle, "AP's TTL controller owns expiry, not the claim")
}

// Ensure must converge on ONE sandbox. The status write that persists a handle
// can fail after Ensure succeeds, so the next reconcile re-enters with nothing
// in hand.
func TestEnsure_IsIdempotentForAnAdoptedSession(t *testing.T) {
	rt, c := newTestRuntime(t)
	class := warmPoolFor(t, rt, "demo-class")
	sess := newSession("demo-session")
	req := sandboxkinds.EnsureRequest{Session: sess, Class: class}

	first, err := rt.Ensure(t.Context(), req)
	require.NoError(t, err)
	second, err := rt.Ensure(t.Context(), req)
	require.NoError(t, err)

	assert.Equal(t, first, second, "a second Ensure must return the same handle")
	assert.Len(t, listClaims(t, c), 1, "a second Ensure must not create a second claim")
	assert.Empty(t, listSandboxes(t, c), "a second Ensure must not fall back to a direct Sandbox")
}

// The claim lookup MUST come before the Sandbox lookup in Ensure. An adopted
// session's Sandbox carries a pool-generated name, so the Sandbox lookup can
// never find it — and if the claim were checked second, a session that later
// renders a DIFFERENT PodSpec would fail the eligibility test on re-entry and
// create a whole second sandbox, cold, alongside the one it already holds.
//
// Re-rendering differently is not hypothetical: the session's own spec and
// status feed podspec.Build (a shared workspace being stamped on, toolchains
// being resolved), and both land after the first Ensure.
func TestEnsure_StaysAdoptedWhenTheSessionLaterRendersDifferently(t *testing.T) {
	rt, c := newTestRuntime(t)
	class := warmPoolFor(t, rt, "demo-class")
	sess := newSession("demo-session")

	first, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{Session: sess, Class: class})
	require.NoError(t, err)
	require.True(t, first.Prewarmed, "precondition: the first pass adopted")

	// The AgentSession controller stamps a shared workspace onto the session
	// after it was created; the PVC exists so the workspace gate still passes.
	sess.Spec.Workspace = v1alpha1.WorkspaceConfig{
		Mode: v1alpha1.WorkspaceShared, SharedClaimName: "demo-workspace",
	}
	require.NoError(t, c.Create(t.Context(), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-workspace", Namespace: "default"},
	}))

	second, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{Session: sess, Class: class})
	require.NoError(t, err)

	assert.True(t, second.Prewarmed,
		"the session already holds a claim; re-deciding eligibility on re-entry would "+
			"disown it")
	assert.Len(t, listClaims(t, c), 1, "still exactly one claim")
	assert.Empty(t, listSandboxes(t, c),
		"re-entry must NOT create a second, cold sandbox alongside the adopted one")
}

// THE MIRROR OF THE STALE-SANDBOX GUARD. A session's class spec is FROZEN onto
// status.resolvedClass at first bind, while the pool's template is rendered
// from the class CR's CURRENT spec — and the freeze and the adoption are always
// separate reconcile passes. So an admin editing the class re-points the pool
// (and, via the Recreate update strategy, rebuilds every warm sandbox at the
// new shape) while a session mid-flight still renders the OLD shape.
//
// Comparing the session's rendered PodSpec against BuildClassSpec(req.Class)
// cannot catch that on its own: BOTH sides derive from the same frozen
// snapshot, so they agree with each other while disagreeing with the pool. Only
// comparing the pool's own TemplateRef against the template name the frozen
// spec would produce can. Without that check this session adopts a sandbox
// running the wrong image, limits and mounts, while status.resolvedClass still
// says otherwise.
func TestEnsure_DoesNotAdoptWhenThePoolHasMovedPastTheSessionsFrozenClass(t *testing.T) {
	rt, c := newTestRuntime(t)
	pw := requirePrewarmer(t, rt)

	// The class shape this session froze at, and the pool built from it.
	frozen := warmPoolFor(t, rt, "demo-class")

	// The admin edits the class. ReconcilePool re-points the SAME pool at a new
	// template; Recreate rebuilds its warm sandboxes at the new image.
	edited := poolRequest()
	edited.Class.Image = "demo.invalid/spicebox-sandbox:v2"
	require.NoError(t, pw.ReconcilePool(t.Context(), edited))
	require.NotEqual(t, edited.Class.Image, frozen.Image, "precondition: the class really moved")

	// This session still holds the PRE-EDIT snapshot.
	sess := newSession("demo-session")
	h, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{Session: sess, Class: frozen})
	require.NoError(t, err, "a pool that moved past this session is a cold start, not a failure")

	assert.False(t, h.Prewarmed,
		"the pool now holds a DIFFERENT class shape; adopting from it would hand this "+
			"session the wrong image while status.resolvedClass still named the old one")
	assert.Empty(t, listClaims(t, c),
		"no claim may be created against a pool whose template this session did not freeze at")
	assert.Len(t, listSandboxes(t, c), 1, "it takes the ordinary cold path instead")
}

// The positive control for the test above: when the pool still holds the exact
// shape the session froze at, adoption proceeds. Without this, the guard above
// would be satisfiable by never adopting at all.
func TestEnsure_AdoptsWhenThePoolStillHoldsTheSessionsFrozenClass(t *testing.T) {
	rt, c := newTestRuntime(t)
	frozen := warmPoolFor(t, rt, "demo-class")

	// A no-op re-reconcile of the same shape must not disturb eligibility.
	require.NoError(t, requirePrewarmer(t, rt).ReconcilePool(t.Context(), poolRequest()))

	sess := newSession("demo-session")
	h, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{Session: sess, Class: frozen})
	require.NoError(t, err)

	assert.True(t, h.Prewarmed, "an unchanged pool must still be adoptable")
	assert.Len(t, listClaims(t, c), 1)
}

// goToolchainFixture and nodeToolchainFixture are two DIFFERENT resolved
// toolchain sets (Task 2's podspec.BuildClassSpec renders an init container
// plus volume/env per mount), used below to prove tryAdopt's eligibility
// check is sensitive to WHICH set a session froze — not just whether the
// class image/resources match. SourceKind "image" is blank-imported above;
// its Apply is pure PodSpec rendering (no exec, no real image pull), so
// these are safe static fixtures.
func goToolchainFixture() v1alpha1.ToolchainMount {
	return v1alpha1.ToolchainMount{
		Name: "go", SourceKind: "image",
		Image: "ap-toolchain-go:dev", Prefix: "/opt/ap-toolchains/go",
		Bin: []string{"bin"}, SizeBytes: 500 * 1024 * 1024,
	}
}

func nodeToolchainFixture() v1alpha1.ToolchainMount {
	return v1alpha1.ToolchainMount{
		Name: "node", SourceKind: "image",
		Image: "ap-toolchain-node:dev", Prefix: "/opt/ap-toolchains/node",
		Bin: []string{"bin"}, SizeBytes: 150 * 1024 * 1024,
	}
}

// Toolchains are now INSIDE the rendered PodSpec both of tryAdopt's
// equalities compare (Task 2/4), so a session whose FROZEN toolchains differ
// from what the pool was built with must not adopt: the warm pods carry the
// pool's overlay, and handing this session one would silently drop a
// compiler it resolved and expects on PATH.
//
// tryAdopt's FIRST equality cannot catch this alone: it renders the class
// shape from THIS session's own frozen toolchains
// (req.Session.Status.ResolvedToolchains), so it always agrees with the
// session's own rendered pod regardless of what the pool holds. It is the
// SECOND equality — templateNameFor(classSpec) against the pool's own
// TemplateRef.Name — that catches the mismatch, because the pool's template
// was hashed from a DIFFERENT toolchain set (see the updated comment block
// above tryAdopt's BuildClassSpec call).
func TestEnsure_DoesNotAdoptWhenTheSessionsFrozenToolchainsDifferFromThePools(t *testing.T) {
	rt, c := newTestRuntime(t)
	pw := requirePrewarmer(t, rt)

	// The pool is built with toolchain set A ("go").
	req := poolRequest()
	req.Toolchains = []v1alpha1.ToolchainMount{goToolchainFixture()}
	require.NoError(t, pw.ReconcilePool(t.Context(), req))

	// This session froze a DIFFERENT toolchain set ("node") at bind time —
	// everything else about the class (image, resources) is identical to what
	// the pool was built with, so toolchains are the ONLY variable this test
	// exercises.
	sess := newSession("demo-session")
	sess.Status.ResolvedToolchains = []v1alpha1.ToolchainMount{nodeToolchainFixture()}

	h, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{Session: sess, Class: req.Class})
	require.NoError(t, err, "a toolchain mismatch is a cold start, not a failure")

	assert.False(t, h.Prewarmed,
		"the pool's warm pods carry toolchain set A; this session froze set B, so "+
			"adopting one would hand it a sandbox missing the compiler it resolved")
	assert.Empty(t, listClaims(t, c),
		"no claim may be created against a pool built for a different toolchain set")
	assert.Len(t, listSandboxes(t, c), 1, "it takes the ordinary cold path instead")
}

// The positive control for the test above: when the pool and the session are
// both built from the SAME toolchain set, adoption proceeds. Without this,
// the guard above would be satisfiable by ReconcilePool silently dropping
// req.Toolchains — e.g. passing nil instead at prewarm.go's BuildClassSpec
// call — since two hashes computed from two DIFFERENT (mismatched) inputs
// would still disagree either way. That regression would mean pre-warming
// had silently stopped firing for every class that declares a toolchain,
// and the negative test alone could never catch it.
func TestEnsure_AdoptsWhenTheSessionsFrozenToolchainsMatchThePools(t *testing.T) {
	rt, c := newTestRuntime(t)
	pw := requirePrewarmer(t, rt)

	// The pool and the session are both built from toolchain set A ("go").
	req := poolRequest()
	req.Toolchains = []v1alpha1.ToolchainMount{goToolchainFixture()}
	require.NoError(t, pw.ReconcilePool(t.Context(), req))

	sess := newSession("demo-session")
	sess.Status.ResolvedToolchains = []v1alpha1.ToolchainMount{goToolchainFixture()}

	h, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{Session: sess, Class: req.Class})
	require.NoError(t, err)

	assert.True(t, h.Prewarmed, "matching toolchain sets must still adopt")
	assert.Len(t, listClaims(t, c), 1, "exactly one claim")
}

// A cluster can install the base agent-sandbox CRD without the extensions
// bundle (SandboxClaim / SandboxWarmPool / SandboxTemplate ship separately).
// This backend must keep working there — cold — and must never ADDRESS a kind
// the cluster cannot serve: through the operator's cached client that starts
// an informer which can never sync.
func TestEnsure_WithoutThePrewarmCRDsStaysCold(t *testing.T) {
	rt, c := newRuntimeWithoutPrewarmCRDs(t)

	sess := newSession("demo-session")
	h, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{Session: sess, Class: testClass()})
	require.NoError(t, err,
		"an absent prewarming CRD must not fail the session — and the interceptor makes "+
			"any attempt to address one a hard error, so this also proves none was made")

	assert.False(t, h.Prewarmed)
	assert.Len(t, listSandboxes(t, c), 1, "the cold path must still create the Sandbox")
}

// refuseExtensionsGroup makes every access to an extensions.agents.x-k8s.io
// object fail the way a cluster without those CRDs would.
//
// Without it the test would be VACUOUS: the fake client answers a Get from its
// object tracker, so it returns a perfectly ordinary NotFound for a kind the
// cluster could not serve at all — indistinguishable from "no pool exists".
// The error here is deliberately NOT a NotFound, so a runtime that addressed
// the kind anyway surfaces as an Ensure failure rather than a silent cold start.
func refuseExtensionsGroup() interceptor.Funcs {
	refuse := func(obj client.Object) error {
		switch obj.(type) {
		case *sandboxextv1beta1.SandboxClaim,
			*sandboxextv1beta1.SandboxWarmPool,
			*sandboxextv1beta1.SandboxTemplate:
			return fmt.Errorf("no matches for kind %T: the extensions.agents.x-k8s.io CRDs are not installed", obj)
		}
		return nil
	}
	return interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := refuse(obj); err != nil {
				return err
			}
			return cl.Get(ctx, key, obj, opts...)
		},
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := refuse(obj); err != nil {
				return err
			}
			return cl.Create(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := refuse(obj); err != nil {
				return err
			}
			return cl.Delete(ctx, obj, opts...)
		},
	}
}

// baseOnlyMapper resolves ONLY agents.x-k8s.io/Sandbox — a cluster with the
// base agent-sandbox CRD and no extensions bundle.
func baseOnlyMapper(t *testing.T) meta.RESTMapper {
	t.Helper()
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{sandboxv1beta1.GroupVersion})
	m.Add(sandboxv1beta1.GroupVersion.WithKind("Sandbox"), meta.RESTScopeNamespace)
	return m
}

// Teardown must reclaim what Ensure created. For an adopted session that is
// the claim: the Sandbox behind it is controller-owned BY the claim, so
// ordinary cascade GC takes it (and its pod) along.
func TestTeardown_DeletesTheClaimForAnAdoptedSession(t *testing.T) {
	rt, c := newTestRuntime(t)
	sess, h := adoptSession(t, rt)

	key := types.NamespacedName{Namespace: sess.Namespace, Name: podspec.PodNameFor(sess)}
	var claim sandboxextv1beta1.SandboxClaim
	// Prove the claim is really there first: asserting only that it is absent
	// afterwards cannot distinguish a working Teardown from one that never
	// created a claim at all.
	require.NoError(t, c.Get(t.Context(), key, &claim), "precondition: Ensure created the claim")

	require.NoError(t, rt.Teardown(t.Context(), h))

	err := c.Get(t.Context(), key, &claim)
	assert.True(t, apierrors.IsNotFound(err), "Teardown must delete the claim")

	// Teardown runs from a finalizer that re-enters until it succeeds; an
	// already-absent claim must be success or the session can never be deleted.
	require.NoError(t, rt.Teardown(t.Context(), h))
}

// adoptedPodName is the pod the agent-sandbox controller records on an adopted
// Sandbox. Pool-generated, like the Sandbox's own name.
const adoptedPodName = "demo-class-pool-x7k29-0"

// createAdoptedPod materializes the pod behind an adopted sandbox with the
// given labels, standing in for the agent-sandbox controller's own patch of
// the already-running pooled pod.
func createAdoptedPod(t *testing.T, c client.Client, name string, labels map[string]string) {
	t.Helper()
	require.NoError(t, c.Create(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, Labels: labels},
	}))
}

// completeAdoption drives an adopted session all the way to the state a
// HEALTHY cluster reaches: the claim bound, the pool's Sandbox Ready, its pod
// recorded, and the session labels the claim asked for actually patched onto
// that pod.
func completeAdoption(t *testing.T, c client.Client, sess *v1alpha1.SpiceboxSession) {
	t.Helper()
	bindClaim(t, c, sess, adoptedSandboxName)
	createAdoptedSandbox(t, c, adoptedSandboxName,
		[]metav1.Condition{cond("Ready", metav1.ConditionTrue, "DependenciesReady")}, adoptedPodName)
	createAdoptedPod(t, c, adoptedPodName, map[string]string{
		sessionLabelKey:                     sess.Name,
		"agentprimitives.authzed.com/class": sess.Spec.Class,
	})
}

// Status resolves through the claim to the pool-generated Sandbox behind it,
// then applies exactly the same condition mapping a cold sandbox gets.
func TestStatus_AdoptedSessionResolvesThroughTheClaim(t *testing.T) {
	rt, c := newTestRuntime(t)
	sess, h := adoptSession(t, rt)
	completeAdoption(t, c, sess)

	got, err := rt.Status(t.Context(), h)
	require.NoError(t, err)
	assert.Equal(t, sandboxkinds.PhaseReady, got.Phase,
		"a bound claim must report the adopted Sandbox's own phase")
	assert.False(t, got.RequiresPolling, "a fully-adopted, Ready sandbox has nothing to poll for")
}

// SECURITY, and a TIMING one. Adoption patches the CLAIM's spec; the
// agent-sandbox controller then patches the ALREADY-RUNNING pooled pod on its
// own queue, later. The pooled Sandbox meanwhile carries Ready=True from before
// it was ever adopted, so a Status that read Ready alone would report
// PhaseReady — and the ToolCall path would dispatch an exec — in the window
// BETWEEN the two.
//
// In that window AP's per-session NetworkPolicy selects nothing (its selector
// is the label that has not landed) and the template's deliberate
// NetworkPolicyManagement: Unmanaged means upstream created nothing either. The
// pod is subject to NO NetworkPolicy at all, including for a session configured
// networkMode: none. Each case below is one shape of that window.
func TestStatus_AdoptedSessionIsNotReadyUntilTheSessionLabelReachesThePod(t *testing.T) {
	cases := []struct {
		name string
		// setup drives the cluster into one shape of the window, AFTER the
		// claim is bound and the pool's Sandbox is Ready.
		setup func(t *testing.T, c client.Client, sess *v1alpha1.SpiceboxSession)
	}{
		{
			name: "the controller has not recorded the pod yet: Pending, not Ready",
			setup: func(t *testing.T, c client.Client, sess *v1alpha1.SpiceboxSession) {
				createAdoptedSandbox(t, c, adoptedSandboxName,
					[]metav1.Condition{cond("Ready", metav1.ConditionTrue, "DependenciesReady")}, "")
			},
		},
		{
			name: "the pod is named but not visible yet: Pending, not Ready",
			setup: func(t *testing.T, c client.Client, sess *v1alpha1.SpiceboxSession) {
				createAdoptedSandbox(t, c, adoptedSandboxName,
					[]metav1.Condition{cond("Ready", metav1.ConditionTrue, "DependenciesReady")}, adoptedPodName)
			},
		},
		{
			name: "the pod exists but still carries only the warm pool's labels: Pending, not Ready",
			setup: func(t *testing.T, c client.Client, sess *v1alpha1.SpiceboxSession) {
				createAdoptedSandbox(t, c, adoptedSandboxName,
					[]metav1.Condition{cond("Ready", metav1.ConditionTrue, "DependenciesReady")}, adoptedPodName)
				// A pool pod's own labels, with no session label: BuildClassSpec
				// returns a bare PodSpec, so this is exactly what a warm pod
				// carries until the claim's injection lands.
				createAdoptedPod(t, c, adoptedPodName, map[string]string{"pool": "demo-class"})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, c := newTestRuntime(t)
			sess, h := adoptSession(t, rt)
			bindClaim(t, c, sess, adoptedSandboxName)
			tc.setup(t, c, sess)

			got, err := rt.Status(t.Context(), h)
			require.NoError(t, err, "an unlanded label is a transient state, not an error")
			assert.Equal(t, sandboxkinds.PhasePending, got.Phase,
				"reporting Ready here would let a tool call be dispatched into a pod NO "+
					"NetworkPolicy selects — strictly less isolated than a cold sandbox")
			assert.Equal(t, agentsandbox.ReasonAwaitingSessionLabels, got.Reason)
			assert.NotEmpty(t, got.Message, "the operator must be told what is being waited on")
			// THE POSITIVE HALF of Status.RequiresPolling, and the ONLY state in
			// this backend that sets it. Nothing this backend watches can end it:
			// the pod is owned by the Sandbox and an adopted Sandbox is owned by
			// the claim, so neither maps back to the SpiceboxSession, and the
			// session-owned claim already fired before the pod was patched.
			// Without this flag the session controller has no reason to look
			// again and the session stalls until the manager's ~10h resync.
			assert.True(t, got.RequiresPolling,
				"no watch of this backend's can end this state, so it MUST ask to be polled")
		})
	}
}

// CONVERGENCE. The Pending above must be a WINDOW, not a wedge: the SAME
// handle, with no re-Ensure and no new claim, must report Ready the moment the
// base Sandbox controller patches the session labels onto the adopted pod.
// Asserting Pending alone would be satisfied by a backend that reported Pending
// forever — which is the failure mode, not the fix. (The other half of
// liveness, something that actually re-drives Status, is the SpiceboxSession
// controller's bounded requeue; see
// pkg/controllers/spiceboxsession's TestReconcile_PendingSandboxConvergesOnTheRequeuedPass.)
func TestStatus_AdoptedSessionBecomesReadyOnceTheLabelLands(t *testing.T) {
	rt, c := newTestRuntime(t)
	sess, h := adoptSession(t, rt)
	bindClaim(t, c, sess, adoptedSandboxName)
	createAdoptedSandbox(t, c, adoptedSandboxName,
		[]metav1.Condition{cond("Ready", metav1.ConditionTrue, "DependenciesReady")}, adoptedPodName)
	// The pooled pod as it exists at adoption: running, Ready, and carrying
	// none of this session's metadata yet.
	createAdoptedPod(t, c, adoptedPodName, map[string]string{"pool": "demo-class"})

	before, err := rt.Status(t.Context(), h)
	require.NoError(t, err)
	require.Equal(t, sandboxkinds.PhasePending, before.Phase, "precondition: inside the window")
	require.True(t, before.RequiresPolling, "precondition: the window asks to be polled")

	// The base Sandbox controller patches the live pod, on its own queue.
	var pod corev1.Pod
	require.NoError(t, c.Get(t.Context(),
		types.NamespacedName{Namespace: "default", Name: adoptedPodName}, &pod))
	pod.Labels[sessionLabelKey] = sess.Name
	pod.Labels["agentprimitives.authzed.com/class"] = sess.Spec.Class
	require.NoError(t, c.Update(t.Context(), &pod))

	after, err := rt.Status(t.Context(), h)
	require.NoError(t, err)
	assert.Equal(t, sandboxkinds.PhaseReady, after.Phase,
		"the same handle must go Ready once the labels land — the refusal is a window, not a wedge")
	assert.False(t, after.RequiresPolling,
		"the poll must stop with the state that needed it, or the session polls forever")
}

// The cold path must be untouched by the check above: a directly-created
// Sandbox's pod is built from podspec.Build, which stamps the session label
// into the pod template itself, so there is no window and no claim to consult.
func TestStatus_ColdSandboxIsReadyWithoutAnyPodLookup(t *testing.T) {
	rt, c := newTestRuntime(t)
	sess := newSession("demo-session")
	h, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{Session: sess, Class: testClass()})
	require.NoError(t, err)
	require.False(t, h.Prewarmed, "precondition: this is the cold path")

	sb := getSandbox(t, c, sess)
	sb.Status.Conditions = []metav1.Condition{cond("Ready", metav1.ConditionTrue, "DependenciesReady")}
	require.NoError(t, c.Status().Update(t.Context(), sb))

	got, err := rt.Status(t.Context(), h)
	require.NoError(t, err)
	assert.Equal(t, sandboxkinds.PhaseReady, got.Phase,
		"no pod exists here at all; a cold sandbox must not be gated on one")
	assert.False(t, got.RequiresPolling, "the cold path never asks to be polled")
}

// A claim that has not been given a sandbox yet is Pending, never Gone and
// never an error: the claim exists and the agent-sandbox controller is still
// working on it. Reporting Gone would make the session controller treat a
// perfectly healthy, still-provisioning sandbox as dead.
func TestStatus_UnboundClaimIsPendingNotGone(t *testing.T) {
	rt, _ := newTestRuntime(t)
	_, h := adoptSession(t, rt)

	got, err := rt.Status(t.Context(), h)
	require.NoError(t, err, "an unbound claim is a normal transient state, not an error")
	assert.Equal(t, sandboxkinds.PhasePending, got.Phase)
	assert.NotEmpty(t, got.Message, "the message must say what is being waited on")
	assert.False(t, got.RequiresPolling,
		"the claim is session-owned, so Owns(&SandboxClaim{}) fires when it binds — "+
			"this Pending is watch-covered and must not add a timer")
}

// An upstream refusal (a rejected additionalPodMetadata label domain is the
// one AP can actually trip — see the claim-label test above) otherwise reaches
// the operator as an indefinite, reasonless Pending: a silent hang, which is
// the exact failure shape this repo's no-silent-errors rule exists to prevent.
func TestStatus_UnboundClaimSurfacesTheClaimsOwnRefusal(t *testing.T) {
	rt, c := newTestRuntime(t)
	sess, h := adoptSession(t, rt)

	claim := getClaim(t, c, sess)
	claim.Status.Conditions = []metav1.Condition{
		cond("Ready", metav1.ConditionFalse, "InvalidMetadata"),
	}
	claim.Status.Conditions[0].Message = "invalid additionalPodMetadata: label domain not in the allowlist"
	require.NoError(t, c.Status().Update(t.Context(), claim))

	got, err := rt.Status(t.Context(), h)
	require.NoError(t, err)
	assert.Equal(t, sandboxkinds.PhasePending, got.Phase)
	assert.Equal(t, "InvalidMetadata", got.Reason,
		"the claim's own reason is more diagnostic than a generic Creating")
	assert.Contains(t, got.Message, "allowlist",
		"the claim's own message must reach the operator verbatim")
	assert.False(t, got.RequiresPolling,
		"a refused claim is resolved by the next Ensure discarding it, not by a timer here")
}

// A claim that no longer exists is Gone, not an error: a completed or reaped
// session re-enters Status on every reconcile and must not error each time.
//
// The message assertion is what makes this test about the CLAIM. Phase and the
// nil error alone do not discriminate: strip the whole Prewarmed branch out of
// Status and the Sandbox lookup at the same name also NotFounds, also returns
// PhaseGone, also without error — the test would pass while proving nothing
// about the indirection it is named for.
func TestStatus_AbsentClaimIsGone(t *testing.T) {
	rt, _ := newTestRuntime(t)
	got, err := rt.Status(t.Context(), sandboxkinds.Handle{
		Kind: agentsandbox.KindName, Ref: "default/never-claimed", Prewarmed: true})
	require.NoError(t, err)
	assert.Equal(t, sandboxkinds.PhaseGone, got.Phase)
	assert.Contains(t, got.Message, "claim",
		"a Prewarmed handle must be resolved through the claim, so the message must "+
			"name the claim and not the Sandbox that never existed under that name")
	assert.False(t, got.RequiresPolling, "a terminal state has nothing to poll for")
}

// Executor on an unbound claim must REFUSE, fail-closed, with no executor.
// Binding to an unresolved target would dispatch a tool call at nothing and
// fail somewhere far from the real cause.
//
// As with TestStatus_AbsentClaimIsGone, the message assertion is what ties
// this to the claim: strip the Prewarmed branch and Executor's Sandbox Get
// NotFounds, returning the same (nil, error) pair for an entirely different
// reason.
func TestExecutor_UnboundClaimRefusesWithNoExecutor(t *testing.T) {
	rt, _ := newTestRuntime(t)
	_, h := adoptSession(t, rt)

	ex, err := rt.Executor(h)
	require.Error(t, err, "a claim with no sandbox yet has no exec target")
	assert.Nil(t, ex, "a backend that refuses must not also return an executor")
	assert.Contains(t, err.Error(), "claim",
		"the refusal must name the CLAIM that has not bound — the reason a caller "+
			"can act on — not a Sandbox lookup that missed")
}

func TestExecutor_AdoptedSessionResolvesThroughTheClaim(t *testing.T) {
	rt, c := newTestRuntime(t)
	sess, h := adoptSession(t, rt)
	bindClaim(t, c, sess, adoptedSandboxName)
	createAdoptedSandbox(t, c, adoptedSandboxName, nil, "demo-class-pool-x7k29")

	ex, err := rt.Executor(h)
	require.NoError(t, err)
	assert.NotNil(t, ex)
}

// An adopted session's Sandbox is controller-owned by the CLAIM (upstream
// clears the warm pool's ownerRef and sets its own), so the session
// controller's Owns(&Sandbox{}) can never map it back to the SpiceboxSession.
// The claim is session-owned and does map back — without this entry an adopted
// session waits for the manager's 10-hour resync, because the reconcile
// returns no RequeueAfter.
func TestWatches_ContributesTheClaimWatchWhenPrewarmingIsAvailable(t *testing.T) {
	rt, _ := newTestRuntime(t)
	w := rt.Watches()
	require.Len(t, w, 2)
	assert.IsType(t, &sandboxv1beta1.Sandbox{}, w[0].Object)
	assert.IsType(t, &sandboxextv1beta1.SandboxClaim{}, w[1].Object,
		"an adopted session converges only through its claim; Owns(&Sandbox{}) "+
			"cannot map a claim-owned Sandbox back to the session")
}

// …and it must be ABSENT without the CRDs. Registering an informer for a kind
// the cluster cannot serve wedges the manager at "failed to wait for caches to
// sync", taking every other controller in the operator down with it — a far
// worse outcome than a backend that merely never pre-warms.
func TestWatches_OmitsTheClaimWatchWithoutThePrewarmCRDs(t *testing.T) {
	rt, _ := newRuntimeWithoutPrewarmCRDs(t)
	w := rt.Watches()
	require.Len(t, w, 1, "only the base Sandbox watch may be registered")
	assert.IsType(t, &sandboxv1beta1.Sandbox{}, w[0].Object)
}

// --- Degrade to cold when the cluster refuses AP's claims -------------------
//
// The agent-sandbox controller validates a claim's additionalPodMetadata label
// keys against an allowlist of DOMAINS, defaulting to "sandbox.users.io" alone
// and read from a file on ITS pod. AP stamps agentprimitives.authzed.com/… so
// its per-session NetworkPolicy can select the sandbox pod, and AP is a
// bring-your-own consumer that cannot set that allowlist. On a stock install
// every AP claim is therefore refused — so pre-warming has to degrade to a
// cold start, never wedge the session over an optimisation it does not need.

// refuseClaim stamps the claim with the Ready=False condition upstream sets
// when validateAdditionalPodMetadata rejects it. Reason is the machine string
// computeReadyCondition emits on its ErrInvalidMetadata arm; the message is
// upstream's own prose, included so the assertions prove it is relayed rather
// than re-invented.
func refuseClaim(t *testing.T, c client.Client, sess *v1alpha1.SpiceboxSession) {
	t.Helper()
	claim := getClaim(t, c, sess)
	claim.Status.Conditions = []metav1.Condition{
		cond("Ready", metav1.ConditionFalse, "InvalidMetadata"),
	}
	claim.Status.Conditions[0].Message =
		`invalid additionalPodMetadata: label domain "agentprimitives.authzed.com" is not in the allowlist`
	require.NoError(t, c.Status().Update(t.Context(), claim))
}

// A refused claim is discarded and the session gets an ordinary cold sandbox.
// Pre-warming must DEGRADE, never break: the refusal is a cluster
// misconfiguration AP cannot fix from inside, and leaving the claim in place
// would wedge the session at Pending forever.
func TestEnsure_RefusedClaimIsDiscardedAndTheSessionGoesCold(t *testing.T) {
	rt, c := newTestRuntime(t)
	class := warmPoolFor(t, rt, "demo-class")
	sess := newSession("demo-session")
	req := sandboxkinds.EnsureRequest{Session: sess, Class: class}

	first, err := rt.Ensure(t.Context(), req)
	require.NoError(t, err)
	require.True(t, first.Prewarmed, "precondition: the first pass adopted")
	refuseClaim(t, c, sess)

	second, err := rt.Ensure(t.Context(), req)
	require.NoError(t, err, "a refused claim must not fail the session")

	assert.False(t, second.Prewarmed,
		"the handle must report the COLD sandbox it now names; a stale Prewarmed:true "+
			"would send Status and Teardown at a claim that no longer exists")
	assert.Empty(t, listClaims(t, c), "the refused claim must be deleted, not left to wedge")
	require.Len(t, listSandboxes(t, c), 1, "the session must get an ordinary cold sandbox")
}

// The loop guard. The session is STILL eligible and the pool STILL exists, so
// a pass that discarded a refused claim and then fell into tryAdopt anyway
// would re-create the very claim upstream just rejected — create, refuse,
// delete, forever, one monitoring warning per turn of the wheel. The direct
// Sandbox created on the degrading pass is what makes the decision stick.
func TestEnsure_AfterDegradingToColdItStaysCold(t *testing.T) {
	rt, c := newTestRuntime(t)
	class := warmPoolFor(t, rt, "demo-class")
	sess := newSession("demo-session")
	req := sandboxkinds.EnsureRequest{Session: sess, Class: class}

	_, err := rt.Ensure(t.Context(), req)
	require.NoError(t, err)
	refuseClaim(t, c, sess)
	_, err = rt.Ensure(t.Context(), req) // the degrading pass
	require.NoError(t, err)
	require.Empty(t, listClaims(t, c), "precondition: the refused claim was discarded")

	for i := 0; i < 3; i++ {
		h, err := rt.Ensure(t.Context(), req)
		require.NoError(t, err)
		assert.False(t, h.Prewarmed, "pass %d must stay cold", i)
		assert.Empty(t, listClaims(t, c),
			"pass %d re-created a claim: adoption must not be retried for a session that "+
				"already fell back, or the cluster refuses it again on every reconcile", i)
		assert.Len(t, listSandboxes(t, c), 1, "pass %d must not create a second sandbox", i)
	}
}

// The refusal is a CLUSTER-OPERATOR problem — the fix is a flag on a
// controller AP does not install — so it goes to the monitoring channel, not
// to the session-participant notice path whose audiences are
// requester/approvers/participants.
//
// Cardinality is the point of the loop below: emitting per reconcile is a
// defect class this repo has hit before. One event per session that attempts
// adoption, and no more.
func TestEnsure_RefusedClaimEmitsExactlyOneMonitoringWarning(t *testing.T) {
	rt, c, mon := newTestRuntimeWithMonitoring(t)
	class := warmPoolFor(t, rt, "demo-class")
	sess := newSession("demo-session")
	req := sandboxkinds.EnsureRequest{Session: sess, Class: class}

	_, err := rt.Ensure(t.Context(), req)
	require.NoError(t, err)
	require.Empty(t, mon.all(), "adoption itself is not a monitoring event")
	refuseClaim(t, c, sess)

	_, err = rt.Ensure(t.Context(), req)
	require.NoError(t, err)

	events := mon.all()
	require.Len(t, events, 1, "exactly one warning on the transition")
	ev := events[0]
	assert.Equal(t, channelevents.MonitoringLevelWarning, ev.Level,
		"warning, not error: the session still works, just cold")
	assert.Equal(t, channelevents.MonitoringTransitionFailed, ev.Transition)
	assert.Equal(t, "SpiceboxSession", ev.Source.Kind)
	assert.Equal(t, sess.Name, ev.Source.Name)
	assert.Equal(t, sess.Namespace, ev.Source.Namespace)
	assert.Equal(t, "InvalidMetadata", ev.Reason,
		"the claim's own machine reason, not prose parsed out of its message")
	// The whole point of the warning: the operator must be able to learn what
	// to change. A warning that only says "pre-warming failed" is noise.
	assert.Contains(t, ev.Hint, "agentprimitives.authzed.com",
		"the hint must name the label domain the operator has to allow")
	assert.Contains(t, ev.Hint, "allowed", "the hint must name the allowlist, not just the symptom")
	assert.Contains(t, ev.Summary, "cold", "the summary must say what the session did instead")

	for i := 0; i < 3; i++ {
		_, err := rt.Ensure(t.Context(), req)
		require.NoError(t, err)
	}
	assert.Len(t, mon.all(), 1,
		"still exactly one: the warning fires on the TRANSITION that discards the claim, "+
			"never on every reconcile")
}

// Monitoring is how an operator LEARNS of a degradation, never how the backend
// decides to degrade. An unconfigured bus leaves a true nil PublishFunc, and
// the fallback must be identical — this is the shape every other test in this
// file runs in, since newTestRuntime supplies no publisher at all.
func TestEnsure_RefusedClaimDegradesWithNoMonitoringConfigured(t *testing.T) {
	rt, c := newTestRuntime(t) // deliberately no MonitoringPublish
	class := warmPoolFor(t, rt, "demo-class")
	sess := newSession("demo-session")
	req := sandboxkinds.EnsureRequest{Session: sess, Class: class}

	_, err := rt.Ensure(t.Context(), req)
	require.NoError(t, err)
	refuseClaim(t, c, sess)

	h, err := rt.Ensure(t.Context(), req)
	require.NoError(t, err, "a nil publisher must not panic and must not fail the session")
	assert.False(t, h.Prewarmed)
	assert.Len(t, listSandboxes(t, c), 1, "the degradation happens regardless of monitoring")
}

// A claim that is merely NOT READY YET must be waited on, never discarded. Only
// a REFUSAL — upstream rejecting the claim's own spec, which resubmitting
// identical bytes can never fix — earns the cold fallback. Discarding on any
// Ready=False would throw away every claim during its normal provisioning
// window and make pre-warming strictly slower than not pre-warming.
func TestEnsure_TransientlyNotReadyClaimIsKeptNotDiscarded(t *testing.T) {
	rt, c := newTestRuntime(t)
	class := warmPoolFor(t, rt, "demo-class")
	sess := newSession("demo-session")
	req := sandboxkinds.EnsureRequest{Session: sess, Class: class}

	_, err := rt.Ensure(t.Context(), req)
	require.NoError(t, err)

	claim := getClaim(t, c, sess)
	claim.Status.Conditions = []metav1.Condition{cond("Ready", metav1.ConditionFalse, "Provisioning")}
	require.NoError(t, c.Status().Update(t.Context(), claim))

	h, err := rt.Ensure(t.Context(), req)
	require.NoError(t, err)
	assert.True(t, h.Prewarmed, "a provisioning claim is still this session's sandbox")
	assert.Len(t, listClaims(t, c), 1, "it must NOT be discarded")
	assert.Empty(t, listSandboxes(t, c), "and no cold sandbox may be created alongside it")
}

// tryAdopt Gets a SandboxWarmPool before it creates a claim. A cluster serving
// SandboxClaim but not SandboxWarmPool would answer that Get with a NoKindMatch
// — which is NOT IsNotFound, so the cold-path guard there misses it — and
// Ensure would fail for every eligible session. Availability is one bit, so it
// must be false unless EVERY kind this backend addresses is servable.
func TestEnsure_WithoutTheWarmPoolCRDStaysCold(t *testing.T) {
	s := newScheme(t)
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		sandboxv1beta1.GroupVersion, sandboxextv1beta1.GroupVersion,
	})
	m.Add(sandboxv1beta1.GroupVersion.WithKind("Sandbox"), meta.RESTScopeNamespace)
	// SandboxClaim only — no SandboxWarmPool mapping.
	m.Add(sandboxextv1beta1.GroupVersion.WithKind("SandboxClaim"), meta.RESTScopeNamespace)

	c := fake.NewClientBuilder().WithScheme(s).WithRESTMapper(m).
		WithStatusSubresource(&sandboxv1beta1.Sandbox{}).
		WithInterceptorFuncs(refuseExtensionsGroup()).Build()
	rt, err := agentsandbox.Kind{}.NewRuntime(sandboxkinds.Deps{Client: c, ExecFor: execfake.New().For})
	require.NoError(t, err)

	h, err := rt.Ensure(t.Context(), sandboxkinds.EnsureRequest{
		Session: newSession("demo-session"), Class: testClass()})
	require.NoError(t, err,
		"a half-installed extensions bundle must not fail the session — the interceptor "+
			"makes any attempt to address either kind a hard error, so this proves none was made")
	assert.False(t, h.Prewarmed)
	assert.Len(t, listSandboxes(t, c), 1)
	assert.Len(t, rt.Watches(), 1, "and no claim informer may be registered either")
}

// If the extensions CRDs are uninstalled while adopted sessions are still
// live, the claim Delete answers NoKindMatch — NOT IsNotFound. Returning it
// would fail this finalizer on every pass and leave those sessions PERMANENTLY
// undeletable, needing manual finalizer surgery. Uninstalling the CRD already
// removed every claim it stored, so absence by uninstall and absence by
// deletion are the same outcome: success.
func TestTeardown_ToleratesTheExtensionsCRDsBeingUninstalled(t *testing.T) {
	s := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithRESTMapper(mapperWithSandbox(t, s)).
		WithStatusSubresource(&sandboxv1beta1.Sandbox{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*sandboxextv1beta1.SandboxClaim); ok {
					return &meta.NoKindMatchError{
						GroupKind:        schema.GroupKind{Group: "extensions.agents.x-k8s.io", Kind: "SandboxClaim"},
						SearchedVersions: []string{"v1beta1"},
					}
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()
	rt, err := agentsandbox.Kind{}.NewRuntime(sandboxkinds.Deps{Client: c, ExecFor: execfake.New().For})
	require.NoError(t, err)

	err = rt.Teardown(t.Context(), sandboxkinds.Handle{
		Kind: agentsandbox.KindName, Ref: "default/demo-session-pod", Prewarmed: true})
	require.NoError(t, err,
		"an uninstalled CRD must not make the session undeletable: Teardown runs from a "+
			"finalizer that re-enters until it succeeds")
}
