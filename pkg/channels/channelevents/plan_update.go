package channelevents

import (
	"errors"
	"fmt"
	"time"
)

// PlanUpdatePayload is the body of a KindPlanUpdate envelope. The
// runner emits one of these on every successful update_plan call;
// channels render from Items (full snapshot is authoritative) and may
// use Diff for log lines or rendering hints.
type PlanUpdatePayload struct {
	// PlanName identifies the plan within the session; Validate requires it.
	PlanName string `json:"planName"`
	// ParentPlan / ParentItem nest this plan under one item of another plan.
	// Both or neither — Validate rejects a ParentItem with no ParentPlan.
	ParentPlan string `json:"parentPlan,omitempty"`
	ParentItem string `json:"parentItem,omitempty"`
	// Items is the FULL snapshot after the update and is what surfaces
	// render; an empty slice means the plan now has no items.
	Items []PlanItemRef `json:"items"`
	// Diff is what changed in this update — a rendering hint and log aid
	// only, never the thing to render from.
	Diff PlanDiff `json:"diff"`
	// UpdatedAt is when the runner produced this snapshot.
	UpdatedAt time.Time `json:"updatedAt"`
	// Paused reports that the agent was NOT actively turning when this
	// snapshot was emitted; false (the zero value) means active. When true,
	// channels neutralise the in_progress item and show a PauseCause banner.
	Paused bool `json:"paused,omitempty"`
	// PauseCause drives the banner text. Empty when Paused is false. One of the
	// PauseCause* constants; channels fall back to a generic banner on unknown.
	PauseCause string `json:"pauseCause,omitempty"`

	// Phases is the staging the agent declared for this plan, in order, so a
	// channel can group Items under the same headings the approval card speaks
	// in. Absent means the plan declared no staging — the ordinary single-phase
	// shape — and a renderer should fall back to a flat list.
	//
	// Every string here is AGENT-AUTHORED and untrusted, exactly like the item
	// labels beside them: display it, escape it, never parse it or key anything
	// on it. Authorization never reads this — the plan gate keys on a frozen
	// list recorded at approval time, never on the live document, which
	// update_plan rewrites in full on every call.
	Phases []PlanPhaseRef `json:"phases,omitempty"`
}

// PlanPhaseRef is the channel-facing shape of one declared plan phase.
//
// Deliberately just identity and a heading. The authority a phase carries —
// its permissions, its named instances, its budgets — reaches a human through
// the approval card, which is computed from the FROZEN plan; repeating it here,
// from the live agent-rewritable document, would put an untrusted second
// account of the same thing next to the trusted one.
type PlanPhaseRef struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// PauseCause* are the documented values for PlanUpdatePayload.PauseCause.
// They are channel-agnostic; each channel kind maps them to its own banner.
const (
	PauseCauseApproval        = "awaiting_approval"
	PauseCauseLeakageApproval = "awaiting_leakage_approval"
	PauseCauseReply           = "awaiting_reply"
	PauseCauseRetry           = "awaiting_retry"
	// PauseCauseIdentityChoice marks a session parked in AwaitingIdentityChoice
	// while the initiating user chooses (agent vs userPassthrough) at the
	// identityMode=ask|dynamic gate. Keeps channelsd's silence watchdog extended
	// through the human wait.
	PauseCauseIdentityChoice = "awaiting_identity_choice"
	PauseCauseFailed         = "failed"
	// PauseCauseIdle marks the turn loop's idle exit — the agent finished what
	// it was asked and the session is parked, still open for the next message.
	// Distinct from PauseCauseReply, which means the agent is WAITING for a
	// reply it asked for (await_user_message): a surface that offers "answer the
	// agent" on awaiting_reply would otherwise offer it at the end of every
	// turn, over the agent's closing statement.
	PauseCauseIdle = "idle"
	// PauseCauseComplete marks a terminal, succeeded session — the agent's work
	// is done (not awaiting anything) and the session is over. Distinct from
	// PauseCauseIdle, where the agent also finished but the session stays open
	// for the next message, and from failed.
	PauseCauseComplete = "complete"
	// PauseCauseStopped marks a terminal, interrupted session — the agent's pod
	// was stopped before it could complete in-flight work.
	PauseCauseStopped = "stopped"
)

// PlanItemRef is the channel-facing shape of one plan item.
// Status is one of "pending" | "in_progress" | "done" | "error".
type PlanItemRef struct {
	// ID is the item's stable key across snapshots; Validate requires it.
	ID     string `json:"id"`
	Label  string `json:"label"`
	Status string `json:"status"`
	// Details and Output are optional agent-supplied plain text; surfaces
	// with rich rendering show them as collapsible panes, others may ignore.
	Details string `json:"details,omitempty"`
	Output  string `json:"output,omitempty"`
	// OperationID links this item to the operation working it, so an
	// operation_activity tick can be shown against the right row. Empty when
	// no operation claimed the item.
	OperationID string `json:"operationId,omitempty"`

	// Phase names the PlanPhaseRef this item is grouped under, for DISPLAY
	// only. Empty means ungrouped, which is legal alongside grouped siblings —
	// a renderer must place those items somewhere rather than drop them.
	Phase string `json:"phase,omitempty"`
}

// PlanDiff mirrors plans.Diff in serialized form. Every field is optional; an
// all-empty PlanDiff means the snapshot changed nothing structural.
type PlanDiff struct {
	// Added and Removed carry item IDs.
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	// Renamed and StatusChanged carry the before/after per item ID.
	Renamed       []PlanRenameDiff       `json:"renamed,omitempty"`
	StatusChanged []PlanStatusChangeDiff `json:"statusChanged,omitempty"`
}

// PlanRenameDiff is one item's label change: From → To, keyed by item ID.
type PlanRenameDiff struct {
	ID   string `json:"id"`
	From string `json:"from"`
	To   string `json:"to"`
}

// PlanStatusChangeDiff is one item's status change: From → To, keyed by item
// ID. Both are PlanItemRef.Status values.
type PlanStatusChangeDiff struct {
	ID   string `json:"id"`
	From string `json:"from"`
	To   string `json:"to"`
}

// Validate runs cheap shape checks. Callers should call before
// publishing to catch programmer errors at the boundary.
func (p PlanUpdatePayload) Validate() error {
	if p.PlanName == "" {
		return errors.New("planName is required")
	}
	if p.ParentItem != "" && p.ParentPlan == "" {
		return errors.New("parentItem set without parentPlan; either both or neither")
	}
	for i, it := range p.Items {
		if it.ID == "" {
			return fmt.Errorf("items[%d]: id is required", i)
		}
	}
	return nil
}
