package sidecartoolbox_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/sidecartoolbox"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1.AddToScheme: %v", err)
	}
	if err := spiceboxv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("spicebox.AddToScheme: %v", err)
	}
	return s
}

func TestReconcile_Valid(t *testing.T) {
	scheme := newScheme(t)
	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "sidecar-sandbox-default"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image: "ignored",
			Resources: spiceboxv1alpha1.SpiceboxResources{
				CPU:              resource.MustParse("100m"),
				Memory:           resource.MustParse("128Mi"),
				EphemeralStorage: resource.MustParse("64Mi"),
			},
		},
		Status: spiceboxv1alpha1.SpiceboxClassStatus{
			Conditions: []metav1.Condition{{Type: "Valid", Status: metav1.ConditionTrue, Reason: "OK"}},
		},
	}
	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "echo",
			Namespace:  "default",
			Finalizers: []string{spiceboxv1alpha1.FinalizerSidecarToolbox},
		},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:    "echo",
			Version: "1",
			Source: spiceboxv1alpha1.SidecarToolboxSource{
				Image: "ghcr.io/example/echo-mcp:v1",
			},
			Sandbox:      spiceboxv1alpha1.SidecarToolboxSandbox{Class: "sidecar-sandbox-default"},
			Transport:    spiceboxv1alpha1.SidecarToolboxTransport{Port: 8080},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "github-pat"}, // any built-in provider id
			Tools:        []spiceboxv1alpha1.MCPServerTool{{Name: "echo"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{Client: c, SkipProbe: true}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "echo", Namespace: "default"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &spiceboxv1alpha1.SidecarToolbox{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "echo", Namespace: "default"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SidecarToolboxConditionValid)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected Valid=True, got %+v", cond)
	}
	if cond.Reason != spiceboxv1alpha1.ReasonSidecarToolboxSpecOK {
		t.Errorf("Reason=%q want %q", cond.Reason, spiceboxv1alpha1.ReasonSidecarToolboxSpecOK)
	}
}

// TestReconcile_UnchangedInputs_DoesNotRestampLastValidatedAt — the twin of the
// same defect in pkg/controllers/mcpserver. status.lastValidatedAt is an
// observation and must be written set-on-meaningful-change (AGENTS.md
// §Server-side apply); conditions.Set* already dedupes on equal state, so a
// fresh wall-clock stamp is the only per-pass churn, and For(&SidecarToolbox{})
// carries no predicate — the controller re-enqueues off its own write, and each
// re-entry is another probe Pod.
func TestReconcile_UnchangedInputs_DoesNotRestampLastValidatedAt(t *testing.T) {
	scheme := newScheme(t)
	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "sidecar-sandbox-default"},
		Status: spiceboxv1alpha1.SpiceboxClassStatus{
			Conditions: []metav1.Condition{{Type: "Valid", Status: metav1.ConditionTrue, Reason: "OK"}},
		},
	}
	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "echo",
			Namespace:  "default",
			Finalizers: []string{spiceboxv1alpha1.FinalizerSidecarToolbox},
		},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:         "echo",
			Version:      "1",
			Source:       spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/example/echo-mcp:v1"},
			Sandbox:      spiceboxv1alpha1.SidecarToolboxSandbox{Class: "sidecar-sandbox-default"},
			Transport:    spiceboxv1alpha1.SidecarToolboxTransport{Port: 8080},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "github-pat"},
			Tools:        []spiceboxv1alpha1.MCPServerTool{{Name: "echo"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{Client: c, SkipProbe: true}
	key := types.NamespacedName{Name: "echo", Namespace: "default"}

	reconcile := func() *metav1.Time {
		t.Helper()
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		require.NoError(t, err, "Reconcile")
		got := &spiceboxv1alpha1.SidecarToolbox{}
		require.NoError(t, c.Get(context.Background(), key, got), "Get SidecarToolbox")
		require.NotNil(t, got.Status.LastValidatedAt, "LastValidatedAt must be stamped")
		return got.Status.LastValidatedAt
	}

	first := reconcile()
	// metav1.Time persists at whole-second granularity, so a same-second
	// re-stamp is invisible; cross a boundary to make the churn observable.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second + 20*time.Millisecond)))
	second := reconcile()

	assert.True(t, first.Equal(second),
		"nothing changed between passes; re-stamping makes every no-op reconcile a real write that re-triggers this controller's own watch")
}

func TestReconcile_ClassMissing(t *testing.T) {
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "echo",
			Namespace:  "default",
			Finalizers: []string{spiceboxv1alpha1.FinalizerSidecarToolbox},
		},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:         "echo",
			Version:      "1",
			Source:       spiceboxv1alpha1.SidecarToolboxSource{Image: "x:1"},
			Sandbox:      spiceboxv1alpha1.SidecarToolboxSandbox{Class: "missing"},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "github-pat"},
			Tools:        []spiceboxv1alpha1.MCPServerTool{{Name: "echo"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{Client: c, SkipProbe: true}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "echo", Namespace: "default"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &spiceboxv1alpha1.SidecarToolbox{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "echo", Namespace: "default"}, got)
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SidecarToolboxConditionValid)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != spiceboxv1alpha1.ReasonSidecarToolboxClassMissing {
		t.Fatalf("expected Valid=False ClassMissing, got %+v", cond)
	}
}

