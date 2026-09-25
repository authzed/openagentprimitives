package runner

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/x/besteffort"
)

// emitActivity publishes the agent's coarse active/paused state at a true
// active⇄paused transition (deduped). It fans out to two consumers:
//  1. the planless KindTurnActivity signal for channelsd's silence watchdog
//     (always, with or without a plan); and
//  2. a plan_update snapshot for every plan with an in_progress item, so
//     channels neutralise the active hourglass + show a paused banner.
//
// No-op when neither publisher is wired (kubectl-driven/tests). The first call
// always fires (nil dedup state) so a fresh Run clears stale state.
func (l *Loop) emitActivity(ctx context.Context, paused bool, cause string) {
	if l.PublishTurnActivity == nil && l.PublishPlanActivity == nil {
		return
	}

	l.planActivityMu.Lock()
	if l.lastPlanPaused != nil && *l.lastPlanPaused == paused {
		l.planActivityMu.Unlock()
		return
	}
	b := paused
	l.lastPlanPaused = &b
	l.planActivityMu.Unlock()

	// Stamp the logical Seq + session UID once so the turn_activity signal and
	// every per-plan snapshot for this transition share an order key.
	seq, uid := l.seqForEmit(ctx, paused)

	// 1. Planless watchdog signal — works regardless of whether a plan exists.
	if l.PublishTurnActivity != nil {
		l.PublishTurnActivity(ctx, !paused, cause, seq, uid)
	}

	// 2. Per-plan banner snapshot — only plans that have an in_progress item.
	if l.PublishPlanActivity == nil {
		return
	}
	store, ok := plans.TryFrom(l.SessionContext)
	if !ok {
		return
	}
	for _, p := range store.InProgress() {
		l.PublishPlanActivity(ctx, p, paused, cause, seq, uid)
	}
}

// setLastAssistantTurnIndex publishes the memory Index of the turn the loop is
// about to dispatch. Called only from Run (the replay seed and the per-turn
// advance).
func (l *Loop) setLastAssistantTurnIndex(i int) {
	l.lastAssistantTurnIndex.Store(int64(i))
}

// lastAssistantTurnIndexValue reads the current assistant-turn index for
// seqForEmit's IDs-free fallback, which runs on off-loop goroutines (the
// revocation subscriber, the SIGTERM stop handler, the decision re-arm
// goroutines) concurrently with the loop's per-turn write.
func (l *Loop) lastAssistantTurnIndexValue() int {
	return int(l.lastAssistantTurnIndex.Load())
}

// seqForEmit derives the logical Seq + session UID for an emitActivity publish.
// Inside a tool's Execute context (OnAwaitYield / approval pauses) the per-call
// IDs are present and authoritative. Loop-side without an IDs context
// (start-of-turn active, retry/fail/reply pauses, fireSessionEnd) it falls back
// to the current assistant turn index with a sentinel block — SeqBlockEnd for
// paused (sorts after the turn's last tool block), SeqBlockStart for active
// (sorts before the first) — so the consumer orders these correctly relative to
// per-block status events.
//
// Callers span goroutines: the loop itself, the SIGTERM/SIGINT stop handler
// (MarkPlanStoppedBestEffort), the NATS revocation subscriber, and
// claimAndRecover's decision-re-arm. BOTH fallback reads below are therefore
// guarded — l.SessionContext under sessionCtxMu (a consistent snapshot instead
// of racing Run's entry-setup reassignment; an RLock taken from the loop
// goroutine itself is harmless) and the turn index through
// lastAssistantTurnIndexValue (against Run's per-turn write). Both feed an
// OrderKey on a signed, append-only log entry, so a torn read is a mis-ordered
// audit record, not a cosmetic glitch.
func (l *Loop) seqForEmit(ctx context.Context, paused bool) (uint64, string) {
	if ids, ok := sandbox.IDsFromCtx(ctx); ok {
		return channelevents.PackSeq(ids.MemTurnIndex, ids.BlockIndex), ids.SessionUID
	}
	sentinel := channelevents.SeqBlockStart
	if paused {
		sentinel = channelevents.SeqBlockEnd
	}
	l.sessionCtxMu.RLock()
	sc := l.SessionContext
	l.sessionCtxMu.RUnlock()
	var uid string
	if sc != nil {
		uid = string(sc.AgentSessionUID)
	}
	return channelevents.PackSeq(l.lastAssistantTurnIndexValue(), sentinel), uid
}

