//go:build integration

// pkg/controllers/agentsession/lifecycle_fold_envtest_test.go
//
// Integration (envtest) coverage for the unified-session-state thesis at the
// operator layer: the signed lifecycle log is the source of truth for phase,
// and a full Reconcile appends transition events to it, folds it, and does not
// strand.
//
// Two behaviours are pinned here that the pre-existing archive/sequencer tests
// do not reach:
//
//  1. Archive sweep through the LOG path (M1). The existing
//     TestArchiveSweepTransitionsAfterDeadline runs with LifecycleMemory unwired,
//     so the archive sweep only takes the direct-status-write fallback. Here we
//     wire LifecycleMemory and seed a runner IdleYield so the folded log projects
//     Idle. The sweep must (a) transition the CR to Succeeded, (b) append
//     ArchiveSweep to the signed log, and (c) leave a log that re-folds to
//     Succeeded — proving the idle -> IdleYield -> ArchiveSweep -> Succeeded chain
//     terminates cleanly through the log, not just the status shortcut.
//
//  2. Operator restart re-folds the durable log (H2). The design's core thesis
//     ("a runner/operator restart re-folds the log and nothing strands") is
//     otherwise only unit-tested against a hand-seeded log (sequencer_test.go).
//     Here we exercise it through a full Reconcile over a real API server: a
//     provisioned session's durable log is advanced to Running, a fresh
//     Reconciler ("bounced" operator, no in-process state) re-reconciles, and the
//     CR phase reconstructs from the fold instead of stranding at the bootstrap
//     Pending floor. A pending decision survives the bounce and resolving it
//     resumes — the operator-layer analog of
//     TestRestartRecovery_OpenDecisionRearmedOnRestart (which covers the runner's
//     claimAndRecover re-arm).
package agentsession_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	lifecyclekind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// lifecycleScopeFor mirrors the unexported agentsession.lifecycleScope: the
// memory scope holding a session's transition log.
func lifecycleScopeFor(ns, name string) memory.Scope {
	return memory.Scope{Kind: "session", ID: ns + "/" + name}
}

// seedLifecycleEvent appends ev to the durable lifecycle log for a session, in
// the runner region at (turn, block). Mirrors how the runner stamps its own
// events (RegionRunner, PackSeq(memTurnIndex, blockIndex)) so the operator's
// operator_post ArchiveSweep sorts after them at equal Seq.
func seedLifecycleEvent(t *testing.T, mem memory.Memory, ns, name, uid string, turn, block int, ev lifecyclecore.Event) {
	t.Helper()
	err := lifecyclekind.Append(memory.WithSystemApproval(context.Background(), "test"), mem, lifecycleScopeFor(ns, name), ev, time.Now().UTC(),
		lifecyclekind.OrderKey{
			Seq:        channelevents.PackSeq(turn, block),
			Region:     string(lifecyclecore.RegionRunner),
			SessionUID: uid,
		})
	require.NoError(t, err, "seed lifecycle event %T", ev)
}

// foldPhase reads the session's durable lifecycle log and returns the folded,
// projected phase — the same computation the operator's derivePhase performs.
func foldPhase(t *testing.T, mem memory.Memory, ns, name string) string {
	t.Helper()
	events, err := lifecyclekind.Events(memory.WithSystemApproval(context.Background(), "test"), mem, lifecycleScopeFor(ns, name))
	require.NoError(t, err, "read lifecycle log")
	return lifecyclecore.Project(lifecyclecore.Fold(events)).Phase
}

