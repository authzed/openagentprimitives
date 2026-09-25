//go:build integration

// pkg/controllers/agentsession/audit_chainheads_envtest_test.go
//
// Reconcile-level test for chain-head anchoring on session completion:
// when a session first reaches a terminal phase, the reconciler computes
// each publisher's final chain head (via the AuditChainHeads DI) and
// stamps status.AuditChainHeads as publisher → "seq:lastHash". The compute
// runs once — a populated map is not recomputed on later reconciles.
package agentsession_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
)

func TestReconcileRecordsAuditChainHeadsOnCompletion(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)

	// Stub the DI: a fixed two-publisher head map. The reconciler must
	// format it as publisher → "seq:lastHash" and only call us once.
	var calls int
	want := map[string]provenance.ChainHead{
		"session:default/cs-s": {Seq: 3, LastHash: "aaaa"},
		"system:operator":      {Seq: 2, LastHash: "bbbb"},
	}
	r.AuditChainHeads = func(_ context.Context, scope memory.Scope) (map[string]provenance.ChainHead, error) {
		calls++
		assert.Equal(t, "default/cs-s", scope.ID, "scope is the session's namespace/name")
		return want, nil
	}

	ac := validClass("cs-ac")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)
	require.NoError(t, env.Client.Create(ctx, validSession("cs-s", "cs-ac")), "create AgentSession")
	reconcileToWork(t, ctx, r, "cs-s")

	// Drive the session to a terminal phase directly.
	key := types.NamespacedName{Namespace: "default", Name: "cs-s"}
	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &sess))
	sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
	require.NoError(t, env.Client.Status().Update(ctx, &sess), "mark session Succeeded")

	// One reconcile records the heads.
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err, "reconcile of a terminal session must succeed")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &got))
	require.NotNil(t, got.Status.AuditChainHeads, "AuditChainHeads recorded on completion")
	assert.Equal(t, map[string]string{
		"session:default/cs-s": "3:aaaa",
		"system:operator":      "2:bbbb",
	}, got.Status.AuditChainHeads, "heads formatted as publisher → seq:lastHash")
	require.Equal(t, 1, calls, "chain heads computed exactly once")

	// A second reconcile must NOT recompute (map already populated).
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "populated heads are not recomputed on later reconciles")
}
