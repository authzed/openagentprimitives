package v1alpha1

import "time"

// WakeEligible reports whether a parked AgentSession's runner pod has exited,
// so stamping the wake-requested-at annotation will make the operator respawn
// it. Idle and AwaitingRetry park with the pod gone. The other Resume phases
// keep a live or starting pod (Running/Pending) or wait on the credential-link
// flow (AwaitingCredentials), where a respawn would either duplicate a live
// runner or start one before its credential exists.
//
// An archive-swept session also parks with its pod reaped, but at Succeeded —
// terminal-looking, yet asleep rather than finished. ArchivedBySweep is the one
// signal separating the two; gating on phase alone left such a session with no
// wake annotation, so channelsd routed the message in, no runner was ever
// respawned, and the turn stranded with no reply.
//
// Lives here rather than in any one consumer because three packages must agree
// on it: channelsd decides whether to stamp, the operator decides whether to
// respawn, and the runner decides whether its own post-Idle wake request would
// be honored. A fourth copy of this switch is how they drift apart.
func WakeEligible(sess *AgentSession) bool {
	if sess == nil {
		return false
	}
	switch sess.Status.Phase {
	case AgentSessionPhaseIdle, AgentSessionPhaseAwaitingRetry:
		return true
	case AgentSessionPhaseSucceeded:
		return ArchivedBySweep(sess)
	default:
		return false
	}
}

// WakePending reports whether an unconsumed wake request is already stamped on
// the session: the wake-requested-at annotation is present and newer than
// status.lastWakeAt (which the operator sets to "now" each time it consumes
// one). An unparsable annotation is treated as no request — a wake we cannot
// order against lastWakeAt could otherwise fire on every reconcile forever.
//
// Says nothing about phase: pair it with WakeEligible to decide whether the
// operator will act on the request.
func WakePending(sess *AgentSession) bool {
	if sess == nil {
		return false
	}
	req := sess.Annotations[AnnotationWakeRequestedAt]
	if req == "" {
		return false
	}
	if sess.Status.LastWakeAt == nil {
		return true
	}
	reqT, err := time.Parse(time.RFC3339Nano, req)
	if err != nil {
		return false
	}
	return reqT.After(sess.Status.LastWakeAt.Time)
}

// UIServePending reports whether an unconsumed UI-serve request is stamped on
// the session: the ui-serve-requested-at annotation is present and newer than
// status.lastUIServeAt (which the operator sets each time it acts on one).
//
// The mirror of WakePending, deliberately down to the unparsable-annotation
// rule: a request we cannot order against the consumed marker is treated as no
// request, because the alternative is a spawn that fires on every reconcile
// forever.
//
// A SEPARATE predicate over a SEPARATE annotation because the two requests
// differ in what they do to a conversation (see AnnotationUIServeRequestedAt):
// on one key, a dashboard's request would consume a pending message wake.
//
// Says nothing about phase: pair it with WakeEligible, the same precondition
// both requests share.
func UIServePending(sess *AgentSession) bool {
	if sess == nil {
		return false
	}
	req := sess.Annotations[AnnotationUIServeRequestedAt]
	if req == "" {
		return false
	}
	if sess.Status.LastUIServeAt == nil {
		return true
	}
	reqT, err := time.Parse(time.RFC3339Nano, req)
	if err != nil {
		return false
	}
	return reqT.After(sess.Status.LastUIServeAt.Time)
}
