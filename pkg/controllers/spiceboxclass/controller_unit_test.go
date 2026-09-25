package spiceboxclass_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	_ "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod" // register the built-in sandbox backend so spiceboxclass.Reconciler resolves it
	sandboxregistry "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/registry"
)

// fakePrewarmControllerKind is a minimal sandboxkinds.Kind registered once
// (below) under a made-up name distinct from validate_test.go's own fake kind
// — the sandboxkinds registry is process-global and panics on a duplicate
// name, and that file's fakeWarmKind is unexported in a different Go package
// (spiceboxclass, not spiceboxclass_test), so it is not reachable here.
type fakePrewarmControllerKind struct{ name string }

func (k fakePrewarmControllerKind) Name() string { return k.name }

// Supports reports FeatureToolchainOverlay true and everything else false —
// the toolchain-resolution tests below set spec.toolchains on this kind, and
// ValidateClassAgainstKind rejects that unless the resolved kind supports the
// capability (pkg/tools/sandboxkinds/validate.go). No existing test in this file
// sets spec.mounts or otherwise depends on this kind rejecting a different
// feature, so widening only this one bit is safe.
func (k fakePrewarmControllerKind) Supports(f sandboxkinds.Feature) bool {
	return f == sandboxkinds.FeatureToolchainOverlay
}
func (k fakePrewarmControllerKind) WorkspaceDomain() string { return "" }
func (k fakePrewarmControllerKind) ValidateClass(spiceboxv1alpha1.SpiceboxClassSpec) error {
	return nil
}
func (k fakePrewarmControllerKind) NewRuntime(sandboxkinds.Deps) (sandboxkinds.Runtime, error) {
	panic("fakePrewarmControllerKind.NewRuntime: the SpiceboxClass controller never constructs a Runtime itself")
}

var _ sandboxkinds.Kind = fakePrewarmControllerKind{}

const fakePrewarmControllerKindName = "fake-controller-prewarm-kind"

// errBoom is a sentinel a fake Runtime returns to prove Reconcile propagates
// a ReconcilePool failure rather than swallowing it.
var errBoom = errors.New("boom")

func init() {
	sandboxregistry.Register(fakePrewarmControllerKind{name: fakePrewarmControllerKindName})
}

// sweepCall records one SweepOrphanedPools invocation.
type sweepCall struct {
	className         string
	classUID          types.UID
	desiredNamespaces []string
}

// recordingPrewarmRuntime satisfies sandboxkinds.Runtime and
// sandboxkinds.Prewarmer, recording every ReconcilePool/SweepOrphanedPools
// call so a test can assert what the controller actually sent — the
// PoolRequest fields and the desired-namespace set, not just "no error came
// back." Every other Runtime method panics: none of them is on the
// SpiceboxClass controller's call path, so a test that reaches one by
// mistake fails loudly instead of returning a meaningless zero value.
type recordingPrewarmRuntime struct {
	mu               sync.Mutex
	poolCalls        []sandboxkinds.PoolRequest
	sweepCalls       []sweepCall
	reconcilePoolErr error
	sweepErr         error
}

func (r *recordingPrewarmRuntime) Ensure(context.Context, sandboxkinds.EnsureRequest) (sandboxkinds.Handle, error) {
	panic("recordingPrewarmRuntime.Ensure: not exercised by the SpiceboxClass controller")
}
func (r *recordingPrewarmRuntime) Status(context.Context, sandboxkinds.Handle) (sandboxkinds.Status, error) {
	panic("recordingPrewarmRuntime.Status: not exercised by the SpiceboxClass controller")
}
func (r *recordingPrewarmRuntime) Teardown(context.Context, sandboxkinds.Handle) error {
	panic("recordingPrewarmRuntime.Teardown: not exercised by the SpiceboxClass controller")
}
func (r *recordingPrewarmRuntime) Executor(sandboxkinds.Handle) (exec.Executor, error) {
	panic("recordingPrewarmRuntime.Executor: not exercised by the SpiceboxClass controller")
}
func (r *recordingPrewarmRuntime) Watches() []sandboxkinds.Watch { return nil }

func (r *recordingPrewarmRuntime) ReconcilePool(_ context.Context, req sandboxkinds.PoolRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.poolCalls = append(r.poolCalls, req)
	return r.reconcilePoolErr
}

func (r *recordingPrewarmRuntime) SweepOrphanedPools(_ context.Context, className string, classUID types.UID, desiredNamespaces []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepCalls = append(r.sweepCalls, sweepCall{className: className, classUID: classUID, desiredNamespaces: desiredNamespaces})
	return r.sweepErr
}

func (r *recordingPrewarmRuntime) calls() []sandboxkinds.PoolRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sandboxkinds.PoolRequest(nil), r.poolCalls...)
}

func (r *recordingPrewarmRuntime) sweeps() []sweepCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sweepCall(nil), r.sweepCalls...)
}

var (
	_ sandboxkinds.Runtime   = (*recordingPrewarmRuntime)(nil)
	_ sandboxkinds.Prewarmer = (*recordingPrewarmRuntime)(nil)
)

// goodUnitResources returns a SpiceboxResources block that satisfies
// validateSpec's CPU/Memory/EphemeralStorage gates.
func goodUnitResources() spiceboxv1alpha1.SpiceboxResources {
	return spiceboxv1alpha1.SpiceboxResources{
		CPU:              resource.MustParse("100m"),
		Memory:           resource.MustParse("128Mi"),
		EphemeralStorage: resource.MustParse("100Mi"),
	}
}

// newReconcilerWithObjects returns a Reconciler wired against a fake
// client seeded with the given objects. Status subresource is enabled
// for SpiceboxClass so the controller's Status().Update() lands. The
// Registry is seeded with builtins so sensitiveEnvNames derives the real
// reserved set (GITHUB_TOKEN, ANTHROPIC_API_KEY) for EnvDefaults checks.
func newReconcilerWithObjects(t *testing.T, objs ...client.Object) (*spiceboxclass.Reconciler, client.Client) {
	t.Helper()
	scheme := testfixtures.NewScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.SpiceboxClass{}).
		Build()
	reg, err := registry.NewWithBuiltins(nil)
	require.NoError(t, err, "registry.NewWithBuiltins")
	return &spiceboxclass.Reconciler{Client: c, Scheme: scheme, Registry: reg}, c
}

// reconcileAndGet runs Reconcile against the given class name and
// returns the resulting object. Fails the test on any error.
func reconcileAndGet(t *testing.T, r *spiceboxclass.Reconciler, c client.Client, name string) *spiceboxv1alpha1.SpiceboxClass {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	require.NoError(t, err, "Reconcile %s", name)
	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: name}, &got),
		"Get %s after Reconcile", name)
	return &got
}

