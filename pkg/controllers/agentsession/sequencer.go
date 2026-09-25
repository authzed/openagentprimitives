package agentsession

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/memory"
	lifecyclekind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// conditionBecameTrue reports whether condType is True on sess but was not True
// on original (the start-of-reconcile snapshot). Used to emit edge-triggered
// lifecycle events exactly once — on the reconcile that flips the condition —
// rather than re-appending the same transition every reconcile. A nil original
// (a direct call without a reconcile snapshot) treats the current True as the
// edge.
func conditionBecameTrue(original, sess *spiceboxv1alpha1.AgentSession, condType string) bool {
	if !conditions.IsTrue(sess.Status.Conditions, condType) {
		return false
	}
	if original == nil {
		return true
	}
	return !conditions.IsTrue(original.Status.Conditions, condType)
}

// lifecycleScope is the memory scope holding a session's transition log.
func lifecycleScope(sess *spiceboxv1alpha1.AgentSession) memory.Scope {
	return memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
}

// maxRunnerTurn returns the highest memTurnIndex any runner-region event in the
// (already-decoded) log has reached, or 0 when the runner has written nothing
// yet. The operator anchors its own boundary events to this turn so they sort
// into the right runner incarnation (see operatorOrderKey).
func maxRunnerTurn(ordered []lifecyclekind.OrderedEvent) int {
	maxTurn := 0
	for i := range ordered {
		k := ordered[i].Key
		if k.Region != string(lifecyclecore.RegionRunner) {
			continue
		}
		if t := channelevents.UnpackTurn(k.Seq); t > maxTurn {
			maxTurn = t
		}
	}
	return maxTurn
}

// operatorOrderKey derives the operator's cross-publisher fold-ordering key for
// ev (see pkg/memory/kinds/lifecycle.OrderKey). The operator has no turn loop,
// so it anchors each event to observedTurn — the highest runner turn seen so far
// — and classifies it by region:
//   - pre-runner events (provisioning, credential handshake, the handoff-in
//     RunnerClaimed) sort at the turn's START block. RegionOperatorPre sorts
//     before RegionRunner at equal Seq, so a stale operator RunnerClaimed can
//     never fold after the runner's terminal event of the same turn and
//     re-promote a rested session.
//   - post-runner events (wake to the next turn, the crash/retry/archive
//     backstops) sort at the turn's END block, after the runner's live events.
//
// Known narrow gap — a same-turn provider-error retry: an idle-wake respawns the
// runner on a NEW user turn (memTurnIndex T+1), so its post-wake events carry a
// higher Seq than this WakeRequested (anchored at turn T's END block) and fold to
// Running — correct. A provider-error RETRY, though, replays the SAME turn T (no
// new user turn), so the post-retry terminal is stamped at (T, END) just like the
// pre-retry one, while WakeRequested — also at (T, END) but in the operator_post
// region — outranks both same-turn runner terminals. The raw fold therefore lands
// on WakeRequested last and projects Pending even though the retry completed to
// Idle. The OrderKey cannot finely order this: within turn T there is no sub-
// position above the END block, and post-region always outranks the runner region
// at equal Seq, so nothing the runner stamps for the replayed turn can sort after
// this WakeRequested. Giving WakeRequested a lower anchor would instead break the
// pre-retry ordering (it must stay after the pre-retry terminal), and bumping the
// turn on a retry would desync memTurnIndex from the transcript and the shared
// channel status stream. It is left un-finely-ordered on purpose: the ONLY
// consumer that acts on this phase is the operator's steady-state reconcilePhase,
// which refuses to demote the runner-written Idle back to a bootstrap/incomplete-
// log Pending — so the CR-visible phase is the correct Idle. See
// TestFold_CompletedRetry_RawPendingButReconciledToIdle.
func operatorOrderKey(ev lifecyclecore.Event, observedTurn int, uid string) lifecyclekind.OrderKey {
	region := lifecyclecore.RegionOperatorPre
	block := channelevents.SeqBlockStart
	switch ev.(type) {
	case lifecyclecore.WakeRequested, lifecyclecore.RunnerCrash,
		lifecyclecore.RetryTTLExpired, lifecyclecore.ArchiveSweep:
		region = lifecyclecore.RegionOperatorPost
		block = channelevents.SeqBlockEnd
	}
	return lifecyclekind.OrderKey{
		Seq:        channelevents.PackSeq(observedTurn, block),
		Region:     string(region),
		SessionUID: uid,
	}
}

