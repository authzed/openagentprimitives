package runner

// sequencer.go makes the runner the lifecycle sequencer for the LIVE region: it
// folds the session's signed transition log, applies an Event through the pure
// core (pkg/agent/session/lifecycle), and executes the returned effects. The
// pure core performs no I/O; this file owns all of it.
//
// Two-publisher log: the operator writes this kind as system:operator (pre/post
// regions); the runner writes it as session:<ns/name> through LifecycleMemory.
// Folding reads both publishers' typed events ordered oldest-first.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// lifecycleScope is the memory scope holding this session's transition log.
func (l *Loop) lifecycleScope() memory.Scope {
	return memory.Scope{Kind: "session", ID: l.SessionKey.Namespace + "/" + l.SessionKey.Name}
}

// sessionIncarnationUID is the AgentSession instance this Loop runs, used to
// narrow every fold of the shared lifecycleScope to this instance's own events
// (see lifecycle.ForIncarnation — the scope survives the CR, so a session
// re-created under a used name reads a log holding its predecessor's events).
//
// Empty when internal/cmd/runner did not pre-build the SessionContext (unit tests,
// kubectl dev runs), which folds the whole log exactly as before.
//
// Reads the SessionContext pointer under sessionCtxMu, mirroring seqForEmit:
// Run reassigns it on the loop goroutine while off-loop callers reach the
// sequencer. LOCK ORDER: callers hold seqMu, so this is the established
// seqMu -> sessionCtxMu direction.
func (l *Loop) sessionIncarnationUID() string {
	l.sessionCtxMu.RLock()
	sc := l.SessionContext
	l.sessionCtxMu.RUnlock()
	if sc == nil {
		return ""
	}
	return string(sc.AgentSessionUID)
}

// foldLifecycle reads the session's signed transition log and folds it to the
// current lifecycle state. With no log wired (LifecycleMemory nil — unit tests
// and kubectl-driven dev runs) it returns the zero state, which normalizes to
// Pending. Only typed transition events are folded; recorded signals in the
// same kind are skipped.
//
// The read goes through l.lifecycleLog, which keeps what it has already decoded
// and asks the backend only for the tail. This is a per-EVENT fold (applyEvent
// calls it before every append) over an append-only kind with no cap or TTL, so
// reading the WHOLE log each call — as package-level lifecycle.Events does —
// would cost O(E²) decodes and bytes over HTTP inside seqMu, E unbounded in
// session age. lifecycle.IncrementalReader is safe against the two-publisher
// hazard (the operator appends to this same scope while the runner is live)
// because nothing is seeded once and trusted, and it re-reads the whole log
// every lifecycle.FullReadEvery reads to repair — and loudly log — anything a
// clock-skewed publisher hid from a tail read.
//
// LOCK ORDER: applyEvent, the only caller, always holds seqMu. The reader's own
// mutex is therefore uncontended and is a leaf (Read releases it before
// returning, nothing under it takes an AP lock), so seqMu → reader.mu cannot
// invert against the sequencer's other order, seqMu → sessionCtxMu. Do not call
// foldLifecycle without seqMu held.
//
// claimAndRecover deliberately does NOT use the reader: it runs once per Run,
// off seqMu, and restart recovery wants a guaranteed-complete read.
func (l *Loop) foldLifecycle(ctx context.Context) (lifecyclecore.State, error) {
	if l.LifecycleMemory == nil {
		return lifecyclecore.State{}.OrDefault(), nil
	}
	// The returned slice is freshly built by Events (the reader's own held slice
	// is never handed out here), and Fold only reads it.
	events, err := l.lifecycleLog.EventsForIncarnation(ctx, l.LifecycleMemory, l.lifecycleScope(), l.sessionIncarnationUID())
	if err != nil {
		return lifecyclecore.State{}, err
	}
	return lifecyclecore.Fold(events), nil
}

