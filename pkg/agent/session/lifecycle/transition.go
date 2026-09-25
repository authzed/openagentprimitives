package lifecycle

// Transition is the pure, total, deterministic core. Unknown (state,event)
// pairs are no-ops that still AppendLog (audit completeness) — never a panic.
func Transition(s State, e Event) (State, []Effect) {
	s = s.OrDefault()
	// Terminal phases are sticky: Succeeded/Failed only log, never move — with
	// exactly one exception. The long-idle archive sweep parks a channel-attached
	// session at Succeeded to shed its pods, but that session is ASLEEP, not
	// finished (State.Archived marks the distinction), so an archived Succeeded
	// plus a genuine WakeRequested falls through to the normal wake path below,
	// clearing Archived as it un-parks. Every other terminal combination stays
	// sticky — a real completion, any Failed session, and any non-wake event on
	// an archived Succeeded.
	if s.Phase == PhaseSucceeded || s.Phase == PhaseFailed {
		_, isWake := e.(WakeRequested)
		archivedResume := s.Phase == PhaseSucceeded && s.Archived && isWake
		if !archivedResume {
			return s, []Effect{AppendLog{Event: e}}
		}
		s.Archived = false
	}
	switch ev := e.(type) {
	case SettingsAccepted:
		return s, project(e)
	case CredsMissing:
		s.Phase = PhaseAwaitingCredentials
		return s, project(e)
	case CredsLinked:
		s.Phase = PhasePending
		return s, project(e)
	case CredsTimeout:
		return fail(s, "CredentialLinkTimeout", e)
	case IdentityChoicePending:
		s.Phase = PhaseAwaitingIdentityChoice
		return s, project(e)
	case IdentityChoiceResolved:
		// The fold does not whitelist Mode — validation of "agent" vs
		// "userPassthrough" lives at the emit site (the runner only ever emits
		// the two valid values); this stays a total, unopinionated projection.
		s.EffectiveIdentityMode = ev.Mode
		s.Phase = PhasePending // unpark; the passthrough gate re-parks if Mode=userPassthrough
		return s, project(e)
	case IdentityChoiceCancelled:
		return fail(s, "IdentityChoiceCancelled", e)
	case IdentityChoiceTimeout:
		return fail(s, "IdentityChoiceTimeout", e)
	case Unschedulable:
		return s, append3(AppendLog{Event: e}, ProjectStatus{}, Notify{Reason: "unschedulable"})
	case RunnerPodRefused:
		// Same shape as Unschedulable and for the same reason: the session has
		// not started, and may yet — the refusing admission rule can be relaxed
		// while the operator is still retrying — so this records and surfaces
		// without moving the phase.
		return s, append3(AppendLog{Event: e}, ProjectStatus{}, Notify{Reason: "runner_pod_refused"})
	case ProvablyUnschedulable:
		return fail(s, "Unschedulable", e)
	case PodReady:
		return s, []Effect{AppendLog{Event: e}} // ① arrives as RunnerClaimed
	case RunnerClaimed:
		s.Phase = PhaseRunning
		s.Region = RegionRunner
		return s, project(e)
	case Held:
		// A hold parks from any live phase. The terminal-sticky guard above has
		// already returned for Succeeded/Failed, so reaching here means the
		// session is live and holdable. The reason/tripper are preserved only in
		// the logged event (via project(e)) — FailureReason is NOT set here: that
		// field is documented as carrying a reason only when Phase==Failed, and a
		// held session is not failed.
		s.Phase = PhaseHeld
		return s, project(e)
	case Released:
		// Only from Held. Accepting it anywhere else would let a release recorded
		// out of order resurrect a session nobody held.
		if s.Phase != PhaseHeld {
			return s, []Effect{AppendLog{Event: e}}
		}
		s.Phase = PhasePending
		return s, project(e)
	default:
		return transitionLive(s, ev) // Tasks 5–7 extend this
	}
}

func project(e Event) []Effect        { return []Effect{AppendLog{Event: e}, ProjectStatus{}} }
func append3(a, b, c Effect) []Effect { return []Effect{a, b, c} }

func fail(s State, reason string, e Event) (State, []Effect) {
	s.Phase = PhaseFailed
	s.FailureReason = reason
	return s, project(e)
}