// foldLifecycle reads the session's signed transition log and folds it to the
// current lifecycle state. With no log wired (LifecycleMemory nil — test
// fixtures that don't exercise the audit path) it returns the zero state, which
// normalizes to Pending. The fold reads only typed transition events; recorded
// signals in the same kind are skipped.
//
// The read is narrowed to THIS AgentSession instance (lifecyclekind.
// ForIncarnation). lifecycleScope is keyed by namespace/name and its append-only
// entries are permanent, so a session re-created under a name a previous one
// used — a redelivered webhook, a re-fired trigger — reads a log that already
// carries that instance's events, terminal ones included.
func (r *Reconciler) foldLifecycle(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (lifecyclecore.State, error) {
	if r.LifecycleMemory == nil {
		return lifecyclecore.State{}.OrDefault(), nil
	}
	events, err := lifecyclekind.EventsForIncarnation(ctx, r.LifecycleMemory, lifecycleScope(sess), string(sess.UID))
	if err != nil {
		return lifecyclecore.State{}, fmt.Errorf("read lifecycle log %s/%s: %w", sess.Namespace, sess.Name, err)
	}
	return lifecyclecore.Fold(events), nil
}

// applyEvent is the operator's sequencer step: fold the session's transition
// log, apply ev through the pure core, execute the returned effects, and write
// the projected phase onto sess.Status (persisted by the caller's applyStatus).
//
// Effect handling in the operator regions (pre-runner provisioning + post-runner
// backstop):
//   - AppendLog: append the typed event to the signed log (durable, immediate).
//   - ProjectStatus: set sess.Status.Phase from the re-folded projection. Other
//     projected fields (conditions, failure reason) stay operator-bookkept at
//     their existing call sites for now; this writes only the phase.
//   - Notify: a best-effort operator wake/notify nudge — the operator already
//     publishes its own monitoring/wake signals at the call sites, so this is
//     recorded for completeness.
//   - StopLoop / ArmTimer / CancelTimer / Unpark / ReissuePending / MarkPlanStopped:
//     runner-region effects. The operator has no turn loop, decision waits, or
//     in-memory plans store, so it records (does not execute) them — the runner
//     interprets these in its own sequencer.
//
// When LifecycleMemory is nil the event is not persisted (logged), and the
// projection is computed from the session's current CR phase as the floor so the
// phase write still lands.
func (r *Reconciler) applyEvent(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, ev lifecyclecore.Event) error {
	logger := log.FromContext(ctx)

	var (
		prior        lifecyclecore.State
		observedTurn int
	)
	if r.LifecycleMemory != nil {
		full, err := lifecyclekind.ReadOrdered(ctx, r.LifecycleMemory, lifecycleScope(sess))
		if err != nil {
			return fmt.Errorf("read lifecycle log %s/%s: %w", sess.Namespace, sess.Name, err)
		}
		// This instance's events only — see foldLifecycle. maxRunnerTurn reads the
		// same narrowed log deliberately: anchoring the operator's own event to a
		// turn a PREVIOUS instance reached would stamp it far ahead of anything
		// this session's runner will write, and it would sort after every one of
		// them for the rest of the session.
		ordered := lifecyclekind.ForIncarnation(full, string(sess.UID))
		events := make([]lifecyclecore.Event, len(ordered))
		for i := range ordered {
			events[i] = ordered[i].Event
		}
		prior = lifecyclecore.Fold(events)
		observedTurn = maxRunnerTurn(ordered)
	} else {
		prior = lifecyclecore.State{Phase: lifecyclecore.Phase(sess.Status.Phase)}.OrDefault()
	}

	// Reconcile the folded prior phase with the authoritative CR phase before
	// applying the transition. The signed log can lag the CR — the operator
	// restarted, an inmem/postgres read has not yet observed a co-writer's
	// append, or the runner has not yet written its advancing event (e.g.
	// IdleYield) to the shared log — in which case the fold floors to the
	// bootstrap Pending while the CR already carries a more-advanced,
	// runner-written phase (Idle/Running). Applying a phase-inheriting event
	// (Sleep, Unschedulable, ArchiveSweep, and any other event that inherits
	// its prior phase — the input-side guard below covers them all generically,
	// not just the ones named here) on that stale Pending prior would then
	// project Pending and clobber the CR — demoting a slept-Idle session and
	// re-provisioning the pods the sleep reaper just tore down. reconcilePhase
	// suppresses exactly that incomplete-log demotion, the same guard the
	// steady-state projector (derivePhase → reconcilePhase) applies to the
	// read side; here it corrects the transition's input so a phase-moving
	// event (WakeRequested's Idle→Pending, CredsLinked's unpark, any terminal
	// fail) still forces its target phase from the reconciled prior.
	prior.Phase = lifecyclecore.Phase(reconcilePhase(string(prior.Phase), sess.Status.Phase))

	// Reconcile the folded Archived bit against the CR too. archive.go writes
	// Phase=Succeeded DIRECTLY, before the runner has published its IdleYield
	// event — so the fold has not yet observed Idle and cannot have folded an
	// ArchiveSweep that set Archived. The bit therefore lags exactly as the phase
	// does above, and the CR (whose Idle-condition reason is the sole authority
	// for "parked by sweep") is the source of truth. Without this, a WakeRequested
	// on a CR-Succeeded-but-fold-Pending session would carry Archived=false into
	// Transition, and the sticky-terminal guard would never let the wake through.
	prior.Archived = spiceboxv1alpha1.ArchivedBySweep(sess)

	orderKey := operatorOrderKey(ev, observedTurn, string(sess.UID))

	next, effects := lifecyclecore.Transition(prior, ev)
	for _, eff := range effects {
		switch e := eff.(type) {
		case lifecyclecore.AppendLog:
			if r.LifecycleMemory == nil {
				logger.Info("LifecycleMemory not configured; transition not persisted to the signed log",
					"session", sess.Namespace+"/"+sess.Name, "event", fmt.Sprintf("%T", e.Event))
				continue
			}
			if err := lifecyclekind.Append(ctx, r.LifecycleMemory, lifecycleScope(sess), e.Event, r.now(), orderKey); err != nil {
				return fmt.Errorf("append lifecycle event %T for %s/%s: %w", e.Event, sess.Namespace, sess.Name, err)
			}
		case lifecyclecore.ProjectStatus:
			sess.Status.Phase = lifecyclecore.Project(next).Phase
			// A folded IdentityChoiceResolved{Mode} (appended by the runner) carries
			// the resolved identity mode through the fold; project it so the operator
			// learns the choice via the signed log, without a separate channel. Guard
			// on non-empty so events that don't touch it (every other transition)
			// leave a previously-resolved mode intact.
			if next.EffectiveIdentityMode != "" {
				sess.Status.EffectiveIdentityMode = next.EffectiveIdentityMode
			}
		case lifecyclecore.Notify:
			logger.V(1).Info("lifecycle notify (operator records; nudge handled at call site)",
				"session", sess.Namespace+"/"+sess.Name, "reason", e.Reason)
		default:
			// Runner-region effect (StopLoop/ArmTimer/CancelTimer/Unpark/
			// ReissuePending/MarkPlanStopped). The operator does not own a turn
			// loop, decision waits, or a plans store — record for completeness.
			logger.V(1).Info("lifecycle effect not executed in operator region",
				"session", sess.Namespace+"/"+sess.Name, "effect", fmt.Sprintf("%T", eff))
		}
	}
	return nil
}
