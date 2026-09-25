package runner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// sessWithInProgressPlan builds a SessionContext whose plans Store has one
// plan ("main") with an in_progress item. The plans Kind comes from the plans
// package's own init() registration, which this binary links.
func sessWithInProgressPlan(t *testing.T) *tool.SessionContext {
	t.Helper()
	reg := state.NewRegistry(state.Deps{
		Operations:       operations.New(nil, nil),
		AppendSystemNote: func(context.Context, map[string]any) error { return nil },
	})
	sess := &tool.SessionContext{Namespace: "default", Name: "sess1", State: reg}
	store := plans.From(sess)
	_, err := store.Update(memory.WithSystemApproval(context.Background(), "test"), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusInProgress},
	}})

	require.NoError(t, err)
	return sess
}

type activityCall struct {
	plan   plans.Plan
	paused bool
	cause  string
	seq    uint64
	uid    string
}

func TestEmitPlanActivity_DedupesAndEnumeratesInProgress(t *testing.T) {
	var calls []activityCall
	l := &Loop{
		SessionKey:     memory.NamespacedName{Namespace: "default", Name: "sess1"},
		SessionContext: sessWithInProgressPlan(t),
		PublishPlanActivity: func(_ context.Context, p plans.Plan, paused bool, cause string, seq uint64, uid string) {
			calls = append(calls, activityCall{p, paused, cause, seq, uid})
		},
	}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// First paused emit fires for the one in_progress plan.
	l.emitActivity(ctx, true, channelevents.PauseCauseReply)
	require.Len(t, calls, 1)
	require.Equal(t, "main", calls[0].plan.Name)
	require.True(t, calls[0].paused)
	require.Equal(t, "awaiting_reply", calls[0].cause)

	// Same state again → deduped (no churn).
	l.emitActivity(ctx, true, channelevents.PauseCauseReply)
	require.Len(t, calls, 1, "repeated same-state emit must be deduped")

	// Transition to active → fires.
	l.emitActivity(ctx, false, "")
	require.Len(t, calls, 2)
	require.False(t, calls[1].paused)
}

func TestEmitPlanActivity_NoPublisherIsNoOp(t *testing.T) {
	l := &Loop{
		SessionKey:     memory.NamespacedName{Namespace: "default", Name: "sess1"},
		SessionContext: sessWithInProgressPlan(t),
	}
	// PublishPlanActivity nil → must not panic, nothing to assert beyond that.
	l.emitActivity(memory.WithSystemApproval(context.Background(), "test"), true, channelevents.PauseCauseReply)
}

func TestEmitPlanActivity_FirstActiveCallFiresFromNilState(t *testing.T) {
	// A fresh Run starts with lastPlanPaused nil; the first emit must fire even
	// for the active state, so a stale paused banner left by a prior Run is
	// cleared at turn-start.
	var calls []activityCall
	l := &Loop{
		SessionKey:     memory.NamespacedName{Namespace: "default", Name: "sess1"},
		SessionContext: sessWithInProgressPlan(t),
		PublishPlanActivity: func(_ context.Context, p plans.Plan, paused bool, cause string, seq uint64, uid string) {
			calls = append(calls, activityCall{p, paused, cause, seq, uid})
		},
	}
	l.emitActivity(memory.WithSystemApproval(context.Background(), "test"), false, "")
	require.Len(t, calls, 1, "first call (nil dedup state) must fire even when active")
	require.False(t, calls[0].paused)
}

func TestApprovalPause_RefcountStaysPausedUntilLastResolves(t *testing.T) {
	// Two approval-gated tool calls block in one turn. The card must stay
	// paused until BOTH resolve — a single resolution must not flip it active
	// while the other approval is still pending.
	var calls []activityCall
	l := &Loop{
		SessionKey:     memory.NamespacedName{Namespace: "default", Name: "sess1"},
		SessionContext: sessWithInProgressPlan(t),
		PublishPlanActivity: func(_ context.Context, p plans.Plan, paused bool, cause string, seq uint64, uid string) {
			calls = append(calls, activityCall{p, paused, cause, seq, uid})
		},
	}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	l.enterApprovalPause(ctx, channelevents.PauseCauseApproval) // 0→1: emit paused
	require.Len(t, calls, 1)
	require.True(t, calls[0].paused)
	require.Equal(t, "awaiting_approval", calls[0].cause)

	l.enterApprovalPause(ctx, channelevents.PauseCauseApproval) // 1→2: no emit
	require.Len(t, calls, 1, "second concurrent approval must not re-emit")

	l.exitApprovalPause(ctx) // 2→1: still pending, no emit
	require.Len(t, calls, 1, "card stays paused while one approval is still pending")

	l.exitApprovalPause(ctx) // 1→0: last resolved, emit active
	require.Len(t, calls, 2)
	require.False(t, calls[1].paused)
}

