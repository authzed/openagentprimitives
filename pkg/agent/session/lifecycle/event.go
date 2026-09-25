package lifecycle

// Event is a sealed union: the one typed input alphabet replacing the dual
// NATS/HTTP "signal" transports.
type Event interface{ isEvent() }

type baseEvent struct{}

func (baseEvent) isEvent() {}

// --- provisioning (operator-pre) ---
type SettingsAccepted struct{ baseEvent }
type PodReady struct{ baseEvent }
type Unschedulable struct{ baseEvent }         // surfaced; stays Pending
type ProvablyUnschedulable struct{ baseEvent } // → Failed

// RunnerPodRefused is the runner workload being REFUSED at creation — an
// admission verdict (a quota, a policy, a webhook), not a placement failure, so
// it is distinct from Unschedulable, where the workload exists and cannot be
// placed. Like Unschedulable it is surfaced and stays in the current phase: the
// operator retries, and the refusing condition may well be lifted.
//
// Message carries the refusal's own words. It is the only place they become
// durable — the condition that also carries them lives on a CR the session
// outlives — so a reader who has the log and not the CR can still say why the
// session never started.
type RunnerPodRefused struct {
	baseEvent
	Message string
}

// --- credentials (operator-pre) ---
type CredsMissing struct{ baseEvent }
type CredsLinked struct{ baseEvent }
type CredsTimeout struct{ baseEvent }

// --- identity choice (operator-pre) ---
type IdentityChoicePending struct{ baseEvent } // runner entered the choice gate
type IdentityChoiceResolved struct {           // user chose an identity
	baseEvent
	Mode string // "agent" | "userPassthrough"
	// ConfirmedBy is the channel identity (e.g. Slack user_id) of the human who
	// clicked the choice. "Who chose" is authorization-relevant, so it rides in
	// the signed lifecycle log alongside the resolved Mode. Empty on legacy
	// entries written before the field existed, and on any resolution whose
	// decision carried no approver id.
	ConfirmedBy string
}
type IdentityChoiceCancelled struct { // user clicked Cancel
	baseEvent
	// CancelledBy is the channel identity of the human who clicked Cancel. Same
	// audit rationale as IdentityChoiceResolved.ConfirmedBy; empty on legacy
	// entries or a decision with no approver id.
	CancelledBy string
}
type IdentityChoiceTimeout struct{ baseEvent } // deadline elapsed with no choice

// --- authority handoff ---
type RunnerClaimed struct{ baseEvent } // HANDOFF ①
type RunnerTerminal struct {           // HANDOFF ②
	baseEvent
	Phase  Phase  // Succeeded | Failed | Idle | AwaitingRetry
	Reason string // when Phase==Failed
	// Message is the human-readable half of a failure: what the runner would
	// tell a person, not the classification constant Reason carries. It is
	// recorded here because the CR's status — the only other place it lands —
	// can be deleted while this log cannot, and because a reader holding the
	// log alone (a workshop reading back a test run) otherwise learns THAT a
	// session failed and never why. Empty for a non-failure phase, and on
	// entries written before the field existed.
	Message string
}
type RunnerCrash struct{ baseEvent } // operator backstop

// Stopped is an explicit interrupted termination (SIGTERM from admin-kill /
// supersede / crash-kill). Distinct from a clean Succeeded or an Idle yield —
// it drives MarkPlanStopped to close in-flight plan items.
type Stopped struct{ baseEvent }

// --- turn (runner) ---
type TurnCompleted struct{ baseEvent }
type AgentWorkComplete struct {
	baseEvent
	// Kubectl records which terminal the work-complete resolved to: true when
	// the session COMPLETED (phase=Succeeded), false when it parked Idle. A
	// kubectl session is the common case of the former and names the field; a
	// delegated child is the other one, since its binding reaches its parent
	// rather than a person and so has no conversation to stay alive for.
	Kubectl bool
}

// --- hooks (runner) ---
type HookDeny struct {
	baseEvent
	Post bool // PostToolCall deny (result withheld, side-effect happened)
}
type HookHalt struct {
	baseEvent
	Reason string // ToolGuardHalt | ScopeReviewFailed | RunnerCrash
}

// --- decisions (runner or operator-pre for join) ---
type DecisionAsked struct {
	baseEvent
	RequestID string
	Kind      DecisionKind
}
type DecisionResolved struct {
	baseEvent
	RequestID string
	Approved  bool
	TimedOut  bool // distinct from a human deny (anti-confabulation)
}

// --- recovery / conversational ---
type ProviderError struct{ baseEvent }
type RetryRequested struct{ baseEvent }
type RetryTTLExpired struct{ baseEvent }
type WakeRequested struct{ baseEvent }
type ArchiveSweep struct{ baseEvent }

// Expired fails a session that has exceeded its wall-clock budget.sessionExpiration
// (measured from status.startedAt). Emitted by the operator's expiration sweep.
// Unlike ArchiveSweep it is NOT phase-guarded — a session past its lifetime fails
// from any live phase (the terminal-sticky guard in Transition protects sessions
// that already reached Succeeded/Failed).
type Expired struct{ baseEvent }

// Sleep scales an Idle channel session's compute to zero: the operator reaps all
// its pods but keeps the AgentSession Idle and wakeable. Only Idle sleeps; the
// fold is a no-op on any other phase (a stale sweep). Analogous to ArchiveSweep.
type Sleep struct{ baseEvent }

// AwaitYieldEntered / AwaitResumed are the within-Running await_user_message
// transitions (pod stays alive; phase stays Running). They set/clear the
// AwaitingUserInput flag (→ awaitingUserInputSince scalar) but DO NOT change phase.
// Distinct from IdleYield, which is the await TTL/ctx → IdleExit (pod exits).
type AwaitYieldEntered struct{ baseEvent }
type AwaitResumed struct{ baseEvent }

type IdleYield struct{ baseEvent }        // await TTL/ctx → Idle (pod exits, respawn-on-wake)
type ShareDeniedYield struct{ baseEvent } // PreResponse share-denied → Idle, not Failed

// --- in-flight (recorded, no phase change) ---

// Revoked records a single in-flight revocation in the signed lifecycle log.
// Kind matches the registered revocation.Invalidator kind string (e.g.
// "tool-origin"); Key is the kind-specific identity (e.g. "mcpserver/linear").
// Carries no payload in old log entries (both fields empty) — callers must
// treat empty Kind/Key as "revocation recorded without detail" and skip
// re-application on restart for those entries.
type Revoked struct {
	baseEvent
	Kind string // registered revocable-kind name, e.g. "tool-origin"
	Key  string // kind-specific identity, e.g. "mcpserver/linear"
}

type ScopeMutated struct{ baseEvent }
type RestartRequested struct{ baseEvent }

// Held parks a session for forensic review. Emitted by the AgentSession
// reconciler when it observes an unreleased SessionHold.
//
// At is an RFC3339 string, not a time.Time: this package imports no clock
// (imports_test.go enforces it), so the caller that owns the clock stamps it.
type Held struct {
	baseEvent
	Reason    string // platform-authored trip reason
	TrippedBy string // canonical subject, or "tripper/<name>" for an automated trip
	At        string // RFC3339
}

// Released returns a held session to Pending. Accepted ONLY from PhaseHeld, so
// a release cannot be forged from any other phase.
type Released struct {
	baseEvent
	ApprovedBy string // canonical subject of the human who cleared the card
	At         string // RFC3339
}