// MarkPlanStoppedBestEffort closes the agent's plans state-kind on an
// interrupted termination that does NOT flow through the runner sequencer — the
// SIGTERM / admin-kill / supersede path in internal/cmd/runner, where the run context is
// already cancelled and the sequencer's seqMu-guarded applyEvent cannot run. It
// mirrors the sequencer's MarkPlanStopped effect directly: mark every
// non-terminal plan item stopped and publish a PauseCauseStopped plan snapshot
// so the channel surface reflects the stop before the pod dies. Best-effort and
// LOGGED on error — callers MUST pass a FRESH (non-cancelled) context, the run
// context being torn down. No-op for a planless / kubectl-driven session, or an
// unwired plan-activity publisher.
//
// It runs on internal/cmd/runner's runStopHandler goroutine, concurrent with the loop
// goroutine running Run's entry-setup (which reassigns l.SessionContext under
// sessionCtxMu.Lock), so the pointer is snapshotted under a brief RLock —
// mirroring appToolSessionContext — and released before the store/publish I/O
// below; only the pointer read races, never the calls themselves.
func (l *Loop) MarkPlanStoppedBestEffort(ctx context.Context) {
	if l == nil {
		return
	}
	l.sessionCtxMu.RLock()
	sc := l.SessionContext
	l.sessionCtxMu.RUnlock()
	if sc == nil {
		return
	}
	store, ok := plans.TryFrom(sc)
	if !ok {
		return // planless session: nothing to stop.
	}
	changed, err := store.MarkStopped(ctx)
	if err != nil {
		slog.Default().Info("MarkPlanStoppedBestEffort: MarkStopped failed (best-effort)",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", err.Error())
		return
	}
	if l.PublishPlanActivity == nil || len(changed) == 0 {
		return
	}
	seq, uid := l.seqForEmit(ctx, true)
	for _, p := range changed {
		l.PublishPlanActivity(ctx, p, true, channelevents.PauseCauseStopped, seq, uid)
	}
}

// flushRunDuration persists the current accumulated run-time to
// status.runDuration. Best-effort: logged, never fatal — an under-count on a
// failed flush is fail-open on measurement, not a silent drop.
func (l *Loop) flushRunDuration(ctx context.Context) {
	if l.RunClock == nil || l.Status == nil {
		return
	}
	besteffort.Log(slog.Default().Info, "flush run duration",
		l.Status.PatchRunDuration(ctx, l.RunClock.Elapsed()),
		"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name)
}

// runDurationFlushInterval is how often the runner persists accrued active
// run-time to status.runDuration during a long, non-yielding active turn — a
// crash backstop independent of the finer-grained progress heartbeat. The
// authoritative flush is OnAwaitYield; this only bounds run-time loss on an
// abrupt crash, so a coarse cadence is fine.
const runDurationFlushInterval = 30 * time.Second

// runDurationFlusher periodically persists run-time during a long active turn,
// so a crash between await-yields loses at most one interval of run-time.
func (l *Loop) runDurationFlusher(ctx context.Context, interval time.Duration) {
	if l.RunClock == nil || l.Status == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.flushRunDuration(ctx)
		}
	}
}

// OnAwaitYield is invoked by a parking meta tool — await_user_message, or a
// delegated child's ask_parent — immediately before it blocks: the agent has
// yielded to whoever it is waiting on. It publishes the paused turn-activity
// signal so channelsd disarms the silence watchdog for the whole wait instead of
// treating a Running-but-silent session as stuck, and writes the durable
// awaitingUserInputSince scalar as a backstop for the at-most-once NATS event.
// Best-effort: logs on error, never fails. Bound into meta.AwaitConfig.OnYield.
func (l *Loop) OnAwaitYield(ctx context.Context) {
	// Freeze the live progress clock for the whole await block, mirroring
	// enterApprovalPause: a yield waiting on the user is not "progress", and
	// the heartbeat must not keep emitting KindTurnProgress during the wait.
	l.progress.pause()
	l.RunClock.Pause()
	// Flush before the await blocks: the pod may be reaped while parked here, so
	// accrued run-time must be durable before we wait.
	l.flushRunDuration(ctx)
	l.emitActivity(ctx, true, channelevents.PauseCauseReply)
	if l.Status != nil {
		if err := l.Status.SetAwaitingUserInput(ctx); err != nil {
			slog.Default().Info("OnAwaitYield: set awaitingUserInputSince failed (best-effort)",
				"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", err.Error())
		}
	}
	// Within-Running await yield: pod stays alive, phase stays Running; this
	// sets the AwaitingUserInput flag in the lifecycle state machine.
	l.emitLifecycleEvent(ctx, lifecyclecore.AwaitYieldEntered{})
}

