package channelkinds

// outcomeNames is the wire encoding of Outcome. These strings cross a NATS
// boundary (channelevents.ViewMessageResultPayload.Outcome) and appear in
// operator-facing logs, so they are stable API: never rename one, only append.
//
// Outcome is an int enum; without this, string(o) would yield a control-char
// rune ("\x01") rather than a label, and string→Outcome would not compile.
var outcomeNames = map[Outcome]string{
	OutcomeUnknown:            "unknown",
	OutcomeRouted:             "routed",
	OutcomeDeniedByPermission: "denied_by_permission",
	OutcomeInternalError:      "internal_error",
	OutcomeNoActiveSession:    "no_active_session",
	OutcomeForkPending:        "fork_pending",
	OutcomeRefused:            "refused",
	OutcomeHandledNoAgent:     "handled_no_agent",

	OutcomeThreadOwnedByAnotherAgent: "thread_owned_by_another_agent",
}

// String returns the stable wire label. An Outcome outside the declared set
// renders as "unknown" rather than a bare integer — a garbled value must not
// look like a real outcome.
func (o Outcome) String() string {
	if s, ok := outcomeNames[o]; ok {
		return s
	}
	return "unknown"
}

// ParseOutcome maps a wire label back to an Outcome. The bool reports whether
// the label was recognized.
//
// FAIL CLOSED: an unrecognized label yields (OutcomeUnknown, false), never a
// guess. Callers MUST surface !ok as an error — a version-skewed or garbled
// outcome that silently decoded to OutcomeRouted would render a permission
// denial as a successful send.
func ParseOutcome(s string) (Outcome, bool) {
	for o, name := range outcomeNames {
		if name == s {
			return o, true
		}
	}
	return OutcomeUnknown, false
}