func TestReconcile_ProviderMissing(t *testing.T) {
	scheme := newScheme(t)
	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ok"},
		Status: spiceboxv1alpha1.SpiceboxClassStatus{
			Conditions: []metav1.Condition{{Type: "Valid", Status: metav1.ConditionTrue}},
		},
	}
	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "echo",
			Namespace:  "default",
			Finalizers: []string{spiceboxv1alpha1.FinalizerSidecarToolbox},
		},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:         "echo",
			Version:      "1",
			Source:       spiceboxv1alpha1.SidecarToolboxSource{Image: "x:1"},
			Sandbox:      spiceboxv1alpha1.SidecarToolboxSandbox{Class: "ok"},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "does-not-exist"},
			Tools:        []spiceboxv1alpha1.MCPServerTool{{Name: "echo"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{Client: c, SkipProbe: true}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "echo", Namespace: "default"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &spiceboxv1alpha1.SidecarToolbox{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "echo", Namespace: "default"}, got)
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SidecarToolboxConditionValid)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != spiceboxv1alpha1.ReasonSidecarToolboxProviderMissing {
		t.Fatalf("expected Valid=False ProviderMissing, got %+v", cond)
	}
}

// TestReconcile_ProviderNone_ValidTrue proves the UpstreamAuthProviderNone
// sentinel — a controller-issued-token sidecar with no AgentIdentity
// credential to resolve (both shipped workshop manifests set
// upstreamAuth.provider: none) — reaches Valid=True. providerCheck must
// treat the sentinel as satisfied without looking it up in the
// /providers/ library, which has no "none" entry and never should.
func TestReconcile_ProviderNone_ValidTrue(t *testing.T) {
	scheme := newScheme(t)
	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "sidecar-sandbox-default"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image: "ignored",
			Resources: spiceboxv1alpha1.SpiceboxResources{
				CPU:              resource.MustParse("100m"),
				Memory:           resource.MustParse("128Mi"),
				EphemeralStorage: resource.MustParse("64Mi"),
			},
		},
		Status: spiceboxv1alpha1.SpiceboxClassStatus{
			Conditions: []metav1.Condition{{Type: "Valid", Status: metav1.ConditionTrue, Reason: "OK"}},
		},
	}
	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "workshop",
			Namespace:  "default",
			Finalizers: []string{spiceboxv1alpha1.FinalizerSidecarToolbox},
		},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:    "workshop",
			Version: "1",
			Source: spiceboxv1alpha1.SidecarToolboxSource{
				Image: "ap-workshop:dev",
			},
			Sandbox:      spiceboxv1alpha1.SidecarToolboxSandbox{Class: "sidecar-sandbox-default"},
			Transport:    spiceboxv1alpha1.SidecarToolboxTransport{Port: 8080},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: spiceboxv1alpha1.UpstreamAuthProviderNone},
			Tools:        []spiceboxv1alpha1.MCPServerTool{{Name: "inventory"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{Client: c, SkipProbe: true}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "workshop", Namespace: "default"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &spiceboxv1alpha1.SidecarToolbox{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "workshop", Namespace: "default"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SidecarToolboxConditionValid)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf(`expected Valid=True for provider: "none", got %+v`, cond)
	}
	if cond.Reason != spiceboxv1alpha1.ReasonSidecarToolboxSpecOK {
		t.Errorf("Reason=%q want %q", cond.Reason, spiceboxv1alpha1.ReasonSidecarToolboxSpecOK)
	}
}

// TestReconcile_ProviderNoneWithEnvVar_Refused pins the one combination that
// admitting the sentinel newly made reachable. Nothing ever resolves a library
// credential for a controller-issued-token sidecar, so an envVar names a
// variable no credential is injected into; left admitted, it clears this
// controller and then dead-ends at session boot in resolveSidecarCredential,
// whose failure tells the operator to run `oap agent setup-identity` — advice
// that structurally cannot fix a "none" provider. Refusing it here keeps the
// contradiction where its author can still act on it. Before providerCheck
// accepted the sentinel at all this pairing was unreachable; it is reachable
// now, so it needs its own guard.
func TestReconcile_ProviderNoneWithEnvVar_Refused(t *testing.T) {
	scheme := newScheme(t)
	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ok"},
		Status: spiceboxv1alpha1.SpiceboxClassStatus{
			Conditions: []metav1.Condition{{Type: "Valid", Status: metav1.ConditionTrue}},
		},
	}
	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "contradictory",
			Namespace:  "default",
			Finalizers: []string{spiceboxv1alpha1.FinalizerSidecarToolbox},
		},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:    "contradictory",
			Version: "1",
			Source:  spiceboxv1alpha1.SidecarToolboxSource{Image: "x:1"},
			Sandbox: spiceboxv1alpha1.SidecarToolboxSandbox{Class: "ok"},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{
				Provider: spiceboxv1alpha1.UpstreamAuthProviderNone,
				EnvVar:   "UPSTREAM_TOKEN",
			},
			Tools: []spiceboxv1alpha1.MCPServerTool{{Name: "echo"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{Client: c, SkipProbe: true}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "contradictory", Namespace: "default"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &spiceboxv1alpha1.SidecarToolbox{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "contradictory", Namespace: "default"}, got)
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SidecarToolboxConditionValid)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf(`expected Valid=False for provider "none" + envVar, got %+v`, cond)
	}
	if !strings.Contains(cond.Message, "upstreamAuth.envVar must be empty") {
		t.Errorf("message should name the contradiction, got %q", cond.Message)
	}
}

