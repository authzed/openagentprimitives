//go:build integration

// pkg/controllers/agentclass/oapinstall_test.go
package agentclass_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

// createLLMCredsSecret creates the Secret newClass's AgentClass references, so
// Reconcile reaches the ordinary Valid=True path rather than parking at
// SecretMissing — keeping these oap-install-mirror tests independent of that
// unrelated validity check.
func createLLMCredsSecret(t *testing.T, env *testenv.Env) {
	t.Helper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk-ant-test")},
	}
	require.NoError(t, env.Client.Create(context.Background(), sec), "create llm-creds")
}

// reconcileAC drives one Reconcile of the named AgentClass in the default
// namespace and returns the reloaded object.
func reconcileAC(t *testing.T, env *testenv.Env, r *agentclass.Reconciler, name string) spiceboxv1alpha1.AgentClass {
	t.Helper()
	ctx := context.Background()
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}})
	require.NoError(t, err, "Reconcile %s", name)
	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, &got), "Get %s", name)
	return got
}

// seedOapInstallStatus writes an initial status.oapInstall onto the named
// AgentClass via the status subresource, standing in for a prior install's
// mirror — so preserve/update semantics can be exercised deterministically
// against a KNOWN prior InstalledAt rather than racing the wall clock.
func seedOapInstallStatus(t *testing.T, env *testenv.Env, name string, s spiceboxv1alpha1.OapInstallStatus) {
	t.Helper()
	ctx := context.Background()
	var ac spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, &ac), "Get for seed")
	ac.Status.OapInstall = &s
	require.NoError(t, env.Client.Status().Update(ctx, &ac), "seed status.oapInstall")
}

// TestOapInstallMirroredFromAnnotation covers the primary path: a valid
// oap-source annotation, produced the same way `oap agent install` stamps it
// (instance.MarshalOapSource, which no longer carries a timestamp), is mirrored
// into status.oapInstall — and the controller stamps a non-zero InstalledAt of
// its own, since the annotation carries none.
func TestOapInstallMirroredFromAnnotation(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	createLLMCredsSecret(t, env)

	raw, err := instance.MarshalOapSource(instance.OapSource{
		Ref:        "registry.example.com/agents/support-bot:v3",
		Digest:     "sha256:deadbeef",
		Version:    "1.2.3",
		SourceKind: "registry",
	})
	require.NoError(t, err, "MarshalOapSource")

	ac := newClass("oap-mirror-1")
	ac.Annotations = map[string]string{instance.AnnotationOapSource: raw}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	got := reconcileAC(t, env, r, "oap-mirror-1")

	if assert.NotNil(t, got.Status.OapInstall, "status.oapInstall must be populated") {
		assert.Equal(t, "registry.example.com/agents/support-bot:v3", got.Status.OapInstall.SourceRef)
		assert.Equal(t, "sha256:deadbeef", got.Status.OapInstall.Digest)
		assert.Equal(t, "1.2.3", got.Status.OapInstall.Version)
		assert.Equal(t, "registry", got.Status.OapInstall.SourceKind)
		assert.False(t, got.Status.OapInstall.InstalledAt.IsZero(),
			"controller must stamp a wall-clock InstalledAt (the annotation carries none)")
	}
	assert.True(t,
		hasCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, metav1.ConditionTrue),
		"want Valid=True; got conditions=%+v", got.Status.Conditions)
}

// TestOapInstallNilWithoutAnnotation covers an AgentClass created by any
// non-oap means (kubectl apply, wizard, etc.): with no oap-source annotation,
// status.oapInstall must stay nil rather than being synthesized.
func TestOapInstallNilWithoutAnnotation(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	createLLMCredsSecret(t, env)

	ac := newClass("oap-mirror-2")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass without annotation")

	got := reconcileAC(t, env, r, "oap-mirror-2")
	assert.Nil(t, got.Status.OapInstall, "status.oapInstall must stay nil absent the annotation")
}