func transitionLive(s State, e Event) (State, []Effect) {
	switch ev := e.(type) {
	case TurnCompleted:
		// Counters are projected by the caller after the full turn; log only here.
		return s, []Effect{AppendLog{Event: e}}
	case HookDeny:
		// Surface every hook-deny to status (no-silent-errors), regardless of pre/post.
		s.Gated++
		return s, project(e)
	case HookHalt:
		// Halt must stop the turn loop — StopLoop is mandatory. No further LLM calls after a halt.
		s.Phase = PhaseFailed
		s.FailureReason = ev.Reason
		return s, []Effect{AppendLog{Event: e}, ProjectStatus{}, StopLoop{}}
	case AwaitYieldEntered:
		// await_user_message yielded at entry: pod stays alive, phase stays Running.
		// Sets the within-Running flag → projects to the awaitingUserInputSince scalar.
		s.AwaitingUserInput = true
		return s, project(e)
	case AwaitResumed:
		// Real inbound reply woke the await; clear the flag, stay Running.
		s.AwaitingUserInput = false
		return s, project(e)
	case ShareDeniedYield:
		// PreResponse share-denied: yields to Idle (distinct flavor — NOT a failure).
		s.Phase = PhaseIdle
		s.AwaitingUserInput = false
		return s, project(e)
	case AgentWorkComplete:
		if ev.Kubectl {
			// kubectl session exits cleanly → Succeeded.
			s.Phase = PhaseSucceeded
		} else {
			// channel-attached session: pod exits, operator respawns on wake.
			s.Phase = PhaseIdle
		}
		s.AwaitingUserInput = false
		return s, project(e)
	case IdleYield:
		// await TTL/ctx → IdleExit: the pod exits (distinct from AwaitYieldEntered).
		s.Phase = PhaseIdle
		s.AwaitingUserInput = false
		return s, project(e)
	case WakeRequested:
		// Idle → Pending: operator respawns the runner. Clear Slept so the lazy
		// provisioning gate re-creates the reaped pods on this turn.
		s.Phase = PhasePending
		s.Slept = false
		s.Region = RegionOperatorPre
		return s, project(e)
	default:
		return transitionDecision(s, ev) // Task 6
	}
}

// transitionDecision handles all five human-in-the-loop decision kinds through one
// shared path. The five kinds differ only by their decisionParams entry — there is no
// per-kind branching outside that table. This prevents drift where a new kind is wired
// for one path (ask) but forgotten for another (timeout, resolve).
func transitionDecision(s State, e Event) (State, []Effect) {
	switch ev := e.(type) {
	case DecisionAsked:
		params := decisionParams[ev.Kind]
		s = addPending(s, PendingDecision{RequestID: ev.RequestID, Kind: ev.Kind})
		if params.setsScopeReview {
			s.ScopeReviewPending = true
		}
		// join stays in its current phase (Idle) but the pending entry is now first-class
		// visible in the projected status. All other kinds promote to AwaitingDecision
		// so the runner can block further tool dispatch until the decision arrives.
		if ev.Kind != DecisionJoin {
			s.Phase = PhaseAwaitingDecision
		}
		return s, []Effect{AppendLog{Event: e}, ProjectStatus{}, ArmTimer{RequestID: ev.RequestID, Kind: ev.Kind}}

	case DecisionResolved:
		next, kind, found := removePending(s, ev.RequestID)
		if !found {
			// Late or duplicate — already handled (e.g. after a restart). Audit only.
			return s, []Effect{AppendLog{Event: e}}
		}
		s = next
		// Re-derive the flag from the remaining set; do not rely on the caller to clear it.
		s.ScopeReviewPending = anyScopeReview(s)

		// scope_review timeout fails closed: a cold-start gate that timed out cannot be
		// silently bypassed — the session must fail so the operator can investigate.
		if ev.TimedOut && decisionParams[kind].onTimeout == timeoutFail {
			s.Phase = PhaseFailed
			s.FailureReason = "ScopeReviewFailed"
		} else if len(s.Pending) == 0 && s.Phase == PhaseAwaitingDecision {
			// Last pending decision resolved: unpark back to the pre-decision phase.
			// For join, preDecisionPhase is Idle and the phase was never changed, so this
			// branch does not execute — the equality guard keeps Idle stable.
			s.Phase = decisionParams[kind].preDecisionPhase
		}
		return s, []Effect{
			AppendLog{Event: e}, ProjectStatus{}, CancelTimer{RequestID: ev.RequestID},
			Unpark{RequestID: ev.RequestID, Approved: ev.Approved, TimedOut: ev.TimedOut},
		}

	default:
		return transitionTerminal(s, e)
	}
}

// maxRetry caps retry attempts: ProviderError on the fifth attempt → Failed instead
// of another AwaitingRetry cycle. The count is inclusive: attempts > maxRetry fires.
const maxRetry = 5