// TestReconcile_SecretGated_ValidAndReachableDeferred_NoProbe proves the
// redesign's core: a secret-gated (SecretInputs != nil ⇒ separate-pod) sidecar
// reaches Valid=True WITHOUT the operator ever probing it, and its Reachable
// condition is Unknown/DeferredToSession (a distinct non-failure state), because
// reachability is the runner's job against the live per-session pod. The
// ProbeFunc is wired to FAIL if invoked and SkipProbe is deliberately left false,
// so a regression that probes secret-gated sidecars (the old netpol-blocked,
// secret-less behavior) is caught here.
func TestReconcile_SecretGated_ValidAndReachableDeferred_NoProbe(t *testing.T) {
	scheme := newScheme(t)
	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ok"},
		Status: spiceboxv1alpha1.SpiceboxClassStatus{
			Conditions: []metav1.Condition{{Type: "Valid", Status: metav1.ConditionTrue}},
		},
	}
	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "kube-tb",
			Namespace:  "default",
			Finalizers: []string{spiceboxv1alpha1.FinalizerSidecarToolbox},
		},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:         "kube",
			Version:      "1",
			Source:       spiceboxv1alpha1.SidecarToolboxSource{Image: "x:1"},
			Sandbox:      spiceboxv1alpha1.SidecarToolboxSandbox{Class: "ok"},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "github-pat"},
			Tools:        []spiceboxv1alpha1.MCPServerTool{{Name: "whoami"}},
			// SecretInputs is what makes this secret-gated (separate-pod).
			SecretInputs: []spiceboxv1alpha1.SidecarToolboxSecretInput{
				{Name: "kubeconfig", Deliver: "file:/var/run/sre/kubeconfig", From: "kubeconfig"},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()

	probeCalled := false
	r := &sidecartoolbox.Reconciler{
		Client: c,
		// NOT SkipProbe: prove the secret-gated branch (not SkipProbe) is what
		// bypasses the probe. If the operator ever probes a secret-gated sidecar,
		// this ProbeFunc runs and fails the assertion below.
		ProbeFunc: func(context.Context, *spiceboxv1alpha1.SidecarToolbox) (sidecartoolbox.ProbeResult, error) {
			probeCalled = true
			return sidecartoolbox.ProbeResult{}, fmt.Errorf("secret-gated sidecar must not be probed by the operator")
		},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "kube-tb", Namespace: "default"}})
	require.NoError(t, err, "Reconcile must succeed for a secret-gated sidecar")

	got := &spiceboxv1alpha1.SidecarToolbox{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "kube-tb", Namespace: "default"}, got), "get after reconcile")

	assert.False(t, probeCalled, "the operator must NOT probe a secret-gated sidecar")

	valid := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SidecarToolboxConditionValid)
	require.NotNil(t, valid, "Valid condition must be set")
	assert.Equal(t, metav1.ConditionTrue, valid.Status, "secret-gated sidecar validates structurally")
	assert.Equal(t, spiceboxv1alpha1.ReasonSidecarToolboxSpecOK, valid.Reason)

	reach := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SidecarToolboxConditionReachable)
	require.NotNil(t, reach, "Reachable condition must be set (deferred, not absent)")
	assert.Equal(t, metav1.ConditionUnknown, reach.Status, "reachability is deferred, not failed")
	assert.Equal(t, spiceboxv1alpha1.ReasonSidecarToolboxDeferredToSession, reach.Reason)
}

func TestReconcile_CELCompileError(t *testing.T) {
	scheme := newScheme(t)
	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ok"},
		Status: spiceboxv1alpha1.SpiceboxClassStatus{
			Conditions: []metav1.Condition{{Type: "Valid", Status: metav1.ConditionTrue}},
		},
	}
	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "echo",
			Namespace:  "default",
			Finalizers: []string{spiceboxv1alpha1.FinalizerSidecarToolbox},
		},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:         "echo",
			Version:      "1",
			Source:       spiceboxv1alpha1.SidecarToolboxSource{Image: "x:1"},
			Sandbox:      spiceboxv1alpha1.SidecarToolboxSandbox{Class: "ok"},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "github-pat"},
			Tools: []spiceboxv1alpha1.MCPServerTool{{
				Name: "echo",
				Args: spiceboxv1alpha1.MCPServerToolArgs{
					Constraints: []spiceboxv1alpha1.MCPServerConstraint{{CEL: "this is not valid CEL"}},
				},
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{Client: c, SkipProbe: true}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "echo", Namespace: "default"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &spiceboxv1alpha1.SidecarToolbox{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "echo", Namespace: "default"}, got)
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SidecarToolboxConditionValid)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != spiceboxv1alpha1.ReasonSidecarToolboxConstraintCompileError {
		t.Fatalf("expected Valid=False ConstraintCompileError, got %+v", cond)
	}
}

func TestReconcile_AddsFinalizerOnFirstReconcile(t *testing.T) {
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.SidecarToolbox{
		// No finalizer in the fixture — the reconciler must add one and requeue.
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source:       spiceboxv1alpha1.SidecarToolboxSource{Image: "x:1"},
			Sandbox:      spiceboxv1alpha1.SidecarToolboxSandbox{Class: "missing"},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "github-pat"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{Client: c, SkipProbe: true}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "echo", Namespace: "default"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !res.Requeue {
		t.Errorf("expected Requeue after finalizer add, got %+v", res)
	}
	got := &spiceboxv1alpha1.SidecarToolbox{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "echo", Namespace: "default"}, got)
	hasFinalizer := false
	for _, f := range got.Finalizers {
		if f == spiceboxv1alpha1.FinalizerSidecarToolbox {
			hasFinalizer = true
			break
		}
	}
	if !hasFinalizer {
		t.Errorf("expected finalizer %q on CR, got %v", spiceboxv1alpha1.FinalizerSidecarToolbox, got.Finalizers)
	}
}

