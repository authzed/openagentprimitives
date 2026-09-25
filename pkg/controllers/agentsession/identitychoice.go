package agentsession

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// defaultIdentityChoiceTimeout is the fallback deadline for an ask|dynamic
// session parked in AwaitingIdentityChoice when the AgentClass leaves
// spec.identityChoiceTimeout unset. It matches the apiserver default.
const defaultIdentityChoiceTimeout = 30 * time.Minute

// reconcileIdentityChoice is the identity-choice gate. It runs immediately
// before the passthrough gate and mirrors its (result, proceed, err) contract:
// proceed=false means the caller must return (result, err) without running the
// rest of the reconcile.
//
// Behavior by AgentClass identity mode:
//
//   - Static (agent | userPassthrough | "" → agent): mirror spec.identityMode
//     onto status.effectiveIdentityMode once, then proceed. Downstream identity
//     logic (the passthrough gate, the runner boot switch) reads the effective
//     mode, so a static class resolves it here on the first pass and never
//     enters AwaitingIdentityChoice.
//
//   - Interactive (ask | dynamic): the initiating user's choice arrives as a
//     signed IdentityChoiceResolved event the RUNNER appends to the lifecycle
//     log (folding to phase=Pending + State.EffectiveIdentityMode). The runner
//     spawns and drives the 3-way prompt; the operator parks the session in
//     AwaitingIdentityChoice and enforces the choice deadline as a backstop.
//     Once the choice is made this gate proceeds so the passthrough gate can
//     run (a no-op for an agent choice; parks AwaitingCredentials for a
//     userPassthrough choice).
//
// Unlike the passthrough gate — whose unpark signal is external CR state (the
// starter's UserIdentity) re-read every reconcile — this gate's unpark signal
// is the runner-appended log event. Because a parked reconcile returns before
// the reconcile's end-of-loop fold runs, this gate folds the log itself to
// learn the resolved mode and mirror it onto status. With no log wired (the
// LifecycleMemory-nil unit fixtures) the fold is skipped and the
// fixture-provided status drives the branches.
func (r *Reconciler) reconcileIdentityChoice(
	ctx context.Context, sess *spiceboxv1alpha1.AgentSession, ac *spiceboxv1alpha1.AgentClass,
) (ctrl.Result, bool, error) {
	mode := ac.Spec.IdentityMode

	// Static modes: mirror spec → status once, then proceed.
	if mode != spiceboxv1alpha1.IdentityModeAsk && mode != spiceboxv1alpha1.IdentityModeDynamic {
		if sess.Status.EffectiveIdentityMode == "" {
			eff := mode
			if eff == "" {
				eff = spiceboxv1alpha1.IdentityModeAgent // "" defaults to agent downstream
			}
			sess.Status.EffectiveIdentityMode = eff
			return ctrl.Result{}, true, r.applyStatus(ctx, sess)
		}
		return ctrl.Result{}, true, nil
	}

	// Interactive modes. Learn the resolved choice from the signed log before
	// deciding: the runner's IdentityChoiceResolved event lives there, not on
	// status, and this gate returns before the reconcile's end-of-loop fold once
	// it parks. Skipped when no log is wired (unit fixtures) — the fixture status
	// then drives the branches below.
	if sess.Status.EffectiveIdentityMode == "" && r.LifecycleMemory != nil {
		state, err := r.foldLifecycle(ctx, sess)
		if err != nil {
			return ctrl.Result{}, false, err
		}
		if state.EffectiveIdentityMode != "" {
			sess.Status.EffectiveIdentityMode = state.EffectiveIdentityMode
		}
	}

	// Choice made (post-resolve, or a row-14 re-spawn with the choice already on
	// status): fall through. The passthrough gate re-parks if the choice was
	// userPassthrough and is a no-op for agent.
	if sess.Status.EffectiveIdentityMode != "" {
		return ctrl.Result{}, true, nil
	}

	// Not parked yet: the runner has not emitted IdentityChoicePending. Let it
	// spawn and drive the prompt; the operator's end-of-loop fold moves the phase
	// to AwaitingIdentityChoice once the runner parks.
	if sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice {
		return ctrl.Result{}, true, nil
	}

	// Parked in AwaitingIdentityChoice. Stamp the park time on first sight and
	// enforce the choice deadline. This is a backstop — the runner also stops
	// waiting and emits IdentityChoiceTimeout/Cancelled — but a crashed or absent
	// runner must not strand the session here forever.
	if sess.Status.IdentityChoiceParkedAt == nil {
		now := metav1.Now()
		sess.Status.IdentityChoiceParkedAt = &now
		if err := r.applyStatus(ctx, sess); err != nil {
			return ctrl.Result{}, false, err
		}
	}

	timeout := defaultIdentityChoiceTimeout
	if ac.Spec.IdentityChoiceTimeout != nil && ac.Spec.IdentityChoiceTimeout.Duration > 0 {
		timeout = ac.Spec.IdentityChoiceTimeout.Duration
	}
	remaining := time.Until(sess.Status.IdentityChoiceParkedAt.Add(timeout))
	if remaining <= 0 {
		// Deadline elapsed with no choice → Failed. Edge-gate the log append so a
		// re-reconcile of an already-Failed session does not re-append the event
		// (mirrors parkAwaitingCredentials' CredsTimeout handling). applyEvent
		// projects only the phase (→ Failed); the failure reason + finishedAt stay
		// operator-bookkept here, per the sequencer's ProjectStatus contract.
		if !isTerminalPhase(sess.Status.Phase) {
			if err := r.applyEvent(ctx, sess, lifecyclecore.IdentityChoiceTimeout{}); err != nil {
				return ctrl.Result{}, false, err
			}
		}
		sess.Status.FailureReason = spiceboxv1alpha1.ReasonIdentityChoiceTimeout
		now := metav1.Now()
		sess.Status.FinishedAt = &now
		// Set the Failed=True condition here too — the backstop must produce a
		// COMPLETE terminal status (phase + condition + reason). The runner's own
		// WriteFailed is idempotent and skips the condition once the phase is
		// already terminal, so when this backstop wins the timeout race (or when
		// the runner is absent/crashed — the backstop's raison d'être) nothing
		// else sets the Failed condition. Without this, a timed-out session ends
		// phase=Failed but with no Failed=True condition, and every consumer that
		// keys off the condition (channelsd session_watcher, `oap`, the UI) would
		// miss the failure. Mirrors markBootFailed / the RunnerCrash backstop.
		conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
			Type:    spiceboxv1alpha1.AgentSessionConditionFailed,
			Status:  metav1.ConditionTrue,
			Reason:  spiceboxv1alpha1.ReasonIdentityChoiceTimeout,
			Message: "no identity was chosen before the choice deadline elapsed",
		})
		return ctrl.Result{}, false, r.applyStatus(ctx, sess)
	}
	return ctrl.Result{RequeueAfter: remaining}, false, nil
}