// transitionTerminal handles the operator-post region: terminal handoffs, retry
// budget sub-machine, in-flight notices (no phase change), and archive sweep.
func transitionTerminal(s State, e Event) (State, []Effect) {
	switch ev := e.(type) {
	case RunnerTerminal:
		// Runner hands authority back to operator-post with its final phase.
		s.Phase = ev.Phase
		s.Region = RegionOperatorPost
		effs := project(e)
		if ev.Phase == PhaseFailed {
			s.FailureReason = ev.Reason
			// Interrupted (Failed) termination must stop the plans state-kind
			// so the runner sequencer can close in-flight work items. Clean Succeeded
			// does not emit this — the work was completed, not abandoned.
			effs = append(effs, MarkPlanStopped{})
		}
		return s, effs

	case RunnerCrash:
		// Operator backstop: the runner pod vanished. Clear every pending decision so
		// none strands on crash; mark plans stopped on interrupted termination.
		s.Pending = nil
		s.ScopeReviewPending = false
		s.Phase = PhaseFailed
		s.FailureReason = "RunnerCrash"
		s.Region = RegionOperatorPost
		return s, append(project(e), MarkPlanStopped{})

	case Stopped:
		// Explicit interrupted termination (SIGTERM from admin-kill / supersede / crash-
		// kill). Clears pending and marks the plan stopped. If a terminal phase had
		// already been reached, the sticky guard at the top of Transition handles it
		// (MarkPlanStopped was already emitted when the terminal was first reached).
		s.Pending = nil
		s.ScopeReviewPending = false
		s.Phase = PhaseFailed
		s.FailureReason = "Stopped"
		s.Region = RegionOperatorPost
		return s, append(project(e), MarkPlanStopped{})

	case ProviderError:
		// Retry budget: increment first, then check. This way the transition from
		// attempt maxRetry (the last allowed retry) to attempt maxRetry+1 is the
		// one that exhausts the budget — consistent with "attempts > maxRetry".
		s.RetryAttempts++
		if s.RetryAttempts > maxRetry {
			return fail(s, "RetryBudgetExhausted", e)
		}
		s.Phase = PhaseAwaitingRetry
		s.Region = RegionOperatorPost
		return s, project(e)

	case RetryRequested:
		// An authorized operator action (human or controller) explicitly requested a
		// retry after the session entered AwaitingRetry. Move back to the operator-pre
		// queued state so the pod respawn cycle can run again.
		s.Phase = PhasePending
		s.Region = RegionOperatorPre
		return s, project(e)

	case RetryTTLExpired:
		// Retry TTL: the AwaitingRetry window elapsed without a RetryRequested.
		return fail(s, "RetryTimeout", e)

	case ArchiveSweep:
		// Idle sessions that have been quiet long enough are archived. Only Idle
		// advances to Succeeded — any other phase (e.g., Running) means the sweep
		// arrived stale and we leave the phase alone.
		if s.Phase == PhaseIdle {
			s.Phase = PhaseSucceeded
			// Mark this Succeeded as parked-by-sweep, not agent-completed: the
			// session is asleep and a later WakeRequested resumes it (the
			// sticky-terminal guard lets exactly that combination through). Setting
			// the bit only inside this branch keeps a stale sweep — which left the
			// phase alone — from marking a live session as archived.
			s.Archived = true
		}
		return s, project(e)

	case Sleep:
		// Idle sessions quiet past the sleep grace shed all pods but stay Idle
		// (wakeable, same thread). Only Idle sleeps; any other phase means the
		// sweep arrived stale and we leave the phase alone.
		if s.Phase == PhaseIdle {
			s.Slept = true
		}
		return s, project(e)

	case Expired:
		// Wall-clock lifetime cap exceeded. fail() sets Phase=Failed +
		// FailureReason; the sticky guard at the top of Transition already
		// no-ops this for terminal sessions.
		return fail(s, "SessionExpired", e)

	case Revoked:
		// In-flight: policy revocation is recorded and the operator is notified, but
		// the runner controls its own phase — no phase change here.
		return s, []Effect{AppendLog{Event: e}, Notify{Reason: "revoked"}}

	case ScopeMutated, RestartRequested:
		// In-flight: audit-only, no phase change. The runner or channel layer handles
		// the behavioral response; the lifecycle state machine just records the event.
		return s, []Effect{AppendLog{Event: e}}

	default:
		// Unknown event: audit-only no-op. Keeps Transition total (never panics) and
		// preserves a complete event log even for future event kinds.
		_ = ev
		return s, []Effect{AppendLog{Event: e}}
	}
}