// TestReconcile_InvalidEnvDefaultsWithCoveredToolspec is the regression
// test for the order-sensitive condition bug: a SpiceboxClass whose
// EnvDefaults shadows an auth-injected env var (here, GITHUB_TOKEN) but
// whose Toolspecs cover every Tool must NOT have its Valid=False
// (InvalidEnvDefaults) condition overwritten by the coverage block.
//
// Pre-fix: the reconciler stamped Valid=False/InvalidEnvDefaults then
// the coverage block unconditionally stamped Valid=True/Resolved.
// Post-fix: the coverage block is gated on validateSpec succeeding, so
// Valid=False/InvalidEnvDefaults survives.
func TestReconcile_InvalidEnvDefaultsWithCoveredToolspec(t *testing.T) {
	// A Valid Toolspec covering the "echo" tool — necessary to drive
	// the reconciler into the coverage block that used to overwrite.
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "echo-default"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name:    "echo-default",
			Version: "1",
			Toolkit: spiceboxv1alpha1.ToolspecToolkitRef{
				Name:     "echo",
				Revision: "2026-04-25",
			},
			AllowSubcommands: []string{""},
		},
		Status: spiceboxv1alpha1.SpiceboxToolspecStatus{
			Conditions: []metav1.Condition{{
				Type:               spiceboxv1alpha1.SpiceboxToolspecConditionValid,
				Status:             metav1.ConditionTrue,
				Reason:             "OK",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}

	// A class with covered Toolspecs but bad EnvDefaults that shadows
	// the auth-injected GITHUB_TOKEN.
	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-bad-env"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image:     "x",
			Resources: goodUnitResources(),
			Tools: []spiceboxv1alpha1.SpiceboxTool{
				{Name: "echo", Command: []string{"/bin/echo"}},
			},
			Toolspecs:   []spiceboxv1alpha1.ToolspecRef{{Name: "echo-default"}},
			EnvDefaults: map[string]string{"GITHUB_TOKEN": "operator-supplied"},
		},
	}

	r, c := newReconcilerWithObjects(t, ts, cls)
	got := reconcileAndGet(t, r, c, cls.Name)

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond, "Valid condition should be stamped")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Valid.Status must be False; full cond=%+v", cond)
	assert.Equal(t, spiceboxv1alpha1.ReasonClassInvalidEnvDefaults, cond.Reason,
		"Valid.Reason must be InvalidEnvDefaults (coverage block must not overwrite)")
	assert.Contains(t, cond.Message, "GITHUB_TOKEN", "Valid.Message should mention the shadowed key")
}

// TestReconcile_ValidSpecWithFullCoverage exercises the happy-path side
// of the same fix: when validateSpec passes AND every tool is covered,
// the coverage block still stamps Valid=True/Resolved.
func TestReconcile_ValidSpecWithFullCoverage(t *testing.T) {
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "echo-default"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name:    "echo-default",
			Version: "1",
			Toolkit: spiceboxv1alpha1.ToolspecToolkitRef{
				Name:     "echo",
				Revision: "2026-04-25",
			},
			AllowSubcommands: []string{""},
		},
		Status: spiceboxv1alpha1.SpiceboxToolspecStatus{
			Conditions: []metav1.Condition{{
				Type:               spiceboxv1alpha1.SpiceboxToolspecConditionValid,
				Status:             metav1.ConditionTrue,
				Reason:             "OK",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-good"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image:     "x",
			Resources: goodUnitResources(),
			Tools: []spiceboxv1alpha1.SpiceboxTool{
				{Name: "echo", Command: []string{"/bin/echo"}},
			},
			Toolspecs: []spiceboxv1alpha1.ToolspecRef{{Name: "echo-default"}},
		},
	}

	r, c := newReconcilerWithObjects(t, ts, cls)
	got := reconcileAndGet(t, r, c, cls.Name)

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond, "Valid condition should be stamped")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "Valid.Status must be True; full cond=%+v", cond)
	assert.Equal(t, "Resolved", cond.Reason, "Valid.Reason must be Resolved")
	assert.Contains(t, got.Status.ToolspecCoverage["echo"], "echo-default",
		"ToolspecCoverage[echo] must include echo-default")
}

// validClassFixture returns a SpiceboxClass whose spec passes every
// validateSpec gate, so a test can mutate exactly the field it's probing.
func validClassFixture(name string) *spiceboxv1alpha1.SpiceboxClass {
	return &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image:     "x",
			Resources: goodUnitResources(),
			Tools: []spiceboxv1alpha1.SpiceboxTool{
				{Name: "echo", Command: []string{"/bin/echo"}},
			},
		},
	}
}

// newClassReconciler is newReconcilerWithObjects with the return values
// reordered to (client, reconciler), seeded with cls.
func newClassReconciler(t *testing.T, cls *spiceboxv1alpha1.SpiceboxClass) (client.Client, *spiceboxclass.Reconciler) {
	t.Helper()
	r, c := newReconcilerWithObjects(t, cls)
	return c, r
}

// An unrecognized sandbox kind must mark the class invalid. Lookup is
// fail-closed: falling back to the built-in pod backend would install a
// different substrate than the one asked for, silently.
func TestReconcile_UnknownSandboxKindIsInvalid(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.Spec.Sandbox.Kind = "not-a-real-backend"

	c, r := newClassReconciler(t, cls)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.NoError(t, err, "Reconcile records the invalid class rather than erroring")

	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "demo-class"}, &got))

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonClassInvalidSandbox, cond.Reason)
	assert.Contains(t, cond.Message, "not-a-real-backend", "the message must name the offending kind")
}

// An empty kind resolves to the built-in backend via the CRD default, so a
// class that never mentions sandboxing stays valid.
func TestReconcile_EmptySandboxKindDefaultsToPodAndIsValid(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.Spec.Sandbox.Kind = ""

	c, r := newClassReconciler(t, cls)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.NoError(t, err)

	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "demo-class"}, &got))

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
}

// A non-zero warmPool.replicas on the built-in "pod" backend — which
// implements no Prewarmer — must mark the class invalid rather than silently
// ignoring the request. Runtimes is left nil (the zero value): a kind absent
// from it type-asserts to "not a Prewarmer" exactly like one that never
// implements it, and this proves that path end-to-end through Reconcile,
// not just through validateSpec directly.
func TestReconcile_WarmPoolOnNonPrewarmerKindIsInvalid(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.Spec.Sandbox.WarmPool = &spiceboxv1alpha1.WarmPoolConfig{Replicas: 2, Namespaces: []string{"ns-a"}}

	c, r := newClassReconciler(t, cls)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.NoError(t, err, "Reconcile records the invalid class rather than erroring")

	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "demo-class"}, &got))

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonClassInvalidSandboxWarmPool, cond.Reason)
	assert.Contains(t, cond.Message, "pod", "the message must name the backend that cannot pre-warm")
}

// A non-zero warmPool.replicas with an empty namespaces list is invalid even
// on a kind that CAN pre-warm: capacity was asked for, but nowhere to put it
// was named. This must surface end-to-end through Reconcile, not just
// through validateSpec directly.
func TestReconcile_WarmPoolWithNoNamespacesIsInvalid(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.Spec.Sandbox.Kind = fakePrewarmControllerKindName
	cls.Spec.Sandbox.WarmPool = &spiceboxv1alpha1.WarmPoolConfig{Replicas: 2}

	rt := &recordingPrewarmRuntime{}
	r, c := newReconcilerWithObjects(t, cls)
	r.Runtimes = sandboxkinds.Runtimes{fakePrewarmControllerKindName: rt}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.NoError(t, err, "Reconcile records the invalid class rather than erroring")

	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "demo-class"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonClassInvalidSandboxWarmPool, cond.Reason)
	assert.Contains(t, cond.Message, "namespaces is empty")
	// Absent because Reconcile's warmPoolMalformed gate skips reconcilePool
	// outright for this shape — NOT because of the createAllowed split, which
	// this case never reaches. See
	// TestReconcile_InvalidClassGetsNoNewCapacityButStillSweepsItsDesiredSet
	// for the assertion that actually covers that gate.
	assert.Empty(t, rt.calls(), "a malformed warmPool stanza must not reach the backend at all")
}

