//go:build integration

// Package idempotency_test proves the harness itself CATCHES both violation
// shapes it exists to gate: an SSA-applied payload that isn't a byte-stable
// no-op, and a reconciler that never reaches steady state. These fixtures
// are synthetic (a fake volatile ConfigMap, two toy reconcile.Reconcilers)
// so detection is proven without touching any production code.
package idempotency_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv/idempotency"
)

func TestMain(m *testing.M) { os.Exit(testenv.RunPackage(m)) }

const testFieldManager = "idempotency-harness-test"

// volatileConfigMap is a client.Object whose DeepCopyObject stamps a FRESH
// wall-clock annotation on every copy — modeling the "installedAt:
// time.Now()" class of bug (the oap-agent-install regression this harness
// exists to catch): the "same" desired-state construction actually produces
// a materially different payload on every apply attempt, even though from
// the caller's perspective it looks like "the same" object is being
// reapplied. CheckApplyIdempotent DeepCopies want before EACH of its two
// applies, so this override is exactly the seam that lets a synthetic
// fixture reproduce the bug without any production code involved.
//
// Built on unstructured.Unstructured (rather than a typed corev1.ConfigMap
// wrapper) so the overridden DeepCopyObject's return value still satisfies
// runtime.Unstructured — controller-runtime's client dispatches Patch/Get
// for such objects by the GVK carried in the object itself, not by a scheme
// lookup on the concrete Go type (which a bespoke wrapper type would fail).
type volatileConfigMap struct {
	unstructured.Unstructured
}

func newVolatileConfigMap(name string) *volatileConfigMap {
	v := &volatileConfigMap{}
	v.SetAPIVersion("v1")
	v.SetKind("ConfigMap")
	v.SetName(name)
	v.SetNamespace("default")
	return v
}

func (v *volatileConfigMap) DeepCopyObject() runtime.Object {
	cp := v.Unstructured.DeepCopy()
	ann := cp.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann["installedAt"] = strconv.FormatInt(time.Now().UnixNano(), 10)
	cp.SetAnnotations(ann)
	return &volatileConfigMap{Unstructured: *cp}
}

func TestCheckApplyIdempotent_DetectsVolatileAnnotation(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	want := newVolatileConfigMap("volatile-cm")
	err := idempotency.CheckApplyIdempotent(ctx, env.Client, want, testFieldManager)
	require.Error(t, err, "a payload that re-stamps a fresh wall-clock value on every DeepCopy must be flagged non-idempotent")
	assert.Contains(t, err.Error(), "NOT idempotent")
	assert.Contains(t, err.Error(), "resourceVersion changed")
	assert.Contains(t, err.Error(), "installedAt", "the diff should name the offending annotation")
}

func TestCheckApplyIdempotent_StablePayloadIsNoOp(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	want := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "stable-cm", Namespace: "default",
			Annotations: map[string]string{"fixed": "value"},
		},
	}
	// Exercise the require-based wrapper here (the violation-detecting test
	// above must use the error-returning core, since a passing
	// RequireApplyIdempotent call on a stable payload is exactly what a real
	// caller does).
	idempotency.RequireApplyIdempotent(t, ctx, env.Client, want, testFieldManager)
}

// stormReconciler patches a fresh wall-clock annotation onto the target
// ConfigMap on EVERY reconcile — modeling the guardian
// ObservedSchemaWrittenAt regression, where a controller restamped a value
// unconditionally instead of only on a meaningful state transition,
// producing an unbounded reconcile storm.
type stormReconciler struct {
	c client.Client
}

func (s *stormReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var cm corev1.ConfigMap
	if err := s.c.Get(ctx, req.NamespacedName, &cm); err != nil {
		return reconcile.Result{}, err
	}
	cp := cm.DeepCopy()
	if cp.Annotations == nil {
		cp.Annotations = map[string]string{}
	}
	cp.Annotations["observedAt"] = strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := s.c.Patch(ctx, cp, client.MergeFrom(&cm)); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

// convergingReconciler patches the annotation only ONCE — on the
// absent-value pass — and is a true no-op on every subsequent pass,
// modeling the post-fix guardian behavior (stamp only on a False->True
// transition, preserve the prior value otherwise).
type convergingReconciler struct {
	c client.Client
}

func (r *convergingReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var cm corev1.ConfigMap
	if err := r.c.Get(ctx, req.NamespacedName, &cm); err != nil {
		return reconcile.Result{}, err
	}
	if _, ok := cm.Annotations["observedAt"]; ok {
		return reconcile.Result{}, nil // already stamped: steady state, no write
	}
	cp := cm.DeepCopy()
	if cp.Annotations == nil {
		cp.Annotations = map[string]string{}
	}
	cp.Annotations["observedAt"] = strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := r.c.Patch(ctx, cp, client.MergeFrom(&cm)); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func TestCheckReconcileConverges_DetectsReconcileStorm(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "storm-cm", Namespace: "default"}}
	require.NoError(t, env.Client.Create(ctx, cm), "create target ConfigMap")

	r := &stormReconciler{c: env.Client}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "storm-cm", Namespace: "default"}}

	err := idempotency.CheckReconcileConverges(ctx, env.Client, r, req, &corev1.ConfigMap{}, 5)
	require.Error(t, err, "a reconciler that restamps a fresh value every pass must be flagged as a non-converging reconcile storm")
	assert.Contains(t, err.Error(), "did NOT converge")
	assert.Contains(t, err.Error(), "resourceVersion history")
}

func TestCheckReconcileConverges_ConvergesAfterFirstWrite(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "converge-cm", Namespace: "default"}}
	require.NoError(t, env.Client.Create(ctx, cm), "create target ConfigMap")

	r := &convergingReconciler{c: env.Client}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "converge-cm", Namespace: "default"}}

	// Exercise the require-based wrapper here — the storm test above needs
	// the error-returning core to assert on the failure, but a converging
	// reconciler is exactly the case a real caller uses Require* for.
	idempotency.RequireReconcileConverges(t, ctx, env.Client, r, req, &corev1.ConfigMap{}, 5)
}
