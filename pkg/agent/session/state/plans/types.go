// Package plans is the session-state Kind for agent-declared multi-step plans.
// The Store holds one Plan per name; the update_plan meta tool is the only
// mutator.
package plans

import (
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// Status is the lifecycle of one plan item.
type Status string

const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusDone       Status = "done"
	StatusError      Status = "error"
	// StatusStopped is system-only: the runner marks non-terminal items stopped
	// on interrupted termination (SIGTERM / admin-kill / crash). Agents cannot
	// author this status.
	StatusStopped Status = "stopped"
)

// IsValid reports whether s is one of the five documented statuses (including
// the system-only StatusStopped so stopped plans round-trip on replay).
func (s Status) IsValid() bool {
	switch s {
	case StatusPending, StatusInProgress, StatusDone, StatusError, StatusStopped:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether this status is a settled outcome — the item is
// accounted for and will not change again. Done, error and stopped all are;
// pending and in_progress are not.
//
// One home for the set, because "which statuses are finished?" is asked from
// two directions: MarkStopped sweeps everything NOT terminal into stopped, and
// the plan-steps-complete completion requirement refuses to call a round done
// while anything NOT terminal is left. Two enumerations of one set is how those
// two drift.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusDone, StatusError, StatusStopped:
		return true
	default:
		return false
	}
}

// IsAgentSettable reports whether an agent (via update_plan) may set this status.
// StatusStopped is system-only and is excluded; IsValid still accepts it so
// a stopped plan replays correctly through ReplayNote.
func (s Status) IsAgentSettable() bool {
	switch s {
	case StatusPending, StatusInProgress, StatusDone, StatusError:
		return true
	default:
		return false
	}
}

// Plan is one named, ordered list of Items. Multiple plans coexist per
// session; addressing is by name. ParentPlan / ParentItem are soft refs
// — channels resolve at render time and fall back to flat rendering on
// a dangling target.
type Plan struct {
	// Name addresses the plan within the session; unique per session.
	Name string `json:"name"`
	// ParentPlan and ParentItem are soft refs to the owning plan item; empty
	// means a top-level plan.
	ParentPlan string `json:"parent_plan,omitempty"`
	ParentItem string `json:"parent_item,omitempty"`
	// Items is the ordered step list; the order is the rendered order.
	Items []Item `json:"items"`
	// UpdatedAt is when Update last changed the plan.
	UpdatedAt time.Time `json:"updated_at"`

	// Phases is the agent's declared phase list: what it intends to do, and
	// the authority it says each step needs.
	//
	// This is the WORKING plan — agent-authored, rewritten in full on every
	// update_plan call, and carrying ZERO authorization weight. Nothing here
	// gates anything. It becomes authority only when a human approves it, at
	// which point the ordered list is FROZEN into the append-only audit record
	// and phase identity becomes (frozen-plan digest, index).
	//
	// Absent phases is the ordinary single-phase shape, never "no authority".
	Phases []Phase `json:"phases,omitempty"`
}

// Phase is one declared step of a plan.
//
// Every string on this type and its children is agent-authored and UNTRUSTED.
// The Why fields exist so an approver and an auditor can read the agent's
// stated reasoning; they are rendered as the agent's claim, escaped, and never
// parsed, matched against, or allowed to influence an authorization decision.
//
// ID is likewise the agent's own name for this phase, used only to resolve
// Requires edges at freeze time and to group items for display. Authorization
// never keys on it — an agent that could name its own phases could transplant
// an approval between plans.
type Phase struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Why   string `json:"why"`

	// Requires gates selection on prior ENTRY of each named phase — never on
	// an agent's assertion that it finished something.
	Requires []PhaseRequirement `json:"requires,omitempty"`

	// Max is the entry budget. Nil means the default (count 1): agents are
	// told to plan for a single entry unless they explicitly justify more, so
	// an approver sees one-shot as the norm and anything else as an argued
	// exception. A pointer, so "unset" stays distinguishable from an explicit
	// count that the schema would reject anyway.
	Max *PhaseMax `json:"max,omitempty"`

	// Budget bounds the GOVERNED TOOL CALLS this phase may make. Nil means the
	// phase declared none and the runtime default applies. Present means the
	// agent is asking for a specific number and Why is mandatory — the same
	// shape as Max, so an approver reads an unusual budget the same way they
	// read an unusual entry count: as an argued exception.
	Budget *PhaseBudget `json:"budget,omitempty"`

	// Permissions is the declared CLASS ceiling for this phase.
	Permissions []PhasePermission `json:"permissions,omitempty"`

	// Slots is the declared INSTANCE axis for this phase. Carried through the
	// schema now; the instance axis itself lands with the slots spec.
	Slots []PhaseSlot `json:"slots,omitempty"`
}