// The positive path: a kind whose Runtime type-asserts to Prewarmer gets one
// ReconcilePool call PER NAMESPACE listed, each carrying the resolved
// replicas and the class's own UID (required so the pool objects can be
// owned by, and garbage collected with, the class) — and the class stays
// valid. ClassUID is asserted against a REAL, non-empty UID set on the
// fixture: the controller-runtime fake client does not mint one on Create,
// so asserting against got.UID here would pass vacuously (both sides "")
// even if ClassUID were dropped from the request entirely.
func TestReconcile_CallsReconcilePoolPerListedNamespace(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.UID = types.UID("class-uid-1")
	cls.Spec.Sandbox.Kind = fakePrewarmControllerKindName
	cls.Spec.Sandbox.WarmPool = &spiceboxv1alpha1.WarmPoolConfig{Replicas: 3, Namespaces: []string{"ns-a", "ns-b"}}

	rt := &recordingPrewarmRuntime{}
	r, c := newReconcilerWithObjects(t, cls)
	r.Runtimes = sandboxkinds.Runtimes{fakePrewarmControllerKindName: rt}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.NoError(t, err)

	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "demo-class"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "a Prewarmer kind honoring the request stays valid")

	calls := rt.calls()
	require.Len(t, calls, 2, "ReconcilePool must be called once per listed namespace")
	gotNamespaces := []string{calls[0].Namespace, calls[1].Namespace}
	assert.ElementsMatch(t, []string{"ns-a", "ns-b"}, gotNamespaces,
		"a pool must land in EACH listed namespace, named by the same poolNameFor the read side uses")
	for _, call := range calls {
		assert.Equal(t, "demo-class", call.ClassName)
		assert.Equal(t, int32(3), call.Replicas)
		assert.Equal(t, types.UID("class-uid-1"), call.ClassUID, "ClassUID must be the class's REAL uid")
		assert.Equal(t, got.Spec, call.Class)
	}
}

// wp == nil (warmPool never set, and no cluster-tier default) is a genuine
// no-op: reconcilePool has no namespace list to iterate at all, so
// ReconcilePool is never called.
func TestReconcile_NoWarmPoolMeansNoReconcilePoolCalls(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.Spec.Sandbox.Kind = fakePrewarmControllerKindName

	rt := &recordingPrewarmRuntime{}
	r, _ := newReconcilerWithObjects(t, cls)
	r.Runtimes = sandboxkinds.Runtimes{fakePrewarmControllerKindName: rt}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.NoError(t, err)
	assert.Empty(t, rt.calls(), "no warmPool config at all means nothing to reconcile")
}

// Explicitly turning pre-warming off (replicas: 0) while KEEPING the
// namespace list still calls ReconcilePool once per namespace with
// Replicas: 0 — this is the supported way to tear a pool down cleanly,
// distinct from clearing warmPool entirely (which leaves reconcilePool with
// no list to iterate; see WarmPoolConfig.Namespaces's doc comment).
func TestReconcile_ExplicitZeroReplicasTearsDownEachListedNamespace(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.Spec.Sandbox.Kind = fakePrewarmControllerKindName
	cls.Spec.Sandbox.WarmPool = &spiceboxv1alpha1.WarmPoolConfig{Replicas: 0, Namespaces: []string{"ns-a", "ns-b"}}

	rt := &recordingPrewarmRuntime{}
	r, _ := newReconcilerWithObjects(t, cls)
	r.Runtimes = sandboxkinds.Runtimes{fakePrewarmControllerKindName: rt}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.NoError(t, err)

	calls := rt.calls()
	require.Len(t, calls, 2)
	for _, call := range calls {
		assert.Equal(t, int32(0), call.Replicas)
	}
}

// A ReconcilePool failure must surface as a Reconcile error so the standard
// controller-runtime requeue-with-backoff applies — the same choice this
// controller already makes for computeCoverage's I/O errors just above. It
// must NOT suppress the status write, though: the class's spec is still
// VALID (the failure is operational, e.g. a transient API error), so
// Valid/observedGeneration must still be persisted before Reconcile returns
// the error for requeue.
func TestReconcile_ReconcilePoolErrorSurfacesAsReconcileErrorButStillWritesStatus(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.Spec.Sandbox.Kind = fakePrewarmControllerKindName
	cls.Spec.Sandbox.WarmPool = &spiceboxv1alpha1.WarmPoolConfig{Replicas: 1, Namespaces: []string{"ns-a"}}

	rt := &recordingPrewarmRuntime{reconcilePoolErr: errBoom}
	r, c := newReconcilerWithObjects(t, cls)
	r.Runtimes = sandboxkinds.Runtimes{fakePrewarmControllerKindName: rt}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.Error(t, err, "a ReconcilePool failure must not be swallowed")
	assert.ErrorIs(t, err, errBoom)

	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "demo-class"}, &got),
		"the class object must still exist and be reachable")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond, "the Valid condition must be persisted even though Reconcile returned an "+
		"operational error — an operational pool failure must not suppress the spec verdict")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "the SPEC itself is valid; only the operational "+
		"reconcile failed")
}

// ErrPrewarmingUnavailable is different in kind from an ordinary operational
// failure: it is ReconcilePool's own capability probe reporting a fact
// validateSpec's static type assertion could not see ("this cluster cannot
// pre-warm that kind right now"). It must downgrade the class to
// Valid=False/InvalidSandboxWarmPool — the SAME outcome a statically
// detectable WarmPool misconfiguration gets — AND the status write must
// still happen, AND Reconcile must return nil (matching every other
// validateSpec-shaped rejection: the class is recorded invalid, not errored).
func TestReconcile_PrewarmingUnavailableDowngradesToInvalidAndWritesStatus(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.Spec.Sandbox.Kind = fakePrewarmControllerKindName
	cls.Spec.Sandbox.WarmPool = &spiceboxv1alpha1.WarmPoolConfig{Replicas: 1, Namespaces: []string{"ns-a"}}

	rt := &recordingPrewarmRuntime{
		reconcilePoolErr: fmt.Errorf("agent-sandbox: %w (extensions CRDs not installed)", sandboxkinds.ErrPrewarmingUnavailable),
	}
	r, c := newReconcilerWithObjects(t, cls)
	r.Runtimes = sandboxkinds.Runtimes{fakePrewarmControllerKindName: rt}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.NoError(t, err, "a pre-warming-unavailable class is recorded invalid, not errored")

	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "demo-class"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonClassInvalidSandboxWarmPool, cond.Reason)
	assert.Contains(t, cond.Message, "extensions CRDs not installed")
}