// applyEvent is the runner's live-region sequencer step: fold the transition
// log, apply ev through the pure core, and execute the returned effects. It
// returns stop=true when the core asks the turn loop to end (StopLoop).
//
// Effect handling in the runner (live) region:
//   - AppendLog: append the typed event to the signed log via LifecycleMemory
//     (publisher session:<ns/name>). Without it the operator's post-region
//     fold-based projection re-derives Running and reverts the runner's
//     terminal/idle phase.
//   - StopLoop: signal the caller to break Run.
//   - MarkPlanStopped: mark every non-terminal item stopped in the plans
//     state-kind store and publish the PauseCauseStopped plan activity.
//   - ProjectStatus / ArmTimer / CancelTimer / Unpark / Notify / ReissuePending:
//     recorded, not acted on — the StatusPatcher already writes CR status at
//     each terminal/turn site, the operator owns the fold-based projection, the
//     approval.Orchestrator owns live park/unpark, and channelsd owns nudges.
//
// Every call serializes on l.seqMu so the per-publisher signing chain advances
// monotonically across concurrent dispatch goroutines.
func (l *Loop) applyEvent(ctx context.Context, ev lifecyclecore.Event) (stop bool) {
	l.seqMu.Lock()
	defer l.seqMu.Unlock()

	prior, err := l.foldLifecycle(ctx)
	if err != nil {
		// A fold failure must not strand the loop: apply from the default
		// state so StopLoop/guard decisions still fire, and log so the missing
		// durable history is explicable.
		slog.Default().Info("lifecycle: fold for transition failed; applying from default state",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
			"event", fmt.Sprintf("%T", ev), "err", err.Error())
		prior = lifecyclecore.State{}.OrDefault()
	}

	_, effects := lifecyclecore.Transition(prior, ev)
	for _, eff := range effects {
		switch e := eff.(type) {
		case lifecyclecore.AppendLog:
			if l.LifecycleMemory == nil {
				continue // no signed log wired (tests / kubectl dev runs)
			}
			key := l.lifecycleOrderKey(ctx, e.Event)
			if aerr := lifecycle.Append(ctx, l.LifecycleMemory, l.lifecycleScope(), e.Event, time.Now().UTC(), key); aerr != nil {
				slog.Default().Info("lifecycle: append transition failed (best-effort)",
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
					"event", fmt.Sprintf("%T", e.Event), "err", aerr.Error())
			}
		case lifecyclecore.StopLoop:
			stop = true
		case lifecyclecore.MarkPlanStopped:
			// Interrupted termination: mark every non-terminal plan item stopped.
			// The snapshot is published before the caller (fail / Stopped path)
			// emits the paused turn_activity, so channelsd sees the
			// PauseCauseStopped plan update before yield-suppression kicks in.
			//
			// SessionContext is snapshotted under sessionCtxMu.RLock to guard
			// against a future off-loop MarkPlanStopped-producing event racing
			// Run's entry-setup write. applyEvent already holds seqMu; the lock
			// order seqMu → sessionCtxMu matches seqForEmit's, so no inversion.
			l.sessionCtxMu.RLock()
			sc := l.SessionContext
			l.sessionCtxMu.RUnlock()
			store, hasPlan := plans.TryFrom(sc)
			if !hasPlan {
				// No plans state registered (kubectl-driven / minimal sessions).
				break
			}
			changed, merr := store.MarkStopped(ctx)
			if merr != nil {
				slog.Default().Info("lifecycle: MarkPlanStopped failed (best-effort)",
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
					"err", merr.Error())
				break
			}
			if l.PublishPlanActivity != nil && len(changed) > 0 {
				seq, uid := l.seqForEmit(ctx, true)
				for _, p := range changed {
					l.PublishPlanActivity(ctx, p, true, channelevents.PauseCauseStopped, seq, uid)
				}
			}
		default:
			// ProjectStatus / ArmTimer / CancelTimer / Unpark / Notify /
			// ReissuePending — owned by the StatusPatcher, the approval
			// orchestrator, and channelsd in the current wiring.
			_ = e
		}
	}
	return stop
}