// lifecycleEventKinds returns the concrete type names of every event in the
// session's durable log, for presence assertions.
func lifecycleEventKinds(t *testing.T, mem memory.Memory, ns, name string) []string {
	t.Helper()
	events, err := lifecyclekind.Events(memory.WithSystemApproval(context.Background(), "test"), mem, lifecycleScopeFor(ns, name))
	require.NoError(t, err, "read lifecycle log")
	kinds := make([]string, 0, len(events))
	for _, e := range events {
		switch e.(type) {
		case lifecyclecore.IdleYield:
			kinds = append(kinds, "IdleYield")
		case lifecyclecore.ArchiveSweep:
			kinds = append(kinds, "ArchiveSweep")
		case lifecyclecore.RunnerClaimed:
			kinds = append(kinds, "RunnerClaimed")
		case lifecyclecore.SettingsAccepted:
			kinds = append(kinds, "SettingsAccepted")
		case lifecyclecore.DecisionAsked:
			kinds = append(kinds, "DecisionAsked")
		case lifecyclecore.DecisionResolved:
			kinds = append(kinds, "DecisionResolved")
		}
	}
	return kinds
}

// TestArchiveSweep_ThroughLifecycleLog_FoldsToSucceeded is the log-driven analog
// of TestArchiveSweepTransitionsAfterDeadline. With LifecycleMemory wired and a
// seeded runner IdleYield (the runner yielded to Idle after its idle-TTL), the
// archive sweep must archive the CR to Succeeded, record ArchiveSweep in the
// signed log, and leave a log that re-folds to Succeeded — the idle ->
// IdleYield -> ArchiveSweep -> Succeeded chain must not strand back to Idle or
// Pending. (M1)
func TestArchiveSweep_ThroughLifecycleLog_FoldsToSucceeded(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// Short archive window so the seeded LastIdleAt is comfortably past the
	// deadline; LifecycleMemory wired so the sweep exercises the log path.
	mem := memory.NewLocal(inmem.NewBackend())
	r := newReconcilerWithArchive(t, env, time.Minute)
	r.LifecycleMemory = mem

	ac := classWithArchiveAfter("ac-arch-log", time.Minute)
	sess := channelSession("arch-log", "ac-arch-log")
	bootstrapSession(t, env, r, ac, sess)

	var created spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &created), "Get created session")

	// Seed the runner's IdleYield so the folded log projects Idle before the
	// sweep. Without this the fold sits at the Pending floor and ArchiveSweep is
	// a no-op on the projection (it only advances a folded Idle).
	seedLifecycleEvent(t, mem, sess.Namespace, sess.Name, string(created.UID),
		1, channelevents.SeqBlockEnd, lifecyclecore.IdleYield{})
	require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, foldPhase(t, mem, sess.Namespace, sess.Name),
		"seeded log must fold to Idle before the sweep")

	// CR is Idle with a LastIdleAt two minutes ago — past the 1m deadline.
	setIdlePhaseWithTimestamp(t, env, sess, time.Now().Add(-2*time.Minute))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	require.NoError(t, err, "Reconcile (archive sweep through the lifecycle log)")

	// (a) CR archived to Succeeded, not stranded.
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &got), "Get session after sweep")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, got.Status.Phase, "phase after archive")
	assert.NotNil(t, got.Status.FinishedAt, "FinishedAt stamped on archive")
	assert.Nil(t, got.Status.LastIdleAt, "LastIdleAt cleared on archive")

	// (b) ArchiveSweep recorded in the signed lifecycle log.
	assert.Contains(t, lifecycleEventKinds(t, mem, sess.Namespace, sess.Name), "ArchiveSweep",
		"ArchiveSweep must be appended to the durable lifecycle log")

	// (c) Re-folding the durable log now projects Succeeded — the chain
	// terminates and does not strand.
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, foldPhase(t, mem, sess.Namespace, sess.Name),
		"re-folded log is terminal Succeeded (idle -> IdleYield -> ArchiveSweep does not strand)")
}