func TestReconcile_DeletionBlockedWhileAgentClassReferences(t *testing.T) {
	scheme := newScheme(t)
	now := metav1.Now()
	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "echo",
			Namespace:         "default",
			Finalizers:        []string{spiceboxv1alpha1.FinalizerSidecarToolbox},
			DeletionTimestamp: &now,
		},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source:       spiceboxv1alpha1.SidecarToolboxSource{Image: "x:1"},
			Sandbox:      spiceboxv1alpha1.SidecarToolboxSandbox{Class: "ok"},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "github-pat"},
		},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SidecarToolboxes: []spiceboxv1alpha1.AgentClassSidecarToolboxRef{
				{Name: "echo", Ref: "echo"},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, ac).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{Client: c, SkipProbe: true}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "echo", Namespace: "default"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected RequeueAfter when delete is blocked, got %+v", res)
	}
	got := &spiceboxv1alpha1.SidecarToolbox{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "echo", Namespace: "default"}, got)
	hasFinalizer := false
	for _, f := range got.Finalizers {
		if f == spiceboxv1alpha1.FinalizerSidecarToolbox {
			hasFinalizer = true
			break
		}
	}
	if !hasFinalizer {
		t.Errorf("finalizer must remain while AgentClass references the CR")
	}
}

func TestReconcile_DeletionUnblockedWhenNoReferences(t *testing.T) {
	scheme := newScheme(t)
	now := metav1.Now()
	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "echo",
			Namespace:         "default",
			Finalizers:        []string{spiceboxv1alpha1.FinalizerSidecarToolbox},
			DeletionTimestamp: &now,
		},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source:       spiceboxv1alpha1.SidecarToolboxSource{Image: "x:1"},
			Sandbox:      spiceboxv1alpha1.SidecarToolboxSandbox{Class: "ok"},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "github-pat"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{Client: c, SkipProbe: true}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "echo", Namespace: "default"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &spiceboxv1alpha1.SidecarToolbox{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "echo", Namespace: "default"}, got); err == nil {
		// Fake client may keep the object after RemoveFinalizer; the
		// canonical signal is the absence of the finalizer.
		for _, f := range got.Finalizers {
			if f == spiceboxv1alpha1.FinalizerSidecarToolbox {
				t.Errorf("finalizer should be removed when no references; got %v", got.Finalizers)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// pinCheck phase wiring tests (using ProbeFunc injection seam)
// ---------------------------------------------------------------------------

const (
	pinDigest1 = "sha256:aaaa0000111122223333444455556666aaaabbbbccccdddd0000111122223333"
	pinDigest2 = "sha256:bbbb0000111122223333444455556666aaaabbbbccccdddd0000111122223333"
)

// validCRForPin builds a fully-valid SidecarToolbox fixture suitable for the
// pinCheck phase tests. It includes a valid SpiceboxClass and all required
// spec fields.
func validCRForPin(name string, imageRef string) (*spiceboxv1alpha1.SpiceboxClass, *spiceboxv1alpha1.SidecarToolbox) {
	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image: "ignored",
			Resources: spiceboxv1alpha1.SpiceboxResources{
				CPU:              resource.MustParse("100m"),
				Memory:           resource.MustParse("128Mi"),
				EphemeralStorage: resource.MustParse("64Mi"),
			},
		},
		Status: spiceboxv1alpha1.SpiceboxClassStatus{
			Conditions: []metav1.Condition{{Type: "Valid", Status: metav1.ConditionTrue, Reason: "OK"}},
		},
	}
	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "default",
			Finalizers: []string{spiceboxv1alpha1.FinalizerSidecarToolbox},
		},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:         name,
			Version:      "1",
			Source:       spiceboxv1alpha1.SidecarToolboxSource{Image: imageRef},
			Sandbox:      spiceboxv1alpha1.SidecarToolboxSandbox{Class: "cls"},
			Transport:    spiceboxv1alpha1.SidecarToolboxTransport{Port: 8080},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "github-pat"},
			Tools:        []spiceboxv1alpha1.MCPServerTool{{Name: "echo"}},
		},
	}
	return cls, cr
}

// makeProbeFunc returns a ProbeFunc that always succeeds with the given tools
// and resolved digest.
func makeProbeFunc(digest string) func(ctx context.Context, cr *spiceboxv1alpha1.SidecarToolbox) (sidecartoolbox.ProbeResult, error) {
	return func(_ context.Context, _ *spiceboxv1alpha1.SidecarToolbox) (sidecartoolbox.ProbeResult, error) {
		return sidecartoolbox.ProbeResult{
			Tools:  []probe.Tool{{Name: "echo"}},
			Digest: digest,
		}, nil
	}
}

func reconcileNN(t *testing.T, r *sidecartoolbox.Reconciler, ns, name string) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: name},
	})
	require.NoError(t, err)
}

func getCR(t *testing.T, c client.Client, ns, name string) *spiceboxv1alpha1.SidecarToolbox {
	t.Helper()
	got := &spiceboxv1alpha1.SidecarToolbox{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: name}, got))
	return got
}

func TestPinCheck_TOFUFirstObservation_WritesBaseline(t *testing.T) {
	// First probe: no prior baseline → TOFU record written, PinDrift=True/PinMatch.
	scheme := newScheme(t)
	cls, cr := validCRForPin("echo", "ghcr.io/example/echo-mcp:v1")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{
		Client:    c,
		ProbeFunc: makeProbeFunc(pinDigest1),
	}

	reconcileNN(t, r, "default", "echo")

	got := getCR(t, c, "default", "echo")
	require.NotNil(t, got.Status.Pin, "pin baseline must be written on first probe")
	assert.Equal(t, "image", got.Status.Pin.Kind)
	assert.Equal(t, "named", got.Status.Pin.Strength)
	assert.Equal(t, pinDigest1, got.Status.Pin.Digest)
	assert.Equal(t, "v1", got.Status.Pin.Version)
	assert.NotNil(t, got.Status.Pin.ObservedAt)

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonPinMatch, cond.Reason)
}