// lifecycleOrderKey derives the runner's cross-publisher fold-ordering key for
// ev (see pkg/memory/kinds/lifecycle.OrderKey). All runner events carry
// RegionRunner; the difference is the block position within the turn:
//   - a mid-turn event dispatched inside a tool's Execute context carries the
//     call's exact (memTurnIndex, blockIndex) via seqForEmit's IDs path;
//   - the handoff-in RunnerClaimed sorts at the turn's START block, before that
//     turn's tool activity and any same-turn operator RunnerClaimed;
//   - every other loop-side event (turn boundary, terminal, idle/await) sorts at
//     the turn's END block, after that turn's tool activity.
//
// The memTurnIndex is monotonic across resume, so a wake respawn's RunnerClaimed
// (later turn) sorts after the prior turn's terminal — a genuine Idle→Running
// wake still folds to Running — while a stale same-turn claim sorts before it.
func (l *Loop) lifecycleOrderKey(ctx context.Context, ev lifecyclecore.Event) lifecycle.OrderKey {
	_, isClaim := ev.(lifecyclecore.RunnerClaimed)
	seq, uid := l.seqForEmit(ctx, !isClaim)
	return lifecycle.OrderKey{Seq: seq, Region: string(lifecyclecore.RegionRunner), SessionUID: uid}
}

// emitLifecycleEvent applies ev for its durable-log + audit side effect only,
// discarding the StopLoop signal. Used at sites that own their control flow
// (terminal returns, turn boundary, await yield/resume, decision ask/resolve).
func (l *Loop) emitLifecycleEvent(ctx context.Context, ev lifecyclecore.Event) {
	_ = l.applyEvent(ctx, ev)
}

// idleYieldEventFor picks the lifecycle event to record when a channel-attached
// session yields to Idle on a Terminal+IdleExit tool result. A share-denied
// yield (respond_to_user's information-leakage block set ShareDenied on the
// result) records the distinct ShareDeniedYield so the audit trail can tell a
// blocked-share yield apart from a plain await_user_message idle; every other
// idle exit records IdleYield. The resulting phase is Idle either way.
func idleYieldEventFor(shareDenied bool) lifecyclecore.Event {
	if shareDenied {
		return lifecyclecore.ShareDeniedYield{}
	}
	return lifecyclecore.IdleYield{}
}

// EmitRevoked records a revocation in the session's signed lifecycle log. Safe
// to call from any goroutine (serialized via seqMu). The live per-tool guard
// (revocation_guard PreToolCall hook) fires independently — this is additive
// audit + restart-recovery data, not a substitute for the live-path invalidation.
// kind is the registered revocable-kind name (e.g. "tool-origin"); key is the
// kind-specific identity (e.g. "mcpserver/linear"). Both are recorded in the
// event so claimAndRecover can re-apply the revocation on runner restart.
func (l *Loop) EmitRevoked(ctx context.Context, kind, key string) {
	l.emitLifecycleEvent(ctx, lifecyclecore.Revoked{Kind: kind, Key: key})
}