// TestOperatorBounce_ReFoldsDurableLog_PhaseReconstructs proves the design's
// restart thesis at the wired operator layer: after a runner claim and an open
// decision are durable in the log, a fresh Reconciler (a "bounced" operator with
// no in-process state) re-reconciles the persisted CR and reconstructs phase
// purely from the folded log — nothing strands at the bootstrap Pending floor,
// the pending decision survives the bounce, and resolving it resumes. This is
// the operator-layer analog of TestRestartRecovery_OpenDecisionRearmedOnRestart
// (runner-side claimAndRecover re-arm). (H2)
func TestOperatorBounce_ReFoldsDurableLog_PhaseReconstructs(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// The durable lifecycle log survives every simulated operator bounce; only
	// the Reconciler instance is replaced (fresh Tokens/Memory, same client, same
	// LifecycleMemory) to model a restarted operator process.
	mem := memory.NewLocal(inmem.NewBackend())

	bounced := func() *agentsession.Reconciler {
		r := newReconciler(t, env)
		r.LifecycleMemory = mem
		return r
	}

	ac := validClass("ac-bounce")
	sess := validSession("bounce1", "ac-bounce")
	bootstrapSession(t, env, bounced(), ac, sess)

	var created spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &created), "Get created session")
	uid := string(created.UID)
	key := types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}

	// Part A: durable RunnerClaimed -> a bounced operator reconstructs Running.
	//
	// envtest has no kubelet, so the runner pod never reports Ready and the
	// operator never naturally emits RunnerClaimed. Seeding it into the durable
	// log models the prior runner incarnation having claimed the session before
	// the operator restarted.
	seedLifecycleEvent(t, mem, sess.Namespace, sess.Name, uid, 1, channelevents.SeqBlockStart, lifecyclecore.RunnerClaimed{})
	require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, foldPhase(t, mem, sess.Namespace, sess.Name),
		"durable log with RunnerClaimed folds to Running")

	_, err := bounced().Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err, "bounced operator reconcile (Part A)")

	var afterA spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &afterA), "Get session after Part A")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, afterA.Status.Phase,
		"a bounced operator reconstructs Running from the durable log — it does not strand at Pending")

	// Part B: an open decision survives the bounce; the phase reflects it.
	seedLifecycleEvent(t, mem, sess.Namespace, sess.Name, uid, 1, channelevents.SeqBlockStart+1,
		lifecyclecore.DecisionAsked{RequestID: "r1", Kind: lifecyclecore.DecisionToolCall})
	require.Equal(t, string(lifecyclecore.PhaseAwaitingDecision), foldPhase(t, mem, sess.Namespace, sess.Name),
		"an open tool-call decision folds to AwaitingDecision")

	_, err = bounced().Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err, "bounced operator reconcile (Part B)")

	var afterB spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &afterB), "Get session after Part B")
	assert.Equal(t, string(lifecyclecore.PhaseAwaitingDecision), afterB.Status.Phase,
		"the bounced operator reconstructs the pending-decision phase from the durable log")
	// The pending decision is durable: it survived the bounce and no resolution
	// has been recorded, so the runner's claimAndRecover will re-arm it.
	kinds := lifecycleEventKinds(t, mem, sess.Namespace, sess.Name)
	assert.Contains(t, kinds, "DecisionAsked", "the open decision must survive the operator bounce")
	assert.NotContains(t, kinds, "DecisionResolved", "the decision is still pending after the bounce")

	// Part C: resolving the decision resumes — the bounced operator re-folds back
	// to Running (the analog of a delivered decision unblocking the runner).
	seedLifecycleEvent(t, mem, sess.Namespace, sess.Name, uid, 1, channelevents.SeqBlockStart+2,
		lifecyclecore.DecisionResolved{RequestID: "r1", Approved: true})
	require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, foldPhase(t, mem, sess.Namespace, sess.Name),
		"resolving the decision unparks the fold back to Running")

	_, err = bounced().Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err, "bounced operator reconcile (Part C)")

	var afterC spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &afterC), "Get session after Part C")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, afterC.Status.Phase,
		"resolving the pending decision resumes the session to Running")
}
