package plangate

import (
	"sort"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// PhaseAuthorityRecord projects one frozen phase into the record fields that
// carry its AUTHORITY — everything Plan.Digest and Phase.AuthorityKey hash.
//
// It exists because the projection and the digest have to move together and
// three times have not. Each time a new dimension entered the digest — slot
// requests, then the call budget, then the slot's instance id — the writer kept
// recording the old set, so a plan rebuilt from its own log digested
// differently from the digest those very records named, and every fold
// discarded the lot. That failure is silent at the write, silent at the read,
// and shows up as a session losing its entire gate state across a restart.
//
// So there is one projection, in the package that owns the digest, and
// PlanForDigest is its inverse. A dimension added to the digest without being
// added here fails the round-trip test next to it rather than a live session.
//
// The caller sets Event, PlanDigest and its own operational fields (tier, mode,
// provenance, timestamps); this fills only what authority is made of.
//
// standing maps each slot's resource type to how an approval on it gets its
// authority (v1alpha1.StandingSessionOnly / StandingRequired) — the SAME
// per-type map the runner holds (Loop.PlanGateSlotStanding), passed in rather
// than looked up here so this stays a pure projection of its arguments. It is
// NOT part of the plan's authority: unlike Ceiling, MaxCount and Slots, it
// never enters Digest or AuthorityKey, because the runner re-resolves it fresh
// at approval time rather than reading it off the frozen plan (see
// approverCanDelegateSlots) — recording it here is audit trail, not a second
// source of truth for it.
func PhaseAuthorityRecord(p Plan, index int, standing map[string]string) plangateaudit.Content {
	if index < 0 || index >= len(p.Phases) {
		return plangateaudit.Content{}
	}
	ph := p.Phases[index]

	rec := plangateaudit.Content{
		PhaseIndex: new(int32(index)),
		// The phase's authority BY NAME, which is what an approval is keyed on.
		// Inert on a plan-level record — the fold reads it only off an approval —
		// and carried anyway, because this is the projection of what the phase
		// may do and the key is precisely that.
		PhaseKey:    ph.AuthorityKey(),
		Consents:    ph.Consents,
		MaxCount:    ph.Max.Count,
		BudgetCalls: ph.Budget.Calls,
		Requires:    requiresIndices(ph),
	}
	for _, h := range ph.Permissions {
		rec.Ceiling = append(rec.Ceiling, h.String())
	}
	// Sorted: a ceiling is a SET, and the same reach recorded in two orders must
	// read back as one grant rather than as a widening.
	sort.Strings(rec.Ceiling)
	for _, s := range ph.Slots {
		// Both spellings. Slots is the type list every existing reader and every
		// record already written speaks; SlotRefs carries the instance the type
		// list cannot, and a reader that finds it prefers it.
		rec.Slots = append(rec.Slots, s.Type)
		rec.SlotRefs = append(rec.SlotRefs, plangateaudit.SlotRef{
			Type: s.Type, ID: s.ID, Standing: standing[s.Type],
		})
	}
	return rec
}

// requiresIndices projects a phase's ordering edges to bare indices.
func requiresIndices(ph Phase) []int {
	if len(ph.Requires) == 0 {
		return nil
	}
	out := make([]int, 0, len(ph.Requires))
	for _, r := range ph.Requires {
		out = append(out, r.Phase)
	}
	return out
}

// slotsFromRecord reads a record's slot requests back as frozen slots.
//
// SlotRefs when present, the bare type list otherwise. The fallback is not a
// nicety: the log is append-only, and records written before an instance could
// be named meant exactly "this type, no instance" — which is what the type list
// alone reconstructs.
func slotsFromRecord(r plangateaudit.Content) []Slot {
	if len(r.SlotRefs) > 0 {
		out := make([]Slot, 0, len(r.SlotRefs))
		for _, s := range r.SlotRefs {
			out = append(out, Slot{Type: s.Type, ID: s.ID})
		}
		return out
	}
	if len(r.Slots) == 0 {
		// nil, not an empty slice: a phase that requested nothing must rebuild
		// identically to one frozen with no slots at all.
		return nil
	}
	out := make([]Slot, 0, len(r.Slots))
	for _, s := range r.Slots {
		out = append(out, Slot{Type: s})
	}
	return out
}
