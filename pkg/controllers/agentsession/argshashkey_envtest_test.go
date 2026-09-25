//go:build integration

// pkg/controllers/agentsession/argshashkey_envtest_test.go
//
// Reconcile-level tests for the per-session args-hash HMAC key: minted into
// the per-session Secret on first reconcile, and re-ensured on the update
// path for pre-upgrade Secrets that lack it (the runner pod's SubPath mount
// would otherwise hang in ContainerCreating).
package agentsession_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

func TestReconcileMintsArgsHashKey(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	ac := validClass("ahk-ac")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)
	require.NoError(t, env.Client.Create(ctx, validSession("ahk-s", "ahk-ac")), "create AgentSession")
	reconcileToWork(t, ctx, r, "ahk-s")

	var sec corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{
		Namespace: "default",
		Name:      agentsession.MemoryTokenSecretName(&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "ahk-s"}}),
	}, &sec), "per-session Secret must exist")
	key := sec.Data["args-hash-key"]
	require.NotEmpty(t, key, "args-hash-key minted on first reconcile")
	assert.Len(t, key, 64, "32 random bytes, hex-encoded")
}

func TestReconcileEnsuresArgsHashKeyOnExistingSecret(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	ac := validClass("ahk-ac2")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)
	require.NoError(t, env.Client.Create(ctx, validSession("ahk-s2", "ahk-ac2")), "create AgentSession")
	reconcileToWork(t, ctx, r, "ahk-s2")

	// Simulate a pre-upgrade Secret: strip the key, keep everything else.
	secName := agentsession.MemoryTokenSecretName(&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "ahk-s2"}})
	var sec corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: secName}, &sec))
	priorToken := sec.Data["token"]
	delete(sec.Data, "args-hash-key")
	require.NoError(t, env.Client.Update(ctx, &sec), "strip args-hash-key (pre-upgrade Secret)")

	reconcileToWork(t, ctx, r, "ahk-s2")

	var after corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: secName}, &after))
	assert.Len(t, after.Data["args-hash-key"], 64, "key re-ensured on the update path")
	assert.Equal(t, priorToken, after.Data["token"], "other Secret keys untouched")
}