// The mirror of the test above, for the !classOwned branch (controller.go's
// `errors.Is(err, ErrPrewarmingUnavailable) && !classOwned`): a
// TIER-INHERITED warmPool (the class sets none of its own) on a kind whose
// Runtime IS structurally a Prewarmer, but whose capability probe reports
// ErrPrewarmingUnavailable, must NOT invalidate the class — it must log and
// skip, exactly like the static not-Prewarmer case, because the class itself
// never asked for anything the cluster can't currently honor.
func TestReconcile_TierInheritedPrewarmingUnavailableStaysValidAndLogs(t *testing.T) {
	cas := &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			Defaults: &spiceboxv1alpha1.SettingsDefaults{
				Sandbox: &spiceboxv1alpha1.SandboxBackend{
					WarmPool: &spiceboxv1alpha1.WarmPoolConfig{Replicas: 3, Namespaces: []string{"ns-cluster"}},
				},
			},
		},
	}
	cls := validClassFixture("demo-class")
	cls.Spec.Sandbox.Kind = fakePrewarmControllerKindName
	// No cls.Spec.Sandbox.WarmPool: classOwned is false, and the request
	// reaching the Runtime is entirely the cluster tier's.

	rt := &recordingPrewarmRuntime{
		reconcilePoolErr: fmt.Errorf("agent-sandbox: %w (extensions CRDs not installed)", sandboxkinds.ErrPrewarmingUnavailable),
	}
	r, c := newReconcilerWithObjects(t, cas, cls)
	r.Runtimes = sandboxkinds.Runtimes{fakePrewarmControllerKindName: rt}

	var msgs []string
	logger := funcr.New(func(prefix, args string) {
		msgs = append(msgs, prefix+" "+args)
	}, funcr.Options{})
	ctx := ctrllog.IntoContext(context.Background(), logger)

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.NoError(t, err, "a tier-inherited unavailable-prewarming request must not error the class")

	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "demo-class"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"a tier-inherited request the cluster cannot currently honor must not invalidate a class that never asked itself; full cond=%+v", cond)

	found := false
	for _, m := range msgs {
		if strings.Contains(m, "demo-class") && strings.Contains(m, "not currently available") {
			found = true
			break
		}
	}
	assert.True(t, found, "expected an INFO log naming the class and the unavailability; got %v", msgs)
}

// Item 5 end-to-end: a class that names a Prewarmer-capable kind but never
// sets its own warmPool must still get a pool sized by the CLUSTER tier
// default — proving the controller reads the TIER-RESOLVED value, not
// class.Spec.Sandbox.WarmPool directly.
func TestReconcile_ClusterTierWarmPoolSizesAClassThatDoesNotOverride(t *testing.T) {
	cas := &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			Defaults: &spiceboxv1alpha1.SettingsDefaults{
				Sandbox: &spiceboxv1alpha1.SandboxBackend{
					WarmPool: &spiceboxv1alpha1.WarmPoolConfig{Replicas: 5, Namespaces: []string{"ns-cluster"}},
				},
			},
		},
	}
	cls := validClassFixture("demo-class")
	cls.Spec.Sandbox.Kind = fakePrewarmControllerKindName
	// No cls.Spec.Sandbox.WarmPool — the cluster default must apply.

	rt := &recordingPrewarmRuntime{}
	r, _ := newReconcilerWithObjects(t, cas, cls)
	r.Runtimes = sandboxkinds.Runtimes{fakePrewarmControllerKindName: rt}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.NoError(t, err)

	calls := rt.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "ns-cluster", calls[0].Namespace)
	assert.Equal(t, int32(5), calls[0].Replicas)
}

// Item 1 (review round 3, CRITICAL): a cluster-tier warmPool default must
// NOT invalidate a class that never set its own warmPool, even when the
// class's kind cannot pre-warm at all. Before this fix, ResolveClassWarmPool
// folding in the cluster default reached validateSpec's WarmPool-vs-Prewarmer
// check unconditionally — so the moment ANY cluster-wide warmPool.replicas>0
// was set, EVERY class using the default "pod" kind flipped to
// Valid=False/InvalidSandboxWarmPool, cluster-wide, the instant the
// ClusterAgentSettings watch (added in the prior round) re-enqueued them.
//
// Uses the REAL "pod" kind (blank-imported), not a fake — pod genuinely does
// not implement sandboxkinds.Prewarmer, which is exactly the scenario that
// broke.
func TestReconcile_ClusterTierWarmPoolOnNonPrewarmerKindStaysValid(t *testing.T) {
	cas := &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			Defaults: &spiceboxv1alpha1.SettingsDefaults{
				Sandbox: &spiceboxv1alpha1.SandboxBackend{
					WarmPool: &spiceboxv1alpha1.WarmPoolConfig{Replicas: 3, Namespaces: []string{"ns-cluster"}},
				},
			},
		},
	}
	cls := validClassFixture("demo-class")
	// Sandbox.Kind and Sandbox.WarmPool both left unset: resolves to "pod",
	// and the class itself never asked for a pool at all.

	r, c := newReconcilerWithObjects(t, cas, cls)

	var msgs []string
	logger := funcr.New(func(prefix, args string) {
		msgs = append(msgs, prefix+" "+args)
	}, funcr.Options{})
	ctx := ctrllog.IntoContext(context.Background(), logger)

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.NoError(t, err)

	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "demo-class"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"a class that never set its own warmPool must stay valid regardless of what the cluster tier resolves to; full cond=%+v", cond)

	found := false
	for _, m := range msgs {
		if strings.Contains(m, "demo-class") && strings.Contains(m, "pod") {
			found = true
			break
		}
	}
	assert.True(t, found,
		"the tier-inherited-but-unsatisfiable case must be diagnosable via a log line naming the class and kind, not silently dropped; got %v", msgs)
}

// Item 3 (Minor): fetchClusterSettings failing with a non-NotFound error must
// NOT abort Reconcile before the status write — the same "operational
// failure, not a spec verdict" shape item 4's fix already applies to
// reconcilePool. The class's OWN spec is untouched by this failure (it never
// even reaches ResolveClassWarmPool), so its Valid condition must still be
// computed and persisted; the fetch error itself still causes a requeue.
func TestReconcile_FetchClusterSettingsErrorStillWritesStatusAndRequeues(t *testing.T) {
	cls := validClassFixture("demo-class")

	scheme := testfixtures.NewScheme(t)
	getErr := errors.New("apiserver unreachable")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cls).
		WithStatusSubresource(&spiceboxv1alpha1.SpiceboxClass{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*spiceboxv1alpha1.ClusterAgentSettings); ok {
					return getErr
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	reg, err := registry.NewWithBuiltins(nil)
	require.NoError(t, err)
	r := &spiceboxclass.Reconciler{Client: c, Registry: reg}

	_, reconcileErr := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.Error(t, reconcileErr, "the fetch failure must cause a requeue")
	assert.ErrorIs(t, reconcileErr, getErr)

	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "demo-class"}, &got),
		"the class object must still exist and be reachable")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond, "the Valid condition must be persisted even though fetching cluster settings failed")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "the class's own spec never depended on the failed fetch")
}