func TestPinCheck_UnchangedDigest_PreservesObservedAt(t *testing.T) {
	// Second probe with same digest: ObservedAt must not change.
	scheme := newScheme(t)
	originalTime := metav1.NewTime(metav1.Now().Add(-10 * 60 * 1e9)) // 10 min ago
	cls, cr := validCRForPin("echo", "ghcr.io/example/echo-mcp:v1")
	cr.Status.Pin = &spiceboxv1alpha1.PinRecord{
		Kind:       "image",
		Strength:   "named",
		Digest:     pinDigest1,
		Version:    "v1",
		ObservedAt: &originalTime,
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{
		Client:    c,
		ProbeFunc: makeProbeFunc(pinDigest1),
	}

	reconcileNN(t, r, "default", "echo")

	got := getCR(t, c, "default", "echo")
	require.NotNil(t, got.Status.Pin)
	assert.Equal(t, pinDigest1, got.Status.Pin.Digest)
	// metav1.Time is serialized to second-precision by the fake client; compare
	// Unix seconds so the nanosecond rounding doesn't cause a spurious failure.
	assert.Equal(t, originalTime.Unix(), got.Status.Pin.ObservedAt.Unix(),
		"ObservedAt must be preserved when digest is unchanged")

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonPinMatch, cond.Reason)
}

func TestPinCheck_DigestChange_SetsDrifted(t *testing.T) {
	// Digest changes → PinDrift=False/PinDrifted; baseline not auto-updated.
	scheme := newScheme(t)
	cls, cr := validCRForPin("echo", "ghcr.io/example/echo-mcp:v1")
	originalTime := metav1.Now()
	cr.Status.Pin = &spiceboxv1alpha1.PinRecord{
		Kind:       "image",
		Strength:   "named",
		Digest:     pinDigest1,
		Version:    "v1",
		ObservedAt: &originalTime,
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{
		Client:    c,
		ProbeFunc: makeProbeFunc(pinDigest2), // new digest
	}

	reconcileNN(t, r, "default", "echo")

	got := getCR(t, c, "default", "echo")
	// Baseline must NOT be auto-updated on drift.
	require.NotNil(t, got.Status.Pin)
	assert.Equal(t, pinDigest1, got.Status.Pin.Digest, "baseline must not change on drift")

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonPinDrifted, cond.Reason)
	assert.Contains(t, cond.Message, "pin the source image by digest", "accept-hint must appear in message")
}

func TestPinCheck_FrozenDeclaredRefMismatch_SetsDrifted(t *testing.T) {
	// Frozen declared ref: probe returns different digest → PinDrift=False/PinDrifted.
	scheme := newScheme(t)
	frozenRef := "ghcr.io/example/echo-mcp@" + pinDigest1
	cls, cr := validCRForPin("echo", frozenRef)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{
		Client:    c,
		ProbeFunc: makeProbeFunc(pinDigest2), // mismatches declared digest
	}

	reconcileNN(t, r, "default", "echo")

	got := getCR(t, c, "default", "echo")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonPinDrifted, cond.Reason)
}

func TestPinCheck_SkipProbe_NoPinDriftCondition(t *testing.T) {
	// SkipProbe=true: pinCheck is fully skipped; no PinDrift condition written.
	scheme := newScheme(t)
	cls, cr := validCRForPin("echo", "ghcr.io/example/echo-mcp:v1")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{Client: c, SkipProbe: true}

	reconcileNN(t, r, "default", "echo")

	got := getCR(t, c, "default", "echo")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
	assert.Nil(t, cond, "PinDrift condition must not be set when SkipProbe=true")
}

func TestPinCheck_InlineBaseImage_WritesBaseline(t *testing.T) {
	// Inline source: BaseImage is used as the declared ref.
	scheme := newScheme(t)
	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image: "ignored",
			Resources: spiceboxv1alpha1.SpiceboxResources{
				CPU:              resource.MustParse("100m"),
				Memory:           resource.MustParse("128Mi"),
				EphemeralStorage: resource.MustParse("64Mi"),
			},
		},
		Status: spiceboxv1alpha1.SpiceboxClassStatus{
			Conditions: []metav1.Condition{{Type: "Valid", Status: metav1.ConditionTrue, Reason: "OK"}},
		},
	}
	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "inline",
			Namespace:  "default",
			Finalizers: []string{spiceboxv1alpha1.FinalizerSidecarToolbox},
		},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:    "inline",
			Version: "1",
			Source: spiceboxv1alpha1.SidecarToolboxSource{
				Inline: &spiceboxv1alpha1.SidecarToolboxInlineSource{
					BaseImage: "python:3.12-slim",
					Script: spiceboxv1alpha1.SidecarToolboxScriptSource{
						ConfigMapRef: spiceboxv1alpha1.SidecarToolboxConfigMapKeyRef{Name: "src", Key: "server.py"},
					},
					Entrypoint: []string{"python", "/app/server.py"},
				},
			},
			Sandbox:      spiceboxv1alpha1.SidecarToolboxSandbox{Class: "cls"},
			Transport:    spiceboxv1alpha1.SidecarToolboxTransport{Port: 8080},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "github-pat"},
			Tools:        []spiceboxv1alpha1.MCPServerTool{{Name: "echo"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{
		Client:    c,
		ProbeFunc: makeProbeFunc(pinDigest1),
	}

	reconcileNN(t, r, "default", "inline")

	got := getCR(t, c, "default", "inline")
	require.NotNil(t, got.Status.Pin, "pin baseline must be written for inline source")
	assert.Equal(t, pinDigest1, got.Status.Pin.Digest)
	assert.Equal(t, "python:3.12-slim", got.Status.Pin.Details["declaredRef"])
}

// makeErrProbeFunc returns a ProbeFunc that always fails with the given error.
func makeErrProbeFunc(err error) func(ctx context.Context, cr *spiceboxv1alpha1.SidecarToolbox) (sidecartoolbox.ProbeResult, error) {
	return func(_ context.Context, _ *spiceboxv1alpha1.SidecarToolbox) (sidecartoolbox.ProbeResult, error) {
		return sidecartoolbox.ProbeResult{}, err
	}
}

func TestPinCheck_ProbeFailure_WithBaseline_SetsPinVerifyFailed(t *testing.T) {
	// Probe fails on a toolbox that already has a recorded baseline.
	// Expected: Reachable=False/ProbeFailed AND PinDrift=False/PinVerifyFailed;
	// Valid condition NOT set (probeGate stops the chain before finalize).
	scheme := newScheme(t)
	originalTime := metav1.Now()
	cls, cr := validCRForPin("echo", "ghcr.io/example/echo-mcp:v1")
	cr.Status.Pin = &spiceboxv1alpha1.PinRecord{
		Kind:       "image",
		Strength:   "named",
		Digest:     pinDigest1,
		Version:    "v1",
		ObservedAt: &originalTime,
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{
		Client:    c,
		ProbeFunc: makeErrProbeFunc(fmt.Errorf("image pull backoff")),
	}

	reconcileNN(t, r, "default", "echo")

	got := getCR(t, c, "default", "echo")

	reachable := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SidecarToolboxConditionReachable)
	require.NotNil(t, reachable)
	assert.Equal(t, metav1.ConditionFalse, reachable.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonSidecarToolboxProbeFailed, reachable.Reason)

	pinDrift := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
	require.NotNil(t, pinDrift, "PinDrift condition must be set when probe fails with an existing baseline")
	assert.Equal(t, metav1.ConditionFalse, pinDrift.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonPinVerifyFailed, pinDrift.Reason)

	// Baseline must be preserved unchanged — probe failure must not wipe or update it.
	require.NotNil(t, got.Status.Pin)
	assert.Equal(t, pinDigest1, got.Status.Pin.Digest)

	// probeGate stops the chain before finalize, so Valid is not set to True.
	valid := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SidecarToolboxConditionValid)
	assert.Nil(t, valid, "Valid condition must not be set when probe fails (finalize must not run)")
}