// sessWithoutPlan builds a SessionContext with NO plans Kind registered, so
// plans.TryFrom returns !ok — the planless case.
func sessWithoutPlan(t *testing.T) *tool.SessionContext {
	t.Helper()
	// The clear is the point here — the planless case needs an empty registry —
	// and the restore hands the init-registered Kinds back to the next test.
	t.Cleanup(state.ResetForTest())
	reg := state.NewRegistry(state.Deps{
		Operations:       operations.New(nil, nil),
		AppendSystemNote: func(context.Context, map[string]any) error { return nil },
	})
	return &tool.SessionContext{Namespace: "default", Name: "sess1", State: reg}
}

type turnActivityCall struct {
	active bool
	cause  string
	seq    uint64
	uid    string
}

// TestPausedTurnActivity_AskVersusIdleExit pins the difference between the two
// ways a turn stops without failing, because every surface that offers "answer
// the agent" reads exactly this cause to decide whether to.
//
// await_user_message is the agent ASKING and blocking on the answer; the idle
// exit is the agent having FINISHED, with the session parked and still open for
// a next message nobody owes it. Publishing awaiting_reply for both makes the
// end of every turn indistinguishable from a question — which is how a browser
// view ends up dropping a reply prompt over the agent's closing statement.
func TestPausedTurnActivity_AskVersusIdleExit(t *testing.T) {
	newLoop := func(calls *[]turnActivityCall) *Loop {
		l := &Loop{
			SessionKey: memory.NamespacedName{Namespace: "default", Name: "sess1"},
			PublishTurnActivity: func(_ context.Context, active bool, cause string, seq uint64, uid string) {
				*calls = append(*calls, turnActivityCall{active, cause, seq, uid})
			},
		}
		// A stub pipeline runner, so fireSessionEnd's SessionEnd point does not
		// build the real executor for a Loop with no hooks wired.
		l.pipelineExec = &endCapturingRunner{}
		l.pipelineOnce.Do(func() {})
		return l
	}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	t.Run("await_user_message parks as an ask: paused, cause awaiting_reply", func(t *testing.T) {
		var calls []turnActivityCall
		newLoop(&calls).OnAwaitYield(ctx)
		require.Len(t, calls, 1, "the yield publishes exactly one turn_activity")
		assert.False(t, calls[0].active)
		assert.Equal(t, channelevents.PauseCauseReply, calls[0].cause)
	})

	t.Run("the idle exit is not an ask: paused, cause idle", func(t *testing.T) {
		var calls []turnActivityCall
		newLoop(&calls).fireSessionEnd(ctx, "idle")
		require.Len(t, calls, 1, "the idle exit publishes exactly one turn_activity")
		assert.False(t, calls[0].active)
		assert.Equal(t, channelevents.PauseCauseIdle, calls[0].cause)
	})
}

func TestEmitActivity_PlanlessPublishesTurnActivityOnly(t *testing.T) {
	var turnCalls []turnActivityCall
	var planCalls []activityCall
	l := &Loop{
		SessionKey:     memory.NamespacedName{Namespace: "default", Name: "sess1"},
		SessionContext: sessWithoutPlan(t),
		PublishTurnActivity: func(_ context.Context, active bool, cause string, seq uint64, uid string) {
			turnCalls = append(turnCalls, turnActivityCall{active, cause, seq, uid})
		},
		PublishPlanActivity: func(_ context.Context, p plans.Plan, paused bool, cause string, seq uint64, uid string) {
			planCalls = append(planCalls, activityCall{p, paused, cause, seq, uid})
		},
	}
	l.emitActivity(memory.WithSystemApproval(context.Background(), "test"), true, channelevents.PauseCauseReply)

	require.Len(t, turnCalls, 1, "planless agent must still publish turn_activity")
	assert.False(t, turnCalls[0].active)
	assert.Equal(t, "awaiting_reply", turnCalls[0].cause)
	require.Empty(t, planCalls, "no in_progress plan ⇒ no plan snapshot")
}