// Item 2 wiring (spiceboxclass side): reconcilePool must call
// SweepOrphanedPools with the desired-namespace set matching wp's shape in
// each case — the REAL List+Delete mechanics live in
// pkg/tools/sandboxkinds/agentsandbox (see its TestSweepOrphanedPools_* suite,
// including the cases-(a)/(b) deliberate-failure transcripts); these prove
// only that the controller computes and passes the right desired set.
func TestReconcile_SweepDesiredNamespaces(t *testing.T) {
	cases := []struct {
		name           string
		warmPool       *spiceboxv1alpha1.WarmPoolConfig
		wantNamespaces []string
	}{
		{
			name:           "nil warmPool: desired is empty (reclaim everywhere)",
			warmPool:       nil,
			wantNamespaces: nil,
		},
		{
			name:           "replicas: 0, namespaces kept: desired is empty (reclaim everywhere)",
			warmPool:       &spiceboxv1alpha1.WarmPoolConfig{Replicas: 0, Namespaces: []string{"ns-a", "ns-b"}},
			wantNamespaces: nil,
		},
		{
			name:           "replicas>0 with namespaces: desired matches the list",
			warmPool:       &spiceboxv1alpha1.WarmPoolConfig{Replicas: 2, Namespaces: []string{"ns-a"}},
			wantNamespaces: []string{"ns-a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cls := validClassFixture("demo-class")
			cls.UID = types.UID("class-uid-1")
			cls.Spec.Sandbox.Kind = fakePrewarmControllerKindName
			cls.Spec.Sandbox.WarmPool = tc.warmPool

			rt := &recordingPrewarmRuntime{}
			r, _ := newReconcilerWithObjects(t, cls)
			r.Runtimes = sandboxkinds.Runtimes{fakePrewarmControllerKindName: rt}

			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "demo-class"},
			})
			require.NoError(t, err)

			sweeps := rt.sweeps()
			require.Len(t, sweeps, 1, "SweepOrphanedPools must be called exactly once per reconcile")
			assert.Equal(t, "demo-class", sweeps[0].className)
			assert.Equal(t, types.UID("class-uid-1"), sweeps[0].classUID)
			assert.Equal(t, tc.wantNamespaces, sweeps[0].desiredNamespaces)
		})
	}
}

// A SweepOrphanedPools failure is an ordinary operational error: requeued,
// but must not suppress the status write, matching every other operational
// failure path in this controller.
func TestReconcile_SweepErrorSurfacesAsReconcileErrorButStillWritesStatus(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.Spec.Sandbox.Kind = fakePrewarmControllerKindName

	rt := &recordingPrewarmRuntime{sweepErr: errBoom}
	r, c := newReconcilerWithObjects(t, cls)
	r.Runtimes = sandboxkinds.Runtimes{fakePrewarmControllerKindName: rt}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, errBoom)

	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "demo-class"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond, "a sweep failure must not suppress the status write")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
}

// Item 1 (review round 4, IMPORTANT): reclaiming capacity is not a function
// of overall spec validity. A class invalid for a reason UNRELATED to
// warmPool (a zeroed CPU quantity, here) that has warmPool cleared must
// still have its pools reclaimed — the sweep runs independently of specErr;
// only pool CREATION stays gated on it. Before this fix, the whole
// reconcilePool call (create AND sweep) was gated on specErr == nil, so an
// admin's explicit teardown intent (clearing warmPool) was silently defeated
// for as long as the class happened to be invalid for any other reason.
func TestReconcile_SweepStillRunsWhenClassIsInvalidForAnUnrelatedReason(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.Spec.Sandbox.Kind = fakePrewarmControllerKindName
	// warmPool cleared (nil): the admin's teardown intent.
	cls.Spec.Sandbox.WarmPool = nil
	// An UNRELATED invalidity: a zeroed CPU quantity fails validateSpec's
	// resource check, nothing to do with warmPool.
	cls.Spec.Resources.CPU = resource.MustParse("0")

	rt := &recordingPrewarmRuntime{}
	r, c := newReconcilerWithObjects(t, cls)
	r.Runtimes = sandboxkinds.Runtimes{fakePrewarmControllerKindName: rt}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.NoError(t, err)

	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "demo-class"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "the UNRELATED invalidity is still reported")
	assert.Equal(t, spiceboxv1alpha1.ReasonClassInvalidResource, cond.Reason,
		"the resources failure, not a warmPool one, must be what's reported")

	// warmPool is nil here, so the per-namespace creation loop has nothing to
	// iterate regardless of createAllowed; this assertion pins the no-op, not
	// the gate. The gate itself is covered by
	// TestReconcile_InvalidClassGetsNoNewCapacityButStillSweepsItsDesiredSet.
	assert.Empty(t, rt.calls(), "a cleared warmPool asks for no capacity anywhere")
	sweeps := rt.sweeps()
	require.Len(t, sweeps, 1, "the sweep must still run despite the unrelated invalidity")
	assert.Equal(t, "demo-class", sweeps[0].className)
	assert.Nil(t, sweeps[0].desiredNamespaces, "warmPool is cleared: desired must be empty, reclaiming everything")
}

// The mirror case: a class whose warmPool STANZA ITSELF is malformed
// (replicas>0, no namespaces) must NOT have its pools swept. The desired set
// cannot be computed honestly from a malformed request — an empty desired
// set in that case would tear down real capacity over what is more likely a
// typo mid-edit than an intentional teardown.
func TestReconcile_MalformedWarmPoolStanzaSkipsTheSweep(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.Spec.Sandbox.Kind = fakePrewarmControllerKindName
	cls.Spec.Sandbox.WarmPool = &spiceboxv1alpha1.WarmPoolConfig{Replicas: 3} // no namespaces named

	rt := &recordingPrewarmRuntime{}
	r, c := newReconcilerWithObjects(t, cls)
	r.Runtimes = sandboxkinds.Runtimes{fakePrewarmControllerKindName: rt}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.NoError(t, err)

	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "demo-class"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonClassInvalidSandboxWarmPool, cond.Reason)

	// Both absences come from the SAME cause — Reconcile's warmPoolMalformed
	// gate skips reconcilePool entirely — so neither says anything about the
	// createAllowed split inside it.
	assert.Empty(t, rt.calls(), "a malformed warmPool stanza must not reach the backend at all")
	assert.Empty(t, rt.sweeps(),
		"no sweep either: the desired set cannot be computed honestly from a malformed warmPool stanza")
}

// --- Sweeping across kinds, not only the class's current kind ---------------
//
// SweepOrphanedPools is per-kind by construction: a backend can only find its
// own native objects. So reclamation must be a function of the CLASS, never of
// whichever kind the class happens to name right now — otherwise editing
// spec.sandbox.kind away from a Prewarmer-capable backend strands that
// backend's pools permanently. Nothing would ever resolve the OLD kind's
// Runtime again, the ownerReference cannot help (the class still exists), and
// the idle pods bill forever, indistinguishable from a class that was always
// on the new kind.
//
// Both edit shapes are covered: keeping the warmPool stanza (which also makes
// the class invalid, since the new kind cannot pre-warm) and clearing it.

