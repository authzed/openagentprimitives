package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// SlotPin is a DISPLAY-ONLY mirror of one single-occupancy slot's current
// commitment — the instance SpiceDB's slot_pin relation pins the session to
// for ResourceType, written by authz.GrantSlots (via SlotPinner.EnsurePin) on
// a first bind and repointed by authz.BindApproved's move path
// (SlotPinner.MovePin) when an approved plan moves it.
//
// SpiceDB is the enforcement source of truth: the slot_pin tuple is what a
// tool-call Check and GrantSlots' own single-occupancy gate actually consult.
// Nothing that authorizes a bind or a call reads this field — it exists so
// `oap session show` and operator logs can see the commitment without a live
// SpiceDB read, and it can lag or be absent (e.g. no status writer wired)
// without changing what is actually enforced.
type SlotPin struct {
	// ResourceType is the slot's declared resource type, and the key
	// UpsertSlotPin replaces by — a single-occupancy slot holds at most one
	// instance per type for the session's life, so one entry per type is
	// always the complete picture.
	ResourceType string `json:"resourceType"`

	// ResourceID is the instance ResourceType is currently pinned to.
	ResourceID string `json:"resourceID"`

	// MovedBy is the approval-record reference that authorized the most
	// recent MOVE off a prior instance — empty for a pin that has only ever
	// been first-filled. Populated only by the plan-gate approval path
	// (pkg/agent/runner's narrowToApproved), from the plan-gate audit record
	// that carried the approval: its PhaseKey, or PlanDigest+PhaseIndex when
	// PhaseKey is unset on an older record.
	// +optional
	MovedBy string `json:"movedBy,omitempty"`

	// MovedAt is when MovedBy's move landed on this pin. Empty for a pin that
	// has only ever been first-filled.
	// +optional
	MovedAt *metav1.Time `json:"movedAt,omitempty"`
}

// UpsertSlotPin upserts (by ResourceType) one SlotPin into list, with one
// carve-out that keeps move provenance stable: when the existing entry
// already names pin's OWN ResourceID and pin carries no MovedBy/MovedAt (a
// same-instance re-grant, or a redundant advisory re-read of an unchanged
// pin), the existing entry — including any recorded MovedBy/MovedAt — is
// preserved untouched. Everything else replaces the entry wholesale: a move
// (pin carries MovedBy) records its new provenance, and a DIFFERENT
// ResourceID arriving without move metadata clears the old provenance rather
// than attributing the previous move to an instance it never produced.
// Otherwise pin is appended. The updated slice is returned; the caller is
// responsible for assigning it back (e.g.
// sess.Status.SlotPins = UpsertSlotPin(sess.Status.SlotPins, pin)).
func UpsertSlotPin(list []SlotPin, pin SlotPin) []SlotPin {
	for i := range list {
		if list[i].ResourceType != pin.ResourceType {
			continue
		}
		if list[i].ResourceID == pin.ResourceID && pin.MovedBy == "" && pin.MovedAt == nil {
			return list
		}
		list[i] = pin
		return list
	}
	return append(list, pin)
}