func TestPinCheck_ProbeFailure_NoBaseline_NoPinDriftCondition(t *testing.T) {
	// Probe fails on a fresh toolbox with no prior baseline.
	// Expected: Reachable=False/ProbeFailed; PinDrift NOT set (TOFU — nothing to compare).
	scheme := newScheme(t)
	cls, cr := validCRForPin("echo", "ghcr.io/example/echo-mcp:v1")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{
		Client:    c,
		ProbeFunc: makeErrProbeFunc(fmt.Errorf("connection refused")),
	}

	reconcileNN(t, r, "default", "echo")

	got := getCR(t, c, "default", "echo")

	reachable := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SidecarToolboxConditionReachable)
	require.NotNil(t, reachable)
	assert.Equal(t, metav1.ConditionFalse, reachable.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonSidecarToolboxProbeFailed, reachable.Reason)

	pinDrift := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
	assert.Nil(t, pinDrift, "PinDrift must not be set when probe fails with no prior baseline (TOFU)")
}

func TestPinCheck_EmptyDigest_WithBaseline_SetsPinVerifyFailed(t *testing.T) {
	// Probe succeeds but returns no image digest (kubelet status lag).
	// The toolbox has an existing baseline → PinVerifyFailed.
	scheme := newScheme(t)
	originalTime := metav1.Now()
	cls, cr := validCRForPin("echo", "ghcr.io/example/echo-mcp:v1")
	cr.Status.Pin = &spiceboxv1alpha1.PinRecord{
		Kind:       "image",
		Strength:   "named",
		Digest:     pinDigest1,
		Version:    "v1",
		ObservedAt: &originalTime,
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{
		Client:    c,
		ProbeFunc: makeProbeFunc(""), // probe succeeds, digest empty
	}

	reconcileNN(t, r, "default", "echo")

	got := getCR(t, c, "default", "echo")

	pinDrift := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
	require.NotNil(t, pinDrift, "PinDrift must be set when digest is missing and baseline exists")
	assert.Equal(t, metav1.ConditionFalse, pinDrift.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonPinVerifyFailed, pinDrift.Reason)
	assert.Contains(t, pinDrift.Message, "no image digest")
}

func TestPinCheck_EmptyDigest_NoBaseline_NoPinDriftCondition(t *testing.T) {
	// Probe succeeds but returns no image digest; no prior baseline.
	// Expected: PinDrift condition NOT set (no assertion to fail, TOFU).
	scheme := newScheme(t)
	cls, cr := validCRForPin("echo", "ghcr.io/example/echo-mcp:v1")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{
		Client:    c,
		ProbeFunc: makeProbeFunc(""), // probe succeeds, digest empty
	}

	reconcileNN(t, r, "default", "echo")

	got := getCR(t, c, "default", "echo")

	pinDrift := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
	assert.Nil(t, pinDrift, "PinDrift must not be set when digest is missing and there is no baseline")
}

func TestPinCheck_EmptyDigest_FrozenRef_SetsPinVerifyFailed(t *testing.T) {
	// Probe succeeds but returns no image digest; declared ref is frozen (digest-pinned).
	// A frozen ref is itself an assertion → PinVerifyFailed even without a prior baseline.
	scheme := newScheme(t)
	frozenRef := "ghcr.io/example/echo-mcp@" + pinDigest1
	cls, cr := validCRForPin("echo", frozenRef)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{
		Client:    c,
		ProbeFunc: makeProbeFunc(""), // probe succeeds, digest empty
	}

	reconcileNN(t, r, "default", "echo")

	got := getCR(t, c, "default", "echo")

	pinDrift := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
	require.NotNil(t, pinDrift, "PinDrift must be set when digest is missing and ref is frozen")
	assert.Equal(t, metav1.ConditionFalse, pinDrift.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonPinVerifyFailed, pinDrift.Reason)
	assert.Contains(t, pinDrift.Message, "no image digest")
}

// ---------------------------------------------------------------------------
// pinCheck refreeze-annotation tests (Plan 4 Task 1)
// ---------------------------------------------------------------------------

// patchAnnotation updates the CR's annotations on the fake client and returns
// the refreshed CR so callers can reconcile against the current ResourceVersion.
func patchAnnotation(t *testing.T, c client.Client, ns, name, key, val string) {
	t.Helper()
	cr := getCR(t, c, ns, name)
	if cr.Annotations == nil {
		cr.Annotations = map[string]string{}
	}
	cr.Annotations[key] = val
	require.NoError(t, c.Update(context.Background(), cr), "patch annotation")
}

func TestPinRefreeze_Honored_BaselineRewritten_AnnotationCleared_PinMatch(t *testing.T) {
	// First reconcile: establish a drifted state (baseline=digest1, live=digest2).
	// Second reconcile: annotation == liveHash (digest2) → honored: baseline rewritten,
	// annotation cleared, PinMatch set.
	scheme := newScheme(t)
	originalTime := metav1.Now()
	cls, cr := validCRForPin("echo", "ghcr.io/example/echo-mcp:v1")
	cr.Status.Pin = &spiceboxv1alpha1.PinRecord{
		Kind:       "image",
		Strength:   "named",
		Digest:     pinDigest1,
		Version:    "v1",
		ObservedAt: &originalTime,
		Details:    map[string]string{"declaredRef": "ghcr.io/example/echo-mcp:v1"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()

	// Second reconcile with annotation matching liveHash.
	patchAnnotation(t, c, "default", "echo", spiceboxv1alpha1.AnnotationPinRefreeze, pinDigest2)
	r := &sidecartoolbox.Reconciler{
		Client:    c,
		ProbeFunc: makeProbeFunc(pinDigest2), // live is digest2
	}
	reconcileNN(t, r, "default", "echo")

	got := getCR(t, c, "default", "echo")

	// Annotation must be cleared.
	_, hasAnnotation := got.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze]
	assert.False(t, hasAnnotation, "annotation must be cleared after honoring")

	// Baseline must be rewritten to the new digest.
	require.NotNil(t, got.Status.Pin)
	assert.Equal(t, pinDigest2, got.Status.Pin.Digest, "baseline must be rewritten to live hash")
	require.NotNil(t, got.Status.Pin.ObservedAt, "ObservedAt must be stamped")

	// PinDrift must be True/PinMatch.
	pinDrift := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
	require.NotNil(t, pinDrift)
	assert.Equal(t, metav1.ConditionTrue, pinDrift.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonPinMatch, pinDrift.Reason)
}

func TestPinRefreeze_Stale_DriftStays_MessageNotesFailedRefreeze(t *testing.T) {
	// Baseline=digest1. Live moves to digest2. Annotation set to digest1 (stale:
	// points at old hash, not current live). Drift must remain; message notes
	// the failed refreeze.
	scheme := newScheme(t)
	originalTime := metav1.Now()
	cls, cr := validCRForPin("echo", "ghcr.io/example/echo-mcp:v1")
	cr.Status.Pin = &spiceboxv1alpha1.PinRecord{
		Kind:       "image",
		Strength:   "named",
		Digest:     pinDigest1,
		Version:    "v1",
		ObservedAt: &originalTime,
		Details:    map[string]string{"declaredRef": "ghcr.io/example/echo-mcp:v1"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()

	// Set annotation to digest1 (stale); live is digest2.
	patchAnnotation(t, c, "default", "echo", spiceboxv1alpha1.AnnotationPinRefreeze, pinDigest1)
	r := &sidecartoolbox.Reconciler{
		Client:    c,
		ProbeFunc: makeProbeFunc(pinDigest2),
	}
	reconcileNN(t, r, "default", "echo")

	got := getCR(t, c, "default", "echo")

	// Annotation must NOT be cleared (stale refreeze).
	annotationVal, hasAnnotation := got.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze]
	assert.True(t, hasAnnotation, "stale annotation must remain")
	assert.Equal(t, pinDigest1, annotationVal)

	// Baseline must be unchanged.
	require.NotNil(t, got.Status.Pin)
	assert.Equal(t, pinDigest1, got.Status.Pin.Digest, "baseline must not change on stale refreeze")

	// PinDrift must be False/PinDrifted with the refreeze note appended.
	pinDrift := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
	require.NotNil(t, pinDrift)
	assert.Equal(t, metav1.ConditionFalse, pinDrift.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonPinDrifted, pinDrift.Reason)
	assert.Contains(t, pinDrift.Message, "refreeze to", "message must mention failed refreeze")
	assert.Contains(t, pinDrift.Message, pinDigest1, "message must include stale annotation value")
	assert.Contains(t, pinDrift.Message, pinDigest2, "message must include live digest")
}

func TestPinRefreeze_VsSpecAssertion_Rejected_AnnotationStays(t *testing.T) {
	// Declared ref is frozen (by digest). Annotation == probedDigest != declaredDigest.
	// Spec assertion (frozen ref) wins; annotation must remain; drift message explains.
	scheme := newScheme(t)
	frozenRef := "ghcr.io/example/echo-mcp@" + pinDigest1
	cls, cr := validCRForPin("echo", frozenRef)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()

	// Annotation matches live (digest2) but frozen ref asserts digest1.
	patchAnnotation(t, c, "default", "echo", spiceboxv1alpha1.AnnotationPinRefreeze, pinDigest2)
	r := &sidecartoolbox.Reconciler{
		Client:    c,
		ProbeFunc: makeProbeFunc(pinDigest2), // live is digest2
	}
	reconcileNN(t, r, "default", "echo")

	got := getCR(t, c, "default", "echo")

	// Annotation must NOT be cleared (rejected; user still sees it pending).
	_, hasAnnotation := got.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze]
	assert.True(t, hasAnnotation, "rejected refreeze annotation must remain")

	// PinDrift must be False/PinDrifted; message must explain the assertion wins.
	pinDrift := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
	require.NotNil(t, pinDrift)
	assert.Equal(t, metav1.ConditionFalse, pinDrift.Status)
	assert.Contains(t, pinDrift.Message, "assertion", "message must explain assertion wins")
}

func TestPinRefreeze_NonDrifted_AnnotationEqualsLive_ClearsAnnotation_PinMatch(t *testing.T) {
	// No drift: baseline=digest1, live=digest1. Annotation is also digest1
	// (operator re-runs the refreeze for a resource that was never drifted).
	// Annotation must be cleared; PinMatch preserved.
	scheme := newScheme(t)
	originalTime := metav1.Now()
	cls, cr := validCRForPin("echo", "ghcr.io/example/echo-mcp:v1")
	cr.Status.Pin = &spiceboxv1alpha1.PinRecord{
		Kind:       "image",
		Strength:   "named",
		Digest:     pinDigest1,
		Version:    "v1",
		ObservedAt: &originalTime,
		Details:    map[string]string{"declaredRef": "ghcr.io/example/echo-mcp:v1"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()

	// Annotation matches live (digest1 == baseline).
	patchAnnotation(t, c, "default", "echo", spiceboxv1alpha1.AnnotationPinRefreeze, pinDigest1)
	r := &sidecartoolbox.Reconciler{
		Client:    c,
		ProbeFunc: makeProbeFunc(pinDigest1), // live == baseline
	}
	reconcileNN(t, r, "default", "echo")

	got := getCR(t, c, "default", "echo")

	// Annotation must be cleared.
	_, hasAnnotation := got.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze]
	assert.False(t, hasAnnotation, "annotation must be cleared even on no-op refreeze")

	// Baseline still at digest1.
	require.NotNil(t, got.Status.Pin)
	assert.Equal(t, pinDigest1, got.Status.Pin.Digest)

	// PinDrift must be True/PinMatch.
	pinDrift := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
	require.NotNil(t, pinDrift)
	assert.Equal(t, metav1.ConditionTrue, pinDrift.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonPinMatch, pinDrift.Reason)
}

// ---------------------------------------------------------------------------
// observedDriftDigest key in status.Pin.Details (Part 1)
// ---------------------------------------------------------------------------

// TestPinCheck_ObservedDriftDigest_SetOnDrift verifies that when digest drift
// is detected, status.Pin.Details["observedDriftDigest"] is set to the live
// (probed) digest and the baseline Digest is unchanged.
func TestPinCheck_ObservedDriftDigest_SetOnDrift(t *testing.T) {
	scheme := newScheme(t)
	originalTime := metav1.Now()
	cls, cr := validCRForPin("echo", "ghcr.io/example/echo-mcp:v1")
	cr.Status.Pin = &spiceboxv1alpha1.PinRecord{
		Kind:       "image",
		Strength:   "named",
		Digest:     pinDigest1,
		Version:    "v1",
		ObservedAt: &originalTime,
		Details:    map[string]string{"declaredRef": "ghcr.io/example/echo-mcp:v1"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()
	r := &sidecartoolbox.Reconciler{
		Client:    c,
		ProbeFunc: makeProbeFunc(pinDigest2), // live digest differs from baseline
	}

	reconcileNN(t, r, "default", "echo")

	got := getCR(t, c, "default", "echo")
	require.NotNil(t, got.Status.Pin)
	assert.Equal(t, pinDigest1, got.Status.Pin.Digest,
		"baseline must NOT change on drift")
	assert.Equal(t, pinDigest2, got.Status.Pin.Details["observedDriftDigest"],
		"observedDriftDigest must be set to the live (probed) digest")

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonPinDrifted, cond.Reason)
}

// TestPinCheck_ObservedDriftDigest_ClearedOnRecovery verifies that after drift
// is resolved (refreeze accepted), the observedDriftDigest key is removed from
// status.Pin.Details.
func TestPinCheck_ObservedDriftDigest_ClearedOnRecovery(t *testing.T) {
	scheme := newScheme(t)
	originalTime := metav1.Now()
	cls, cr := validCRForPin("echo", "ghcr.io/example/echo-mcp:v1")
	// Pre-seed a drifted state: baseline=digest1, key=digest2.
	cr.Status.Pin = &spiceboxv1alpha1.PinRecord{
		Kind:       "image",
		Strength:   "named",
		Digest:     pinDigest1,
		Version:    "v1",
		ObservedAt: &originalTime,
		Details: map[string]string{
			"declaredRef":         "ghcr.io/example/echo-mcp:v1",
			"observedDriftDigest": pinDigest2,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, cr).WithStatusSubresource(cr).Build()

	// Accept the drift via refreeze: annotation == live digest2.
	patchAnnotation(t, c, "default", "echo", spiceboxv1alpha1.AnnotationPinRefreeze, pinDigest2)
	r := &sidecartoolbox.Reconciler{
		Client:    c,
		ProbeFunc: makeProbeFunc(pinDigest2), // live is digest2 (matches annotation)
	}
	reconcileNN(t, r, "default", "echo")

	got := getCR(t, c, "default", "echo")

	// Baseline must be updated to digest2.
	require.NotNil(t, got.Status.Pin)
	assert.Equal(t, pinDigest2, got.Status.Pin.Digest)

	// observedDriftDigest must be gone (refreeze wrote a fresh PinRecord).
	_, hasKey := got.Status.Pin.Details["observedDriftDigest"]
	assert.False(t, hasKey, "observedDriftDigest must be cleared after recovery")

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonPinMatch, cond.Reason)
}