// Variant A: the kind changed to one that cannot pre-warm, and the admin left
// warmPool in place. The class is now invalid FOR THAT REASON, which is
// exactly the shape that used to return before the sweep was ever reached.
func TestReconcile_KindChangedAwayFromAPrewarmerStillSweepsTheOldKind(t *testing.T) {
	cls := validClassFixture("demo-class")
	// The built-in "pod" backend implements no Prewarmer.
	cls.Spec.Sandbox.Kind = "pod"
	cls.Spec.Sandbox.WarmPool = &spiceboxv1alpha1.WarmPoolConfig{
		Replicas: 3, Namespaces: []string{"ns-a", "ns-b"},
	}

	// The Runtime for the kind the class USED to name is still registered; it
	// still holds that class's pools.
	old := &recordingPrewarmRuntime{}
	r, c := newReconcilerWithObjects(t, cls)
	r.Runtimes = sandboxkinds.Runtimes{fakePrewarmControllerKindName: old}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.NoError(t, err)

	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "demo-class"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"a class-owned warmPool on a kind that cannot pre-warm is still reported invalid")

	assert.Empty(t, old.calls(), "the old kind must not be given NEW capacity")
	sweeps := old.sweeps()
	require.Len(t, sweeps, 1,
		"the old kind's pools are reachable ONLY through its own SweepOrphanedPools; "+
			"resolving just the current kind strands them forever")
	assert.Equal(t, "demo-class", sweeps[0].className)
	assert.Nil(t, sweeps[0].desiredNamespaces,
		"a kind the class no longer names desires NOTHING, whatever warmPool.namespaces says")
}

// Variant B: the kind changed AND the admin cleared warmPool — the class is
// perfectly valid, so nothing about its status hints that six idle pods are
// still running under the previous backend.
func TestReconcile_KindChangedAndWarmPoolClearedStillSweepsTheOldKind(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.Spec.Sandbox.Kind = "pod"
	cls.Spec.Sandbox.WarmPool = nil

	old := &recordingPrewarmRuntime{}
	r, c := newReconcilerWithObjects(t, cls)
	r.Runtimes = sandboxkinds.Runtimes{fakePrewarmControllerKindName: old}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.NoError(t, err)

	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "demo-class"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "clearing warmPool is a perfectly valid edit")

	sweeps := old.sweeps()
	require.Len(t, sweeps, 1, "no list means pre-warming OFF and no spend — for EVERY kind, not just the current one")
	assert.Nil(t, sweeps[0].desiredNamespaces)
}

// The desired set belongs to the CURRENT kind alone. A second Prewarmer
// backend must be told it desires nothing, or a class's namespace list would
// preserve pools under a backend the class does not use.
func TestReconcile_OnlyTheCurrentKindReceivesTheDesiredNamespaces(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.Spec.Sandbox.Kind = fakePrewarmControllerKindName
	cls.Spec.Sandbox.WarmPool = &spiceboxv1alpha1.WarmPoolConfig{
		Replicas: 2, Namespaces: []string{"ns-a"},
	}

	current, other := &recordingPrewarmRuntime{}, &recordingPrewarmRuntime{}
	r, _ := newReconcilerWithObjects(t, cls)
	r.Runtimes = sandboxkinds.Runtimes{
		fakePrewarmControllerKindName: current,
		// A second Prewarmer-capable backend this class does not name. The key
		// is arbitrary: sweeping walks the constructed Runtimes, which is what
		// makes reclamation independent of what the class resolves to today.
		"another-prewarm-kind": other,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.NoError(t, err)

	currentSweeps := current.sweeps()
	require.Len(t, currentSweeps, 1)
	assert.Equal(t, []string{"ns-a"}, currentSweeps[0].desiredNamespaces,
		"the current kind keeps what the class asks for")

	otherSweeps := other.sweeps()
	require.Len(t, otherSweeps, 1, "every registered Prewarmer is swept, not just the current kind")
	assert.Nil(t, otherSweeps[0].desiredNamespaces,
		"a kind the class does not name desires nothing; passing the class's namespaces "+
			"would preserve pools under a backend it never uses")
	assert.Empty(t, other.calls(), "only the CURRENT kind is given new capacity")
}

// THE createAllowed GATE ITSELF. The three assertions that previously read as
// covering it were each absent for an unrelated reason: two ran with a
// MALFORMED warmPool stanza (so Reconcile skips reconcilePool entirely) and
// one with warmPool cleared (so the per-namespace loop has nothing to iterate).
// Replacing `if createAllowed {` with `if true {` left the whole package suite
// green.
//
// This is the missing combination: invalid for an UNRELATED reason, plus a
// WELL-FORMED warmPool on a kind that CAN pre-warm — the only shape in which
// the loop would otherwise run. Both halves of the split are asserted, because
// the gate is a split and not a skip: creation is refused, the sweep still
// happens, and it happens with the namespaces the class still desires (NOT an
// empty set, which would tear down capacity the admin never asked to release).
func TestReconcile_InvalidClassGetsNoNewCapacityButStillSweepsItsDesiredSet(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.Spec.Sandbox.Kind = fakePrewarmControllerKindName
	// A well-formed request: replicas AND namespaces, on a Prewarmer kind. So
	// nothing about the warmPool stanza itself is malformed, and reconcilePool
	// really is reached with a non-empty wp.Namespaces to iterate.
	cls.Spec.Sandbox.WarmPool = &spiceboxv1alpha1.WarmPoolConfig{
		Replicas: 3, Namespaces: []string{"ns-a", "ns-b"},
	}
	// An UNRELATED invalidity: a zeroed CPU quantity fails validateSpec's
	// resource check, nothing to do with warmPool.
	cls.Spec.Resources.CPU = resource.MustParse("0")

	rt := &recordingPrewarmRuntime{}
	r, c := newReconcilerWithObjects(t, cls)
	r.Runtimes = sandboxkinds.Runtimes{fakePrewarmControllerKindName: rt}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.NoError(t, err)

	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "demo-class"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, spiceboxv1alpha1.ReasonClassInvalidResource, cond.Reason,
		"precondition: the class must be invalid for a reason that is NOT the warmPool stanza")

	assert.Empty(t, rt.calls(),
		"createAllowed is false, so an invalid class must be given no new capacity — "+
			"even though its warmPool request is perfectly well formed")

	sweeps := rt.sweeps()
	require.Len(t, sweeps, 1, "reclaiming capacity is not a function of spec validity")
	assert.Equal(t, []string{"ns-a", "ns-b"}, sweeps[0].desiredNamespaces,
		"an unrelated spec error changes what may be CREATED, never what is DESIRED; "+
			"an empty desired set here would tear down capacity the admin still wants")
}

// The THIRD instance of the status-write-suppression shape in this function,
// after reconcilePool and fetchClusterSettings: a computeCoverage failure is
// an ordinary operational error (an unreadable SpiceboxToolspec), not a
// verdict on the class's own spec, so it must requeue WITHOUT swallowing the
// Valid condition and observedGeneration. Returning early there leaves an
// operator staring at an empty status instead of a diagnosis.
func TestReconcile_CoverageFetchErrorStillWritesStatusAndRequeues(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.Spec.Toolspecs = []spiceboxv1alpha1.ToolspecRef{{Name: "demo-toolspec"}}

	scheme := testfixtures.NewScheme(t)
	getErr := errors.New("apiserver unreachable")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cls).
		WithStatusSubresource(&spiceboxv1alpha1.SpiceboxClass{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				// NOT a NotFound: computeCoverage deliberately treats an absent
				// toolspec as "not covering," so only a hard read failure
				// reaches the error path under test.
				if _, ok := obj.(*spiceboxv1alpha1.SpiceboxToolspec); ok {
					return getErr
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	reg, err := registry.NewWithBuiltins(nil)
	require.NoError(t, err)
	r := &spiceboxclass.Reconciler{Client: c, Scheme: scheme, Registry: reg}

	_, reconcileErr := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.Error(t, reconcileErr, "the coverage read failure must cause a requeue")
	assert.ErrorIs(t, reconcileErr, getErr)

	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "demo-class"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond, "a coverage-read failure must not suppress the status write")
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"the class's own spec is valid; an unreadable toolspec says nothing about it")
	assert.Equal(t, cls.Generation, got.Status.ObservedGeneration,
		"observedGeneration must still be persisted, or the class looks unreconciled")
}