func TestEmitActivity_PlanfulPublishesBoth(t *testing.T) {
	var turnCalls []turnActivityCall
	var planCalls []activityCall
	l := &Loop{
		SessionKey:     memory.NamespacedName{Namespace: "default", Name: "sess1"},
		SessionContext: sessWithInProgressPlan(t),
		PublishTurnActivity: func(_ context.Context, active bool, cause string, seq uint64, uid string) {
			turnCalls = append(turnCalls, turnActivityCall{active, cause, seq, uid})
		},
		PublishPlanActivity: func(_ context.Context, p plans.Plan, paused bool, cause string, seq uint64, uid string) {
			planCalls = append(planCalls, activityCall{p, paused, cause, seq, uid})
		},
	}
	l.emitActivity(memory.WithSystemApproval(context.Background(), "test"), true, channelevents.PauseCauseReply)

	require.Len(t, turnCalls, 1, "turn_activity always fires")
	require.Len(t, planCalls, 1, "in_progress plan also gets a snapshot")
	// Both fan-out publishes for one transition share the same order key.
	assert.Equal(t, turnCalls[0].seq, planCalls[0].seq, "turn + plan snapshots share the emit seq")
}

// TestEmitActivity_LoopSideSeqUsesSentinelBlockAndTurnIndex covers the
// no-IDs-context fallback: a paused emit (no sandbox IDs in ctx) must carry
// PackSeq(lastAssistantTurnIndex, SeqBlockEnd) so it sorts after the turn's
// tool blocks; an active emit uses SeqBlockStart. The session UID comes from
// the SessionContext.
func TestEmitActivity_LoopSideSeqUsesSentinelBlockAndTurnIndex(t *testing.T) {
	var turnCalls []turnActivityCall
	sess := sessWithoutPlan(t)
	sess.AgentSessionUID = "uid-loop"
	l := &Loop{
		SessionKey:     memory.NamespacedName{Namespace: "default", Name: "sess1"},
		SessionContext: sess,
		PublishTurnActivity: func(_ context.Context, active bool, cause string, seq uint64, uid string) {
			turnCalls = append(turnCalls, turnActivityCall{active, cause, seq, uid})
		},
	}
	l.setLastAssistantTurnIndex(5)
	// Paused (no IDs ctx) → end-of-turn sentinel.
	l.emitActivity(memory.WithSystemApproval(context.Background(), "test"), true, channelevents.PauseCauseReply)
	require.Len(t, turnCalls, 1)
	assert.Equal(t, channelevents.PackSeq(5, channelevents.SeqBlockEnd), turnCalls[0].seq)
	assert.Equal(t, "uid-loop", turnCalls[0].uid)

	// Transition to active → start-of-turn sentinel.
	l.emitActivity(memory.WithSystemApproval(context.Background(), "test"), false, "")
	require.Len(t, turnCalls, 2)
	assert.Equal(t, channelevents.PackSeq(5, channelevents.SeqBlockStart), turnCalls[1].seq)
}

// TestEmitRevoked_NoRaceWithLoopTurnIndexWrite is the -race proof that
// seqForEmit's IDs-free fallback is reached from a genuinely off-loop goroutine
// while the loop goroutine advances the turn index.
//
// The revocation path is the faithful, repeatable one of the three: internal/cmd/runner's
// ap.revocation NATS callback calls EmitRevoked on the subscription goroutine →
// applyEvent → the Revoked arm's AppendLog effect → lifecycleOrderKey →
// seqForEmit with an IDs-free rootCtx. applyEvent holds seqMu; Run's per-turn
// write holds nothing, so nothing orders the two. The other two off-loop
// readers reach the same fallback the same way: the SIGTERM stop handler
// (MarkPlanStoppedBestEffort, whose cancel() fires only after it returns, so the
// loop is still running) and claimAndRecover's decision re-arm goroutines.
//
// The read's value becomes OrderKey.Seq on an entry in the session's SIGNED,
// append-only lifecycle log, so a stale read misfiles a Revoked event in the
// fold's causal order — it is not a cosmetic race.
//
// Not meaningful without -race: it passes under a plain `go test` whether or not
// the field is guarded, because a torn/stale int still packs into a valid Seq.
func TestEmitRevoked_NoRaceWithLoopTurnIndexWrite(t *testing.T) {
	key := memory.NamespacedName{Namespace: "ns-race", Name: "sess-revoke"}
	l := &Loop{
		SessionKey:      key,
		LifecycleMemory: buildLifecycleMemory(),
		SessionContext:  &tool.SessionContext{Namespace: key.Namespace, Name: key.Name, AgentSessionUID: "uid-1"},
	}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	stop := make(chan struct{})
	done := make(chan struct{})
	// Writer: Run's per-turn advance (loop.go's setLastAssistantTurnIndex call
	// just before dispatching the turn), on what is the loop goroutine in
	// production.
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				l.setLastAssistantTurnIndex(i)
			}
		}
	}()

	// Reader: the oap.revocation NATS callback, on its own goroutine.
	for i := 0; i < 200; i++ {
		l.EmitRevoked(ctx, "tool-origin", "mcpserver/widget")
	}
	close(stop)
	<-done
}

