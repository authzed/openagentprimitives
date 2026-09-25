package uiaction

import "github.com/authzed/openagentprimitives/pkg/channels/channelevents"

// State is one point on the agent-UI action lifecycle:
//
//	idle -> submitted -> awaiting_approval -> running ->
//	        { succeeded | failed | denied | expired | rate_limited }
//
// `idle` is deliberately absent: it is the BROWSER's state for a control nobody
// has clicked, with no server-side fact to record — persisting one would mean a
// memory write per rendered button. The browser's ActionPhase union
// (web/packages/agentui/src/actions.tsx) carries `idle` and every constant
// below; that union is the mirror, this is the authority.
type State string

const (
	StateSubmitted        State = "submitted"
	StateAwaitingApproval State = "awaiting_approval"
	StateRunning          State = "running"
	StateSucceeded        State = "succeeded"
	StateFailed           State = "failed"
	StateDenied           State = "denied"
	StateExpired          State = "expired"
	StateRateLimited      State = "rate_limited"
)

// StateFor maps ONE synchronous app-tool outcome to the ONE lifecycle state
// it means. It is the single place that mapping exists.
//
// timedOut is checked FIRST and outranks status: a lapsed approval deadline
// resolves to a Deny verdict inside the executor (pipeline.TimeoutDeny), so
// status alone cannot tell "the approver said no" from "nobody answered" — and
// those are DIFFERENT states with different UI ("denied" is final; "expired"
// re-enables the control). Only the runner's per-call approval observation can
// supply timedOut; StateFor has no notion of a deadline.
//
// requires_approval returns StateSubmitted, which is NOT terminal: the call was
// accepted and detached, not settled. IsTerminal is how a caller checks whether
// it must keep watching.
func StateFor(status string, isError, timedOut bool) State {
	if timedOut {
		return StateExpired
	}
	switch status {
	case channelevents.AppToolCallStatusOK:
		if isError {
			return StateFailed
		}
		return StateSucceeded
	case channelevents.AppToolCallStatusRequiresApproval:
		return StateSubmitted
	case channelevents.AppToolCallStatusDenied:
		return StateDenied
	case channelevents.AppToolCallStatusRateLimited:
		return StateRateLimited
	case channelevents.AppToolCallStatusNotFound:
		// A dead button (unopted/unknown tool) is a failure, not a denial —
		// the viewer never had anything to be denied.
		return StateFailed
	case channelevents.AppToolCallStatusError:
		return StateFailed
	default:
		// An AppToolCallStatus this function doesn't recognize is a version
		// skew, not a success — fail toward the state that re-enables the
		// control rather than one that looks like it worked.
		return StateFailed
	}
}

// IsTerminal reports whether s is an end state. A non-terminal record is one
// the browser must keep a control disabled for.
//
// Maintenance obligation: a new State constant must be added to this switch
// explicitly, or it silently becomes non-terminal — which for a genuinely
// terminal state means a control that never re-enables.
func IsTerminal(s State) bool {
	switch s {
	case StateSucceeded, StateFailed, StateDenied, StateExpired, StateRateLimited:
		return true
	default:
		return false
	}
}

// DisplayCopy renders s as browser-safe human prose for an `action:` binding.
// It carries no request id, no tool name, no memory-kind name, and no
// approver identity — nothing internal. addressedToViewer is the design
// spec's approval table in copy form: the viewer who must act is told so;
// everyone else is told only that something is pending, with no name
// attached (Content.Requester/approver identity never reaches this
// function).
func DisplayCopy(s State, addressedToViewer bool) string {
	switch s {
	case StateSubmitted:
		return "Request submitted."
	case StateAwaitingApproval:
		if addressedToViewer {
			return "Your approval is needed to continue."
		}
		return "Waiting on approval from someone else."
	case StateRunning:
		return "In progress."
	case StateSucceeded:
		return "Completed successfully."
	case StateFailed:
		return "The request failed. You can try again."
	case StateDenied:
		return "The request was denied."
	case StateExpired:
		return "The approval window expired. You can try again."
	case StateRateLimited:
		return "Too many requests right now. Try again shortly."
	default:
		// Two callers reach this arm, and for both the honest copy is NOTHING.
		// The zero State (no record yet — nobody clicked, or the newest record
		// names a different action or viewer) is the common one, and is what an
		// `action:` data binding resolves to on a Tier-0 page's FIRST paint; a
		// sentence there opens every such page with a status line about an
		// action nothing has happened to. A State constant this build does not
		// recognize (version skew) is no better off: any sentence would claim a
		// lifecycle point this code cannot name. It must NOT be a diagnostic
		// like "Unknown state." — this value goes straight in front of a viewer.
		return ""
	}
}