// poolErr and covErr are INDEPENDENTLY reachable: covErr needs specErr == nil,
// which says nothing about poolErr. Returning only the first would drop the
// other on the floor with nothing logging it — the no-silent-errors rule. One
// interceptor fails both reads, so both errors are live in the same pass.
func TestReconcile_PoolAndCoverageErrorsAreBothReturned(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.Spec.Toolspecs = []spiceboxv1alpha1.ToolspecRef{{Name: "demo-toolspec"}}

	scheme := testfixtures.NewScheme(t)
	settingsErr := errors.New("cluster settings unreachable")
	coverageErr := errors.New("toolspec unreachable")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cls).
		WithStatusSubresource(&spiceboxv1alpha1.SpiceboxClass{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				switch obj.(type) {
				case *spiceboxv1alpha1.ClusterAgentSettings:
					return settingsErr
				case *spiceboxv1alpha1.SpiceboxToolspec:
					return coverageErr
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	reg, err := registry.NewWithBuiltins(nil)
	require.NoError(t, err)
	r := &spiceboxclass.Reconciler{Client: c, Scheme: scheme, Registry: reg}

	_, reconcileErr := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.Error(t, reconcileErr)
	assert.ErrorIs(t, reconcileErr, settingsErr, "the pool-path failure must survive")
	assert.ErrorIs(t, reconcileErr, coverageErr,
		"the coverage failure must survive too: returning only the first would discard "+
			"the second with nothing logging it")

	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "demo-class"}, &got))
	require.NotNil(t, meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid),
		"neither operational failure may suppress the status write")
}

// --- Toolchain resolution ---------------------------------------------------
//
// A class declaring spec.toolchains resolves them (pkg/tools/toolchain/resolve —
// the SAME resolver the SpiceboxSession controller calls at bind time) so the
// warm pool can be built with the right overlay. Three shapes: resolvable
// (recorded on status, carried to the pool), unresolvable (recorded as a
// message, Valid untouched, pooling skipped), and absent (nothing resolved,
// nothing written, pooling proceeds with an empty Toolchains list).