// PhaseRequirement declares that this phase may not be entered until the named
// phase has been entered. Phase is the AUTHORED id; freezing resolves it to an
// index exactly once, so the gate never resolves a name at decision time.
type PhaseRequirement struct {
	Phase string `json:"phase"`
	Why   string `json:"why"`
}

// PhaseBudget bounds how much work a phase may do inside its ceiling.
//
// Counted in governed tool calls. This is the only mechanism that catches
// rabbit-holing: an agent grinding inside a perfectly legal readonly ceiling
// breaks no permission rule, so no permission model can see it.
type PhaseBudget struct {
	Calls int    `json:"calls"`
	Why   string `json:"why"`
}

// PhaseMax bounds how many times a phase may be entered.
type PhaseMax struct {
	Count int    `json:"count"`
	Why   string `json:"why"`
}

// PhasePermission is one declared permission handle plus the agent's
// justification for needing it. Why is REQUIRED by the schema: it is what
// makes an over-broad ceiling visible and self-incriminating on the card.
type PhasePermission struct {
	Handle string `json:"handle"`
	Why    string `json:"why"`
}

// PhaseSlot is one declared instance-axis slot type plus its justification.
type PhaseSlot struct {
	Type string `json:"type"`
	// ID is the concrete resource the phase will act on, when the agent can
	// name it. Empty means "not known yet" — legal, and surfaced on the card as
	// a phase that will need a second approval.
	ID  string `json:"id,omitempty"`
	Why string `json:"why"`
}

// Item is one step within a Plan.
type Item struct {
	// ID is agent-supplied and stable across calls; it is the addressing key.
	ID string `json:"id"`
	// Label is the mutable human description of the step.
	Label string `json:"label"`
	// Status is the item's lifecycle state; see the Status constants.
	Status Status `json:"status"`
	// Details is optional agent-supplied plain text, rendered as a collapsible
	// detail pane by rich channels and ignored by markdown-only ones.
	Details string `json:"details,omitempty"`
	// Output is the same, for the step's produced result.
	Output string `json:"output,omitempty"`
	// OperationID is set by the Store when the item goes pending → in_progress,
	// and is what the agent cites as operation_id on sandbox calls.
	OperationID string `json:"operation_id,omitempty"`

	// Phase groups this item under a declared phase for DISPLAY. It is a
	// presentation heading and nothing more: the plan gate never reads it.
	//
	// It has to be that way. update_plan submits full state on every call and
	// is Passthrough, so any structure the gate read from this document would
	// be agent-determined by construction — the exact bug three earlier
	// designs died on (Item.Status, then operation_id, then phase ids). The
	// active phase is a runtime-recorded selection over a frozen list.
	Phase string `json:"phase,omitempty"`
}

// FindItem returns a copy of the Item with the matching ID, if any.
func (p Plan) FindItem(id string) (Item, bool) {
	for _, it := range p.Items {
		if it.ID == id {
			return it, true
		}
	}
	return Item{}, false
}