// claimAndRecover is the runner's restart-recovery + authority-handoff step, run
// once at the top of Run: fold the signed log, re-apply recorded revocations so
// previously-revoked subjects stay denied after respawn, re-arm open decision
// waits in the approval orchestrator (so a user's response to a request
// published before the respawn is still captured), and emit RunnerClaimed
// (handoff ①). With no log wired the fold is empty and only the RunnerClaimed
// emit runs.
//
// Revocation re-apply: every Revoked event with a non-empty Kind/Key is
// dispatched by kind through RevocationRegistry to that kind's Invalidate.
// Entries with empty fields are skipped — their live effect was already gone
// when the pod that recorded them exited.
//
// Decision re-arm: for each RequestID in ReissuePending, a goroutine registers
// with the approval orchestrator (Approval.Await) and records DecisionResolved
// in the lifecycle log when a decision arrives, so DeliverDecision is not a
// no-op for old requestIDs still displayed on the channel surface.
func (l *Loop) claimAndRecover(ctx context.Context) {
	if l.LifecycleMemory != nil {
		// A full read, NOT l.lifecycleLog: this runs once per Run (so it is not
		// the O(E²) path foldLifecycle was), it runs off seqMu, and re-applying
		// revocations and re-arming decision waits both need a
		// guaranteed-complete log rather than a lookback-bounded tail.
		events, err := lifecycle.EventsForIncarnation(ctx, l.LifecycleMemory, l.lifecycleScope(), l.sessionIncarnationUID())
		if err != nil {
			slog.Default().Info("lifecycle: read log for restart re-fold failed (best-effort)",
				"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", err.Error())
		} else {
			sessionRef := l.SessionKey.Namespace + "/" + l.SessionKey.Name

			// Re-apply EVERY recorded revocation, dispatching by kind through the
			// same revocation.Registry the live ap.revocation subscriber uses.
			// Matching a single literal kind here silently drops credential
			// revocations on restart: the fresh broker cache re-resolves the
			// revoked credential and the fresh MCPTools re-freeze it. An
			// unrecognised kind is skipped and logged, never fatal — a newer
			// runner may have written a kind this binary does not register.
			if l.RevocationRegistry != nil {
				for _, ev := range events {
					r, ok := ev.(lifecyclecore.Revoked)
					if !ok || r.Kind == "" || r.Key == "" {
						continue
					}
					inv, found := l.RevocationRegistry.Lookup(r.Kind)
					if !found {
						slog.Default().Info("lifecycle: no invalidator for replayed revocation kind; skipping",
							"session", sessionRef, "kind", r.Kind, "key", r.Key)
						continue
					}
					if err := inv.Invalidate(r.Key); err != nil {
						slog.Default().Info("lifecycle: re-apply revocation failed (best-effort)",
							"session", sessionRef, "kind", r.Kind, "key", r.Key, "err", err.Error())
					}
				}
			}

			// Sub-feature I7: re-arm open decision waits from FoldWithReissue.
			if _, effs := lifecyclecore.FoldWithReissue(events); len(effs) > 0 {
				seen := map[string]bool{}
				for _, eff := range effs {
					rp, ok := eff.(lifecyclecore.ReissuePending)
					if !ok || len(rp.Pending) == 0 {
						continue
					}
					if l.Approval == nil {
						slog.Default().Info("lifecycle: restart re-fold found pending decisions but Approval not wired; skipping re-arm",
							"session", sessionRef, "count", len(rp.Pending))
						continue
					}
					for _, pd := range rp.Pending {
						if seen[pd.RequestID] {
							continue // dedupe across effects (should not happen but guard it)
						}
						seen[pd.RequestID] = true
						// Spawn a goroutine that blocks on the orchestrator for this
						// requestID with no OnPublish (don't re-publish — the channel
						// already shows the request). When a decision arrives (the user
						// clicks the old approval button) record it in the lifecycle log
						// so the signed audit chain stays coherent and the pending entry
						// is cleared from the projected state.
						reqID := pd.RequestID
						go func() {
							req := approval.Request{
								RequestID:  reqID,
								SessionRef: sessionRef,
								// No OnPublish: the channel-side surface (Slack approval
								// message) was already published before the crash. Re-
								// publishing would send a duplicate message to the user.
							}
							d, awaitErr := l.Approval.Await(ctx, req)
							timedOut := awaitErr != nil && errors.Is(awaitErr, context.DeadlineExceeded)
							if awaitErr != nil && !timedOut {
								// Context cancelled (session terminated) or publish error
								// — not an error worth surfacing; the session is ending.
								return
							}
							l.emitLifecycleEvent(ctx, lifecyclecore.DecisionResolved{
								RequestID: reqID,
								Approved:  awaitErr == nil && d.Approved,
								TimedOut:  timedOut,
							})
						}()
					}
					slog.Default().Info("lifecycle: restart re-fold re-armed pending decision waits",
						"session", sessionRef, "count", len(seen))
				}
			}
		}
	}
	// Handoff ①: the runner claims the live region.
	l.emitLifecycleEvent(ctx, lifecyclecore.RunnerClaimed{})
}