// validToolchainFixture returns a SpiceboxToolchain whose Status is already
// Valid=True (as if the SpiceboxToolchain controller had already reconciled
// it), so resolve.Resolve succeeds. Status is set directly on the object
// before Create, the same pattern this file already uses for SpiceboxToolspec
// fixtures (see TestReconcile_InvalidEnvDefaultsWithCoveredToolspec) — neither
// type is registered WithStatusSubresource in newReconcilerWithObjects, so a
// plain Create persists the inline Status too.
func validToolchainFixture(name string) *spiceboxv1alpha1.SpiceboxToolchain {
	return &spiceboxv1alpha1.SpiceboxToolchain{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.SpiceboxToolchainSpec{
			Source:    spiceboxv1alpha1.ToolchainSource{Kind: "image", Image: "ap-toolchain-" + name + ":dev", Prefix: "/opt/ap-toolchains/" + name},
			SizeBytes: 100,
		},
		Status: spiceboxv1alpha1.SpiceboxToolchainStatus{
			Conditions: []metav1.Condition{{
				Type:               spiceboxv1alpha1.SpiceboxToolchainConditionValid,
				Status:             metav1.ConditionTrue,
				Reason:             "OK",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

// Case 1: a class naming a resolvable toolchain gets status.ResolvedToolchains
// populated with a non-empty digest, and the resolved mounts are carried
// through to the pool block's PoolRequest — proving the two pieces (status
// recording, and passing the mounts to ReconcilePool) are actually wired
// together, not just independently plausible.
func TestReconcile_ResolvableToolchainsAreRecordedAndPassedToPool(t *testing.T) {
	tc := validToolchainFixture("go")

	cls := validClassFixture("demo-class")
	cls.UID = types.UID("class-uid-1")
	cls.Spec.Sandbox.Kind = fakePrewarmControllerKindName
	cls.Spec.Sandbox.WarmPool = &spiceboxv1alpha1.WarmPoolConfig{Replicas: 2, Namespaces: []string{"ns-a"}}
	cls.Spec.Toolchains = []string{"go"}

	rt := &recordingPrewarmRuntime{}
	r, c := newReconcilerWithObjects(t, tc, cls)
	r.Runtimes = sandboxkinds.Runtimes{fakePrewarmControllerKindName: rt}

	got := reconcileAndGet(t, r, c, cls.Name)

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "a resolvable toolchain leaves the class valid; full cond=%+v", cond)

	require.Len(t, got.Status.ResolvedToolchains, 1, "status.ResolvedToolchains must be populated")
	assert.Equal(t, "go", got.Status.ResolvedToolchains[0].Name)
	assert.NotEmpty(t, got.Status.ToolchainSetDigest, "the digest must be non-empty on a successful resolution")
	assert.Empty(t, got.Status.ToolchainResolutionMessage, "no failure message on a successful resolution")

	calls := rt.calls()
	require.Len(t, calls, 1, "ReconcilePool must be called for the one listed namespace")
	require.Len(t, calls[0].Toolchains, 1, "the resolved mounts must be carried to the PoolRequest")
	assert.Equal(t, "go", calls[0].Toolchains[0].Name)
}

// Case 2: a class naming a toolchain that does not exist gets
// status.ToolchainResolutionMessage set, but Valid stays True (class validity
// must not depend on the toolchain catalog — see
// ToolchainResolutionMessage's doc comment) and ReconcilePool is NOT called
// (pooling is skipped for this pass, since the overlay cannot be built).
//
// Also covers two behaviors the design rationale rests on, previously
// unasserted:
//   - LAST KNOWN-GOOD PRESERVATION: the fixture pre-seeds
//     status.ResolvedToolchains/ToolchainSetDigest as if a prior pass had
//     resolved successfully, and asserts both survive this failed pass
//     byte-for-byte — a stale-but-real record is more useful to an operator
//     than one wiped to empty.
//   - THE SWEEP STILL RUNS: reclaiming orphaned pool capacity does not read
//     toolchains at all, so a toolchain resolution failure must not freeze
//     it — only pool CREATION is gated. Asserted via rt.sweeps(), including
//     that the desired set is NOT emptied by the failure (only pool creation
//     is refused; what is DESIRED is unchanged).
//
// The "not called" half of the ReconcilePool assertion is deliberately
// verified against a broken guard, not just asserted (see the task report for
// the transcript): a negative assertion like Empty(rt.calls()) passes
// silently if the guard that's supposed to produce it is missing but the
// test never reached the pool block for some unrelated reason — this
// fixture's warmPool IS well-formed and its kind DOES implement Prewarmer, so
// the only thing stopping ReconcilePool is the toolchain guard under test.
func TestReconcile_MissingToolchainRecordsMessageStaysValidSkipsPool(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.Spec.Sandbox.Kind = fakePrewarmControllerKindName
	cls.Spec.Sandbox.WarmPool = &spiceboxv1alpha1.WarmPoolConfig{Replicas: 2, Namespaces: []string{"ns-a"}}
	cls.Spec.Toolchains = []string{"does-not-exist"}
	// No SpiceboxToolchain named "does-not-exist" is seeded.

	// A last known-good record from a prior successful pass — this failed
	// pass must leave it untouched, not clear it to empty.
	staleMount := spiceboxv1alpha1.ToolchainMount{
		Name: "old-go", SourceKind: "image", Image: "img-old-go@sha256:1", Prefix: "/opt/ap-toolchains/old-go",
	}
	cls.Status.ResolvedToolchains = []spiceboxv1alpha1.ToolchainMount{staleMount}
	cls.Status.ToolchainSetDigest = "stale-digest-from-a-prior-successful-pass"

	rt := &recordingPrewarmRuntime{}
	r, c := newReconcilerWithObjects(t, cls)
	r.Runtimes = sandboxkinds.Runtimes{fakePrewarmControllerKindName: rt}

	got := reconcileAndGet(t, r, c, cls.Name)

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"a toolchain resolution failure must NOT flip Valid; full cond=%+v", cond)

	assert.Contains(t, got.Status.ToolchainResolutionMessage, "does-not-exist",
		"the message must name the failing toolchain")
	assert.Equal(t, []spiceboxv1alpha1.ToolchainMount{staleMount}, got.Status.ResolvedToolchains,
		"the LAST KNOWN-GOOD record must survive a resolution failure untouched")
	assert.Equal(t, "stale-digest-from-a-prior-successful-pass", got.Status.ToolchainSetDigest,
		"the last known-good digest must survive too")

	assert.Empty(t, rt.calls(), "pooling must be skipped this pass when toolchain resolution fails")

	sweeps := rt.sweeps()
	require.Len(t, sweeps, 1,
		"the sweep must still run on a toolchain resolution failure — SweepOrphanedPools reads only "+
			"className/classUID/desiredNamespaces, none of which a toolchain failure invalidates")
	assert.Equal(t, []string{"ns-a"}, sweeps[0].desiredNamespaces,
		"a toolchain failure must narrow what may be CREATED, never what is DESIRED — an empty desired "+
			"set here would tear down capacity the class still wants")
}

// A resolve.Resolve failure that is classifiable as NEITHER
// ErrToolchainMissing NOR ErrToolchainNotValid (a transient apiserver Get
// failure, here) must be treated as an ordinary OPERATIONAL failure, not a
// fact about the toolchain: no message persisted (it would mislead the
// moment the apiserver recovers), Valid untouched, pooling skipped this
// pass, and — unlike the missing/invalid case — Reconcile itself returns an
// error so controller-runtime requeues with backoff, because the
// SpiceboxToolchain watch cannot rescue this case: nothing about the
// toolchain object actually changed.
func TestReconcile_TransientToolchainResolveErrorRequeuesWithoutPersistingMessage(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.Spec.Sandbox.Kind = fakePrewarmControllerKindName
	cls.Spec.Sandbox.WarmPool = &spiceboxv1alpha1.WarmPoolConfig{Replicas: 1, Namespaces: []string{"ns-a"}}
	cls.Spec.Toolchains = []string{"go"}

	scheme := testfixtures.NewScheme(t)
	getErr := errors.New("etcdserver: request timed out")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cls).
		WithStatusSubresource(&spiceboxv1alpha1.SpiceboxClass{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*spiceboxv1alpha1.SpiceboxToolchain); ok {
					return getErr
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	reg, err := registry.NewWithBuiltins(nil)
	require.NoError(t, err)
	rt := &recordingPrewarmRuntime{}
	r := &spiceboxclass.Reconciler{
		Client: c, Scheme: scheme, Registry: reg,
		Runtimes: sandboxkinds.Runtimes{fakePrewarmControllerKindName: rt},
	}

	_, reconcileErr := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo-class"},
	})
	require.Error(t, reconcileErr, "a transient toolchain-resolve failure must requeue — the watch cannot rescue it")
	assert.ErrorIs(t, reconcileErr, getErr)

	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "demo-class"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "the class's own spec never depended on the failed resolve")
	assert.Empty(t, got.Status.ToolchainResolutionMessage,
		"a transient failure must NOT be persisted as a resolution message — it would mislead once the apiserver recovers")
	assert.Empty(t, rt.calls(), "pooling must still be skipped this pass")
}

// Case 3: a class naming no toolchains at all resolves nothing (no call to
// resolve.Resolve — there is nothing to seed a SpiceboxToolchain for, so a
// bug that resolved anyway would 404 and fail this test); pooling proceeds
// normally with an empty Toolchains list on the PoolRequest.
//
// The fixture pre-seeds all three status fields as if an admin had just
// cleared spec.toolchains after a prior pass left a record (successful or
// failed) — proving the else-branch in Reconcile actually CLEARS a stale
// record rather than merely never writing to a field that started empty.
// Diagnostic-only blast radius (nothing non-test reads these fields), but a
// class naming no toolchains should not keep advertising mounts, a digest,
// or a resolution failure for a toolchain it no longer references.
func TestReconcile_NoToolchainsResolvesNothingAndPoolsWithEmptyToolchains(t *testing.T) {
	cls := validClassFixture("demo-class")
	cls.Spec.Sandbox.Kind = fakePrewarmControllerKindName
	cls.Spec.Sandbox.WarmPool = &spiceboxv1alpha1.WarmPoolConfig{Replicas: 1, Namespaces: []string{"ns-a"}}
	// cls.Spec.Toolchains left unset.

	cls.Status.ResolvedToolchains = []spiceboxv1alpha1.ToolchainMount{
		{Name: "stale", SourceKind: "image", Image: "img-stale@sha256:1", Prefix: "/opt/ap-toolchains/stale"},
	}
	cls.Status.ToolchainSetDigest = "stale-digest"
	cls.Status.ToolchainResolutionMessage = `toolchain missing: toolchain "stale": not found`

	rt := &recordingPrewarmRuntime{}
	r, c := newReconcilerWithObjects(t, cls)
	r.Runtimes = sandboxkinds.Runtimes{fakePrewarmControllerKindName: rt}

	got := reconcileAndGet(t, r, c, cls.Name)

	assert.Empty(t, got.Status.ResolvedToolchains, "a class naming no toolchains must clear a stale ResolvedToolchains record")
	assert.Empty(t, got.Status.ToolchainSetDigest, "and a stale digest")
	assert.Empty(t, got.Status.ToolchainResolutionMessage,
		"and a stale resolution-failure message — the class no longer names any toolchain to fail on")

	calls := rt.calls()
	require.Len(t, calls, 1)
	assert.Empty(t, calls[0].Toolchains, "pooling proceeds with an empty toolchains list")
}