// OnAwaitResume is invoked when an inbound reply wakes a parking meta tool
// (await_user_message, or a delegated child's ask_parent); re-arms the active
// state. Also clears the durable awaitingUserInputSince scalar and, for a
// child that asked, the pending flag on status.parentExchange.
// Best-effort: logs on error, never fails. Bound into meta.AwaitConfig.OnResume.
func (l *Loop) OnAwaitResume(ctx context.Context) {
	// Re-enable the progress clock paused by OnAwaitYield.
	l.progress.resume()
	l.RunClock.Resume()
	l.emitActivity(ctx, false, "")
	if l.Status != nil {
		if err := l.Status.ClearAwaitingUserInput(ctx); err != nil {
			slog.Default().Info("OnAwaitResume: clear awaitingUserInputSince failed (best-effort)",
				"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", err.Error())
		}
		// A delegated child that yielded through ask_parent has just been
		// answered; mark the exchange no longer outstanding so the
		// SubagentRequest controller stops reporting the request as awaiting
		// its parent (and stops measuring it against the parent-reply bound).
		// The exchange NUMBER is kept, so the child's next question is
		// distinguishable from this one.
		if err := l.Status.ClearParentPending(ctx); err != nil {
			slog.Default().Info("OnAwaitResume: clear parentExchange.pending failed (best-effort)",
				"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", err.Error())
		}
	}
	// Real inbound reply woke the await; clear the AwaitingUserInput flag.
	l.emitLifecycleEvent(ctx, lifecyclecore.AwaitResumed{})
}

// enterApprovalPause records one more in-flight approval block and, on the
// first one, flips the plan card to paused. Refcounted so that when several
// approval-gated tool calls block concurrently in one turn, the card stays
// paused until the LAST resolves (see exitApprovalPause). The emit runs after
// the depth lock is released, so no outbound publish happens under the lock.
func (l *Loop) enterApprovalPause(ctx context.Context, cause string) {
	l.approvalPauseMu.Lock()
	l.approvalPauseDepth++
	first := l.approvalPauseDepth == 1
	l.approvalPauseMu.Unlock()
	if first {
		// Freeze the live progress clock while parked on a human — a wait
		// shouldn't read as "progress" (and shouldn't drive setStatus every
		// few seconds for the whole wait). Resumes in exitApprovalPause.
		l.progress.pause()
		l.RunClock.Pause()
		l.emitActivity(ctx, true, cause)
	}
}

// exitApprovalPause records one approval resolved and flips the plan card back
// to active only when the LAST concurrent approval resolves. Paired with
// enterApprovalPause; safe to call once per enter.
func (l *Loop) exitApprovalPause(ctx context.Context) {
	l.approvalPauseMu.Lock()
	if l.approvalPauseDepth > 0 {
		l.approvalPauseDepth--
	}
	last := l.approvalPauseDepth == 0
	l.approvalPauseMu.Unlock()
	if last {
		l.progress.resume()
		l.RunClock.Resume()
		l.emitActivity(ctx, false, "")
	}
}

// emitLifecycleSignal best-effort sends a lifecycle signal. Nil-safe
// on l.Mem. Errors are logged and swallowed; never abort the runner
// over a missed audit signal.
func (l *Loop) emitLifecycleSignal(ctx context.Context, kind memory.SignalKind, payload any) {
	if l.Mem == nil {
		return
	}
	scope := memory.Scope{Kind: "session", ID: l.SessionKey.Namespace + "/" + l.SessionKey.Name}
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			// Practically inert (lifecycle payloads are simple structs),
			// but log + drop the payload rather than silently letting
			// the signal fire with a misleading empty body.
			slog.Default().Info("emitLifecycleSignal: payload marshal failed; signal will fire with empty payload",
				"kind", string(kind), "session", scope.ID, "err", err.Error())
		} else {
			raw = b
		}
	}
	if err := l.Mem.SendSignal(ctx, memory.Signal{
		Kind: kind, Scope: scope, Payload: raw,
	}); err != nil {
		slog.Default().Info("memory.SendSignal lifecycle",
			"kind", string(kind), "session", scope.ID, "err", err.Error())
	}
}

// AddressableChannelKind returns the kind to stamp on an identity that names a
// PERSON: OutboundChannelKind when set, else ChannelKind.
//
// Addressing a human by the INPUT kind is wrong whenever input and output
// differ — a cron session's input is "bento", which has no user identity, so
// the interaction sender skips delivery and the approval is never asked.
//
// It is this SESSION's answer, not the delivering channel's. When the session's
// own outbound binding is not one a person reads — a conversational subagent's
// `agent` Channel — the outbound relay routes the card to an ancestor and
// re-stamps the recipients onto that channel's kind
// (pkg/channels/channelsd/outbound/recipient_kind.go). That correction is the
// relay's because only the relay knows where the card went.
func (l *Loop) AddressableChannelKind() string {
	if l.OutboundChannelKind != "" {
		return l.OutboundChannelKind
	}
	return l.ChannelKind
}