func TestOnAwaitYieldEmitsPausedReply_ResumeEmitsActive(t *testing.T) {
	var turnCalls []turnActivityCall
	l := &Loop{
		SessionKey:     memory.NamespacedName{Namespace: "default", Name: "sess1"},
		SessionContext: sessWithoutPlan(t),
		PublishTurnActivity: func(_ context.Context, active bool, cause string, seq uint64, uid string) {
			turnCalls = append(turnCalls, turnActivityCall{active, cause, seq, uid})
		},
	}
	l.OnAwaitYield(memory.WithSystemApproval(context.Background(), "test"))
	l.OnAwaitResume(memory.WithSystemApproval(context.Background(), "test"))

	require.Len(t, turnCalls, 2)
	assert.False(t, turnCalls[0].active, "yield emits paused")
	assert.Equal(t, channelevents.PauseCauseReply, turnCalls[0].cause)
	assert.True(t, turnCalls[1].active, "resume emits active")
}

// TestOnAwaitYieldStampsSeqFromIDsCtx covers the IDs-context path: OnAwaitYield
// runs inside await's Execute ctx, which carries the per-call IDs, so the
// paused turn_activity must be stamped from those IDs (not the loop fallback).
func TestOnAwaitYieldStampsSeqFromIDsCtx(t *testing.T) {
	var turnCalls []turnActivityCall
	l := &Loop{
		SessionKey:     memory.NamespacedName{Namespace: "default", Name: "sess1"},
		SessionContext: sessWithoutPlan(t),
		PublishTurnActivity: func(_ context.Context, active bool, cause string, seq uint64, uid string) {
			turnCalls = append(turnCalls, turnActivityCall{active, cause, seq, uid})
		},
	}
	l.setLastAssistantTurnIndex(99) // must be ignored in favor of the ctx IDs
	ctx := sandbox.WithIDs(memory.WithSystemApproval(context.Background(), "test"), sandbox.IDs{
		MemTurnIndex: 3, BlockIndex: 1, SessionUID: "uid-await",
	})
	l.OnAwaitYield(ctx)

	require.Len(t, turnCalls, 1)
	assert.Equal(t, channelevents.PackSeq(3, 1), turnCalls[0].seq, "uses the IDs-ctx index, not the loop fallback")
	assert.Equal(t, "uid-await", turnCalls[0].uid)
}

// TestApplyEvent_StoppedMarksPlanStopped verifies that a RunnerTerminal(Failed)
// event drives the plan-stop effect: the plans store transitions
// non-terminal items to stopped AND a plan_update with PauseCauseStopped is
// published — not swallowed by yield-suppression, because the publish happens
// here in applyEvent, before the paused turn_activity from fireSessionEnd.
func TestApplyEvent_StoppedMarksPlanStopped(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sess := sessWithInProgressPlan(t)

	var planCalls []activityCall
	l := &Loop{
		SessionKey:     memory.NamespacedName{Namespace: "default", Name: "sess1"},
		SessionContext: sess,
		PublishPlanActivity: func(_ context.Context, p plans.Plan, paused bool, cause string, seq uint64, uid string) {
			planCalls = append(planCalls, activityCall{p, paused, cause, seq, uid})
		},
	}

	// RunnerTerminal(Failed) triggers the MarkPlanStopped effect in the lifecycle core.
	l.applyEvent(ctx, lifecyclecore.RunnerTerminal{
		Phase:  lifecyclecore.PhaseFailed,
		Reason: "TestReason",
	})

	// The in_progress item must now be stopped in the store.
	store := plans.From(sess)
	got, ok := store.Get("main")
	require.True(t, ok)
	item, found := got.FindItem("a")
	require.True(t, found)
	assert.Equal(t, plans.StatusStopped, item.Status,
		"in_progress item must be stopped on interrupted termination")

	// A plan_update envelope with PauseCauseStopped must have been published.
	require.Len(t, planCalls, 1, "one plan snapshot must be published")
	assert.True(t, planCalls[0].paused)
	assert.Equal(t, channelevents.PauseCauseStopped, planCalls[0].cause,
		"plan snapshot must carry PauseCauseStopped, not PauseCauseFailed")
	assert.Equal(t, "main", planCalls[0].plan.Name)
}