// ToEnvelopePayload maps this snapshot to the channel-facing PlanUpdatePayload —
// the SAME shape the live update_plan publish emits, so a resumed plan card is
// byte-identical to the one shown live. Diff/Paused/PauseCause are per-emit
// context rather than snapshot state: the live caller sets them, a reload leaves
// them zero, and neither affects the rendered card.
func (p Plan) ToEnvelopePayload() channelevents.PlanUpdatePayload {
	items := make([]channelevents.PlanItemRef, 0, len(p.Items))
	for _, it := range p.Items {
		items = append(items, channelevents.PlanItemRef{
			ID:          it.ID,
			Label:       it.Label,
			Status:      string(it.Status),
			Details:     it.Details,
			Output:      it.Output,
			OperationID: it.OperationID,
			Phase:       it.Phase,
		})
	}
	// nil rather than an empty slice when the plan declares no staging, so a
	// renderer testing presence sees "no phases declared" rather than "declared
	// zero phases" — and the JSON omits the key entirely.
	var phases []channelevents.PlanPhaseRef
	if len(p.Phases) > 0 {
		phases = make([]channelevents.PlanPhaseRef, 0, len(p.Phases))
		for _, ph := range p.Phases {
			phases = append(phases, channelevents.PlanPhaseRef{ID: ph.ID, Label: ph.Label})
		}
	}
	return channelevents.PlanUpdatePayload{
		PlanName:   p.Name,
		ParentPlan: p.ParentPlan,
		ParentItem: p.ParentItem,
		Items:      items,
		UpdatedAt:  p.UpdatedAt,
		Phases:     phases,
	}
}

// Content is the mutable body of a plan as submitted to Update: the full new
// state, not a delta.
//
// It exists so the phase list can travel with the items without growing
// Update into a five-positional-parameter call. Both fields are FULL state —
// update_plan submits everything every time and the store diffs by id, so an
// omitted Phases means "this plan now declares no phases", never "leave the
// existing ones alone".
//
// Phases ride on a plan, and an empty Items list DELETES the plan (the
// long-standing update_plan contract), so a phases-only submission with no
// items stores nothing. That is intentional: phases describe how a plan's work
// is staged, and there is no work to stage without items.
type Content struct {
	Items  []Item
	Phases []Phase
}

// ParentRef is the input shape for parent linkage on Update. Empty Plan
// (and empty Item) means "no parent". Plan set without Item is rejected
// at the Store layer, mirroring the JSON schema.
type ParentRef struct {
	Plan string
	Item string
}

// Diff is the structural change Update applied. Returned to callers and
// included in the channelevents envelope.
type Diff struct {
	// Added and Removed are item IDs; empty means no items of that kind changed.
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	// Renamed and StatusChanged carry before/after values per item ID.
	Renamed       []RenameDiff       `json:"renamed,omitempty"`
	StatusChanged []StatusChangeDiff `json:"status_changed,omitempty"`
}

// RenameDiff is one item's label change.
type RenameDiff struct {
	ID   string `json:"id"`
	From string `json:"from"`
	To   string `json:"to"`
}

// StatusChangeDiff is one item's status transition.
type StatusChangeDiff struct {
	ID   string `json:"id"`
	From Status `json:"from"`
	To   Status `json:"to"`
}

// UpdateResult is what Store.Update returns and what update_plan
// surfaces to the LLM.
type UpdateResult struct {
	PlanName string `json:"plan"`
	Diff     Diff   `json:"diff"`
	// InProgress is set only when exactly one item is in_progress after the
	// update; nil otherwise.
	InProgress *InProgressRef `json:"in_progress,omitempty"`
}

// InProgressRef is populated on the UpdateResult when the post-update
// plan has exactly one item in_progress. Channels and the agent both
// read this field — the agent uses OperationID for sandbox calls.
type InProgressRef struct {
	ItemID      string `json:"item_id"`
	OperationID string `json:"operation_id"`
}