// TestOapInstallUnchangedOnMalformedAnnotation covers a corrupted annotation
// value (e.g. hand-edited or truncated JSON): Reconcile must log and move on
// rather than fail the whole reconcile or synthesize a bogus status.
func TestOapInstallUnchangedOnMalformedAnnotation(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	createLLMCredsSecret(t, env)

	ac := newClass("oap-mirror-3")
	ac.Annotations = map[string]string{instance.AnnotationOapSource: "not json"}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass with malformed annotation")

	got := reconcileAC(t, env, r, "oap-mirror-3")
	assert.Nil(t, got.Status.OapInstall, "status.oapInstall must remain unchanged (nil) on parse failure")
}

// TestOapInstallInstalledAtPreservedOnSameDigest covers the set-once semantics:
// re-mirroring an annotation whose (sourceRef, digest) already matches the
// recorded status must PRESERVE the original InstalledAt — an idempotent
// re-install must not reset the install time. Seeded with a known-old
// InstalledAt so the assertion is deterministic.
func TestOapInstallInstalledAtPreservedOnSameDigest(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	createLLMCredsSecret(t, env)

	raw, err := instance.MarshalOapSource(instance.OapSource{
		Ref:        "registry.example.com/agents/support-bot:v3",
		Digest:     "sha256:samedigest",
		Version:    "1.2.3",
		SourceKind: "registry",
	})
	require.NoError(t, err, "MarshalOapSource")

	ac := newClass("oap-preserve")
	ac.Annotations = map[string]string{instance.AnnotationOapSource: raw}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	oldTime := metav1.NewTime(time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC))
	seedOapInstallStatus(t, env, "oap-preserve", spiceboxv1alpha1.OapInstallStatus{
		SourceRef:   "registry.example.com/agents/support-bot:v3",
		Digest:      "sha256:samedigest",
		Version:     "1.2.3",
		SourceKind:  "registry",
		InstalledAt: oldTime,
	})

	got := reconcileAC(t, env, r, "oap-preserve")
	require.NotNil(t, got.Status.OapInstall, "status.oapInstall must remain populated")
	assert.True(t, oldTime.Time.Equal(got.Status.OapInstall.InstalledAt.Time),
		"InstalledAt must be PRESERVED for an unchanged (sourceRef, digest); want %v, got %v",
		oldTime.Time, got.Status.OapInstall.InstalledAt.Time)
}

// TestOapInstallInstalledAtUpdatedOnDigestChange covers the upgrade path: when
// the annotation's digest differs from the recorded status digest, the
// controller stamps a fresh InstalledAt (and updates the digest). Seeded with a
// known-old InstalledAt + a different digest so the update is unambiguous.
func TestOapInstallInstalledAtUpdatedOnDigestChange(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	createLLMCredsSecret(t, env)

	raw, err := instance.MarshalOapSource(instance.OapSource{
		Ref:        "registry.example.com/agents/support-bot:v4",
		Digest:     "sha256:newdigest",
		Version:    "2.0.0",
		SourceKind: "registry",
	})
	require.NoError(t, err, "MarshalOapSource")

	ac := newClass("oap-upgrade")
	ac.Annotations = map[string]string{instance.AnnotationOapSource: raw}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	oldTime := metav1.NewTime(time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC))
	seedOapInstallStatus(t, env, "oap-upgrade", spiceboxv1alpha1.OapInstallStatus{
		SourceRef:   "registry.example.com/agents/support-bot:v3",
		Digest:      "sha256:olddigest",
		Version:     "1.2.3",
		SourceKind:  "registry",
		InstalledAt: oldTime,
	})

	got := reconcileAC(t, env, r, "oap-upgrade")
	require.NotNil(t, got.Status.OapInstall, "status.oapInstall must remain populated")
	assert.Equal(t, "sha256:newdigest", got.Status.OapInstall.Digest, "digest must update to the new install")
	assert.Equal(t, "2.0.0", got.Status.OapInstall.Version, "version must update to the new install")
	assert.False(t, oldTime.Time.Equal(got.Status.OapInstall.InstalledAt.Time),
		"InstalledAt must be UPDATED on a digest change, not preserved from the prior install")
	assert.False(t, got.Status.OapInstall.InstalledAt.IsZero(), "the fresh InstalledAt must be non-zero")
}