// TestApplyEvent_SucceededDoesNotMarkPlanStopped verifies that a clean
// RunnerTerminal(Succeeded) does NOT mark plan items stopped.
func TestApplyEvent_SucceededDoesNotMarkPlanStopped(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sess := sessWithInProgressPlan(t)

	l := &Loop{
		SessionKey:     memory.NamespacedName{Namespace: "default", Name: "sess1"},
		SessionContext: sess,
		PublishPlanActivity: func(_ context.Context, p plans.Plan, _ bool, _ string, _ uint64, _ string) {
			t.Errorf("unexpected plan activity published for clean succeeded completion: plan=%s", p.Name)
		},
	}

	// RunnerTerminal(Succeeded) must NOT emit MarkPlanStopped.
	l.applyEvent(ctx, lifecyclecore.RunnerTerminal{
		Phase: lifecyclecore.PhaseSucceeded,
	})

	store := plans.From(sess)
	got, ok := store.Get("main")
	require.True(t, ok)
	item, found := got.FindItem("a")
	require.True(t, found)
	assert.Equal(t, plans.StatusInProgress, item.Status,
		"clean completion must not stop plan items")
}

// TestMarkPlanStoppedBestEffort_StopsPlanAndPublishesStopped covers the SIGTERM
// / admin-kill / supersede path in internal/cmd/runner: the raw Stopped lifecycle append
// there bypasses the sequencer, so the runner marks the plan stopped + publishes
// PauseCauseStopped directly via this helper on a fresh context. The in_progress
// item must end up Stopped and a paused plan snapshot carrying PauseCauseStopped
// must be published — satisfying the requirement that stopping the agent marks
// its plan stopped too.
func TestMarkPlanStoppedBestEffort_StopsPlanAndPublishesStopped(t *testing.T) {
	sess := sessWithInProgressPlan(t)
	var planCalls []activityCall
	l := &Loop{
		SessionKey:     memory.NamespacedName{Namespace: "default", Name: "sess1"},
		SessionContext: sess,
		PublishPlanActivity: func(_ context.Context, p plans.Plan, paused bool, cause string, seq uint64, uid string) {
			planCalls = append(planCalls, activityCall{p, paused, cause, seq, uid})
		},
	}

	// A fresh (non-cancelled) context, as the SIGTERM handler passes.
	l.MarkPlanStoppedBestEffort(memory.WithSystemApproval(context.Background(), "test"))

	store := plans.From(sess)
	got, ok := store.Get("main")
	require.True(t, ok)
	item, found := got.FindItem("a")
	require.True(t, found)
	assert.Equal(t, plans.StatusStopped, item.Status,
		"in_progress item must be stopped when the agent is stopped (SIGTERM)")

	require.Len(t, planCalls, 1, "one paused plan snapshot must be published")
	assert.True(t, planCalls[0].paused)
	assert.Equal(t, channelevents.PauseCauseStopped, planCalls[0].cause,
		"plan snapshot must carry PauseCauseStopped")
	assert.Equal(t, "main", planCalls[0].plan.Name)
}

// TestMarkPlanStoppedBestEffort_PlanlessAndNilContextAreNoOps guards the
// best-effort contract: a planless session and a nil SessionContext must not
// panic and must publish nothing.
func TestMarkPlanStoppedBestEffort_PlanlessAndNilContextAreNoOps(t *testing.T) {
	var planCalls []activityCall
	pub := func(_ context.Context, p plans.Plan, paused bool, cause string, seq uint64, uid string) {
		planCalls = append(planCalls, activityCall{p, paused, cause, seq, uid})
	}
	// Planless session: plans.TryFrom returns !ok → no-op.
	lp := &Loop{
		SessionKey:          memory.NamespacedName{Namespace: "default", Name: "sess1"},
		SessionContext:      sessWithoutPlan(t),
		PublishPlanActivity: pub,
	}
	lp.MarkPlanStoppedBestEffort(memory.WithSystemApproval(context.Background(), "test"))
	assert.Empty(t, planCalls, "planless session publishes nothing")

	// Nil SessionContext: early return, no panic, nothing published.
	ln := &Loop{
		SessionKey:          memory.NamespacedName{Namespace: "default", Name: "sess1"},
		PublishPlanActivity: pub,
	}
	ln.MarkPlanStoppedBestEffort(memory.WithSystemApproval(context.Background(), "test"))
	assert.Empty(t, planCalls, "nil SessionContext publishes nothing")
}
