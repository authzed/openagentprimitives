package plangate

import (
	"errors"

	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// ErrDoubtfulFold is returned by ActiveCeiling when the fold could not
// establish which phase is active.
var ErrDoubtfulFold = errors.New("plangate: the audit fold could not establish an active phase")

// State is the authorization state derived by replaying the append-only log.
//
// It is REBUILT on demand and never held across a restart. That is the point:
// approval state cannot live in memory (a runner restart or idle-sleep
// re-hydrate would lose a decision a human already made) and cannot live in
// the plan document (the agent rewrites that on every call, so an `approved`
// field there would be agent-writable and the gate would be theatre).
type State struct {
	// Plan is the frozen plan this state was folded against.
	Plan Plan

	// ActivePhase is the index the agent currently holds. Meaningless when
	// Doubtful is set.
	ActivePhase int

	// Spent counts entries per phase index, including phase 0's implicit
	// initial entry.
	Spent map[int]int

	// CallsSpent counts GOVERNED calls per phase index — the budget ledger.
	//
	// Derived from the same records the ceiling test reads, so a budget needs no
	// new state and cannot drift from what the gate actually observed. Calls the
	// gate does not govern (no handle: meta and passthrough tools) spend nothing,
	// or update_plan would burn the budget whose exhaustion forces the re-plan.
	CallsSpent map[int]int

	// Doubtful means the log contained something the fold could not interpret.
	// The ceiling is then EMPTY rather than stale — see ActiveCeiling.
	Doubtful bool

	// deniedCeilings are the handle sets a human refused, kept as sets so a
	// denial survives the plan being reshaped around it.
	deniedCeilings [][]permsurface.Handle

	// deniedRefs are the exact (digest, index) pairs denied, for the common
	// same-plan question.
	deniedRefs map[PhaseRef]struct{}

	// approvedKeys are approvals recorded by phase AUTHORITY (see
	// plangate.Phase.AuthorityKey). Separate from approvedPhases, which holds
	// the legacy (digest, index) form; both are consulted so an append-only log
	// spanning an upgrade folds correctly.
	approvedKeys map[string]struct{}

	// amendedHandles are handles an APPROVED AMENDMENT added to a phase, keyed by
	// the phase it widened.
	//
	// A frozen plan is immutable, so its Ceiling can never grow — which meant an
	// approved amendment cleared exactly the call in front of it and nothing
	// more. The next call wanting the same handle was outside the ceiling again
	// and raised another card, and a human clicked Approve on the same amendment
	// forever (observed live 2026-08-21). Persisting the widening here is what
	// makes "yes" mean yes for the rest of the phase.
	//
	// Additive over the plan, never a replacement: the frozen ceiling still
	// bounds everything the plan itself asked for, and an amendment can only add
	// what a human separately approved.
	amendedHandles map[PhaseRef][]permsurface.Handle

	// envelope is the pre-exposure baseline: the reach the agent expected
	// before it read anything external. envelopeSet distinguishes "declared
	// nothing" (a real claim that it expects to need nothing) from "never
	// declared" — the two must not collapse, because the second is what makes
	// every supersede Severe.
	envelope    map[string]struct{}
	envelopeSet bool

	// pending is what still needs a human. Recording an envelope never adds to
	// it: the baseline is recorded, not approved.
	pending []PhaseRef

	// completedPhases is the set of indices the agent DECLARED finished.
	//
	// Kept separate from Spent so CompletedPhases can require BOTH: a phase is
	// complete only if the runtime saw it entered AND the agent declared it
	// done. Declaration alone would be WEAKER than the entry rule it extends —
	// an agent could declare a phase it never entered and open the one that
	// depends on it.
	completedPhases map[int]struct{}

	// approvedPhases is the set of (digest, index) a human — or tier 0 — cleared
	// to run. Declaring a phase never adds to this: an agent that could approve
	// its own declaration would make the gate theatre.
	approvedPhases map[PhaseRef]struct{}
}

// IsDenied reports whether a human denied this exact phase of this exact plan.
func (s State) IsDenied(ref PhaseRef) bool {
	_, ok := s.deniedRefs[ref]
	return ok
}

// DeniedCeilingIntersects reports whether any handle in the candidate set was
// part of a ceiling a human denied.
//
// Denials key on the CEILING by INTERSECTION rather than on a phase id or
// index, and equality would not do: an agent could evade an equality check by
// re-submitting a superset, or by splitting the denied phase in two so the
// denied reach lands at a different index in a differently-digested plan.
func (s State) DeniedCeilingIntersects(candidate []permsurface.Handle) bool {
	for _, denied := range s.deniedCeilings {
		set := make(map[permsurface.Handle]struct{}, len(denied))
		for _, h := range denied {
			set[h] = struct{}{}
		}
		for _, h := range candidate {
			if _, hit := set[h]; hit {
				return true
			}
		}
	}
	return false
}

// ActiveCeiling returns the handles the agent currently holds.
//
// On a doubtful fold it returns the EMPTY ceiling and an error, never the last
// confidently-read phase. That direction is load-bearing and easy to get
// backwards: taking the last good selection fails OPEN, because a truncated
// read that drops later switch records would restore a ceiling the agent had
// already moved off. Empty fails closed — the agent holds nothing until it
// re-selects, which is recoverable and observable.
// DeniedCeilings returns the handle sets a human refused, for a caller outside
// this package that must carry them onward — the delegation path, which folds a
// PARENT's state and hands the denials to DeriveForChild so "a denial survives a
// delegation" holds the same way it holds across a fork. Returned by value; the
// caller only reads it.
func (s State) DeniedCeilings() [][]permsurface.Handle { return s.deniedCeilings }

func (s State) ActiveCeiling() (map[permsurface.Handle]struct{}, error) {
	if s.Doubtful {
		return nil, ErrDoubtfulFold
	}
	out, err := s.Plan.Ceiling(s.ActivePhase)
	if err != nil {
		return nil, err
	}
	// Union in what approved amendments added to THIS phase. Additive over the
	// frozen plan and never a replacement: the plan still bounds everything it
	// asked for, and each added handle is one a human separately approved.
	//
	// Without this an approved amendment cleared only the call in front of it,
	// so the next call wanting the same handle re-asked — the same card, over
	// and over.
	ref := PhaseRef{PlanDigest: s.Plan.Digest(), Index: s.ActivePhase}
	for _, h := range s.amendedHandles[ref] {
		out[h] = struct{}{}
	}
	return out, nil
}

// Fold replays the plan-gate log into authorization state.
//
// Records naming a different plan are ignored rather than treated as
// corruption: multiple plans legitimately coexist in one session's log, and
// only one governs at a time.
//
// Records this plan's own that cannot be interpreted set Doubtful. The fold
// itself does not return a Go error for those — a malformed record is an
// OUTCOME the caller must handle (log it, fail closed), not an exceptional
// condition. Fold returns an error only for a caller-side mistake.
func Fold(plan Plan, records []plangateaudit.Content) (State, error) {
	st := State{
		Plan:            plan,
		Spent:           map[int]int{},
		deniedRefs:      map[PhaseRef]struct{}{},
		approvedKeys:    map[string]struct{}{},
		amendedHandles:  map[PhaseRef][]permsurface.Handle{},
		CallsSpent:      map[int]int{},
		completedPhases: map[int]struct{}{},
		approvedPhases:  map[PhaseRef]struct{}{},
	}
	digest := plan.Digest()

	// The implicit initial position CONSUMES phase 0's first entry. Without
	// this, phase 0 silently gets one more entry than every other phase, and
	// no plan could legitimately return to its first phase — the budget it
	// never spent on entry would still be sitting there.
	if len(plan.Phases) > 0 {
		st.Spent[0] = 1
	}

	for _, r := range records {
		// Denials are handled BEFORE the foreign-plan skip, and deliberately.
		//
		// A human's "no" must outlive the plan it was made against, or the
		// bypass is trivial: deny a phase, reshape the plan, and the denial
		// belongs to a digest nothing consults any more. So a denial is
		// self-contained — it carries the ceiling it refused — and is folded in
		// whatever plan is current.
		if r.Event == plangateaudit.EventDenied {
			idx := -1
			if r.PhaseIndex != nil {
				idx = int(*r.PhaseIndex)
			} else if r.PlanDigest == digest {
				// A denial against THIS plan naming no phase cannot be placed.
				st.Doubtful = true
			}
			if idx >= 0 {
				st.deniedRefs[PhaseRef{PlanDigest: r.PlanDigest, Index: idx}] = struct{}{}
			}
			if set := parseHandles(r.Ceiling); len(set) > 0 {
				st.deniedCeilings = append(st.deniedCeilings, set)
			} else if r.PlanDigest == digest && idx >= 0 && idx < len(plan.Phases) {
				// Older records predate the Ceiling field; fall back to reading
				// it off this plan when the denial is this plan's own.
				st.deniedCeilings = append(st.deniedCeilings, plan.Phases[idx].Permissions)
			}
			continue
		}

		// The envelope is session-scoped, not plan-scoped: it is the baseline
		// LATER plans are judged against, so it must survive every supersede
		// and is folded ahead of the foreign-plan skip.
		if r.Event == plangateaudit.EventEnvelope {
			// Only the FIRST declaration counts. An agent that has already read
			// external text re-declaring a wider envelope would be laundering
			// post-exposure reach into a "pre-exposure" baseline, which is the
			// one thing this artifact must not permit.
			if st.envelopeSet {
				continue
			}
			st.envelopeSet = true
			st.envelope = make(map[string]struct{}, len(r.Ceiling))
			for _, h := range r.Ceiling {
				// Parse before admitting: a baseline that quietly accepted
				// handles it cannot read would widen itself on malformed input
				// and suppress the very signal it exists to raise.
				if _, err := permsurface.ParseHandle(h); err == nil {
					st.envelope[h] = struct{}{}
				}
			}
			continue
		}

		// A key-bearing approval is handled BEFORE the foreign-plan skip, for
		// the mirror of the reason denials are.
		//
		// A denial must outlive the plan it was made against so a reshape cannot
		// launder it. An approval must outlive one so a reshape does not
		// RE-CHARGE the user: the key covers the phase's authority, so a record
		// carrying it applies to any plan holding a phase that may do exactly
		// the same things. Skipping it here is what made every re-plan — and
		// even a reorder — invalidate approvals the user had already given.
		//
		// Safe because the key IS the authority: a phase that gained a
		// permission or changed an instance has a different key and is not
		// matched. And PhaseApproved still subtracts denials first, so this
		// cannot resurrect a refused phase.
		if r.Event == plangateaudit.EventPhaseApproved && r.PhaseKey != "" {
			st.approvedKeys[r.PhaseKey] = struct{}{}
			continue
		}

		// Not this plan's business. Normal, not doubtful — several plans
		// legitimately coexist in one session's log and only one governs.
		if r.PlanDigest != "" && r.PlanDigest != digest {
			continue
		}

		// The budget ledger. Counted before the event switch because EVERY
		// governed call spends, whatever the gate decided about it — an
		// out-of-ceiling attempt is work the phase did, and not charging for it
		// would make grinding against a wall free.
		if r.Handle != "" && r.PhaseIndex != nil {
			if idx := int(*r.PhaseIndex); idx >= 0 && idx < len(plan.Phases) {
				st.CallsSpent[idx]++
			}
		}

		switch r.Event {
		case plangateaudit.EventPhaseCompleted:
			if r.PhaseIndex == nil {
				st.Doubtful = true
				continue
			}
			if idx := int(*r.PhaseIndex); idx >= 0 && idx < len(plan.Phases) {
				st.completedPhases[idx] = struct{}{}
			}

		case plangateaudit.EventPhaseApproved:
			// An approved AMENDMENT names the one handle it added. Recording it
			// against the phase is what makes the widening outlive the single call
			// that triggered it — without this the next call wanting the same
			// handle is outside the frozen ceiling again and re-asks.
			//
			// Unparseable handles are skipped rather than failing the fold: a
			// record naming a handle this build cannot read must not widen
			// anything, and must not erase what the rest of the log established.
			//
			// A handle-bearing record is an AMENDMENT and nothing more: it
			// must NOT fall through to the whole-phase grant below. The human
			// who approved it was shown one added handle, under a card that
			// says the plan is already approved and that wipes the phase's real
			// ceiling before rendering. Falling through granted them the entire
			// phase — including every readwrite/external handle in a phase that
			// was never challenged, because phaseNeedsApproval is reached only
			// on the in-ceiling arm and a deliberately out-of-ceiling first call
			// takes the amendment arm instead.
			if r.Handle != "" && r.PhaseIndex != nil {
				if h, err := permsurface.ParseHandle(r.Handle); err == nil {
					ref := PhaseRef{PlanDigest: r.PlanDigest, Index: int(*r.PhaseIndex)}
					st.amendedHandles[ref] = append(st.amendedHandles[ref], h)
				}
				// Unparseable handles land here too, and that is right: a
				// record naming a handle this build cannot read must widen
				// nothing AND must not be mistaken for a whole-phase grant.
				continue
			}
			// Key-bearing records were already folded above, ahead of the
			// foreign-plan skip, and handle-bearing amendments continued out
			// just now. Anything reaching here carries neither: it is LEGACY,
			// written before PhaseKey existed. This log is append-only, so
			// these must keep folding exactly as they did, or a session
			// upgraded mid-run starts re-asking for work the user already
			// cleared.
			if r.PhaseIndex == nil {
				st.Doubtful = true
				continue
			}
			st.approvedPhases[PhaseRef{PlanDigest: r.PlanDigest, Index: int(*r.PhaseIndex)}] = struct{}{}

		case plangateaudit.EventPhaseSelected:
			if r.PhaseIndex == nil {
				// A selection naming no phase cannot be interpreted, and
				// guessing index 0 would silently hand the agent phase 0's
				// ceiling.
				st.Doubtful = true
				continue
			}
			idx := int(*r.PhaseIndex)
			if idx < 0 || idx >= len(plan.Phases) {
				st.Doubtful = true
				continue
			}
			st.ActivePhase = idx
			st.Spent[idx]++

		case plangateaudit.EventPlanApproved, plangateaudit.EventSuperseded:
			// A FORK ROOT is an approval record that also carries the parent's
			// resolved denials and baseline. Honouring them here is what makes
			// a derived root equivalent to the chain it replaced — without it
			// the denial would travel into the child and then be ignored, which
			// is worse than not carrying it at all.
			if set := parseHandles(r.DeniedCeiling); len(set) > 0 {
				st.deniedCeilings = append(st.deniedCeilings, set)
			}
			if r.Envelope != nil && !st.envelopeSet {
				st.envelopeSet = true
				st.envelope = make(map[string]struct{}, len(r.Envelope))
				for _, h := range r.Envelope {
					if _, err := permsurface.ParseHandle(h); err == nil {
						st.envelope[h] = struct{}{}
					}
				}
			}

			// A supersede is a FLOOR, not a reset. Budgets are restored only
			// where nothing was denied; a denied phase keeps its spent count,
			// so re-approving a byte-identical plan cannot undo a human's no.
			for idx := range st.Spent {
				if st.IsDenied(PhaseRef{PlanDigest: digest, Index: idx}) {
					continue
				}
				delete(st.Spent, idx)
			}
			if len(plan.Phases) > 0 && !st.IsDenied(PhaseRef{PlanDigest: digest, Index: 0}) {
				st.Spent[0] = 1
			}
			st.ActivePhase = 0

		case plangateaudit.EventApprovalsCleared:
			// This record already only reaches here for THIS plan's digest — the
			// "not this plan's business" skip above discards any record naming a
			// different one before the switch is ever entered. That is what scopes
			// the clear to one plan; nothing inside this case needs to filter by
			// digest again.
			//
			// Both approval forms are cleared WHOLESALE, not filtered by digest:
			//
			//   - approvedKeys is keyed by Phase.AuthorityKey, which is
			//     DIGEST-INDEPENDENT by construction — it exists precisely so an
			//     approval survives the plan being reshaped (see the key-bearing
			//     EventPhaseApproved branch above). An authority key carries no
			//     digest to filter on, so there is no correct way to scope this by
			//     PlanDigest; the record's PlanDigest is retained as audit context
			//     only.
			//   - approvedPhases (the legacy digest+index form) is cleared
			//     wholesale too, for the same reason: a clear that left one form
			//     intact would still let a released agent resume on its old
			//     ceiling through whichever form its approval happened to be
			//     recorded in.
			//
			// A supersede (above) is a FLOOR: it restores budgets but deliberately
			// leaves both approval maps intact, because re-approving a
			// byte-identical plan must not re-charge the user for work already
			// cleared. A clear is the opposite operation and exists for the
			// opposite reason: it is written when a human releases a forensic
			// hold, and reintegration must NOT hand the released agent back the
			// ceiling it held when it was frozen. Collapsing the two into one path
			// would silently turn every release into a no-op on reach.
			clear(st.approvedKeys)
			clear(st.approvedPhases)
		}
	}

	return st, nil
}

// parseHandles converts recorded handle strings back into handles, skipping
// any that no longer parse. A denial whose handles cannot be read is not
// silently widened into "denies nothing" — the caller still has the PhaseRef,
// and an unparseable entry simply contributes no intersection.
func parseHandles(ss []string) []permsurface.Handle {
	if len(ss) == 0 {
		return nil
	}
	out := make([]permsurface.Handle, 0, len(ss))
	for _, s := range ss {
		if h, err := permsurface.ParseHandle(s); err == nil {
			out = append(out, h)
		}
	}
	return out
}

// EnvelopeRecorded reports whether this session recorded a pre-exposure
// baseline at all.
func (s State) EnvelopeRecorded() bool { return s.envelopeSet }

// LeavesEnvelope reports whether the given reach goes beyond the pre-exposure
// baseline.
//
// With no baseline recorded EVERYTHING leaves it. That is deliberate: the
// severity ladder then treats every supersede as an exit, which makes
// declaring an envelope strictly better than not declaring one — no switch to
// forget, no configuration to get wrong.
//
// This is a DETECTION control, not a preventive one. It does not bind an
// injected agent; it makes an injected agent's expansion visible and
// attributable, because the baseline is the one artifact in the session
// provably authored before any attacker-controlled text existed.
func (s State) LeavesEnvelope(handles []string) bool {
	if !s.envelopeSet {
		return true
	}
	for _, h := range handles {
		if _, ok := s.envelope[h]; !ok {
			return true
		}
	}
	return false
}

// WithinEnvelope is the inverse of LeavesEnvelope.
func (s State) WithinEnvelope(handles []string) bool { return !s.LeavesEnvelope(handles) }

// ApprovalsPending is what the fold says still needs a human. Recording an
// envelope contributes nothing to it — that is what "recorded, not approved"
// means.
func (s State) ApprovalsPending() []PhaseRef { return s.pending }

// PhaseApproved reports whether this phase of THIS plan is cleared to run.
//
// Declaring a phase never clears it — an agent that could approve its own
// declaration would make the gate theatre. Clearance comes from tier-0
// auto-approval or a human, both of which write an explicit record.
//
// A DENIED phase is never approved, whatever else the log says. A human's no
// outranks a later approval record, so an agent cannot launder a refusal by
// getting the phase re-approved.
func (s State) PhaseApproved(index int) bool {
	ref := PhaseRef{PlanDigest: s.Plan.Digest(), Index: index}
	if _, denied := s.deniedRefs[ref]; denied {
		return false
	}
	// Authority first: an approval recorded against this phase's authority
	// counts however the plan has since been reshaped around it.
	if index >= 0 && index < len(s.Plan.Phases) {
		if _, ok := s.approvedKeys[s.Plan.Phases[index].AuthorityKey()]; ok {
			return true
		}
	}
	// Then the legacy form, for records that predate PhaseKey.
	_, ok := s.approvedPhases[ref]
	return ok
}

// BudgetExhausted reports whether this phase has spent its governed-call
// budget.
//
// A phase that declared NO budget is never exhausted. Zero means unbounded, not
// "no calls" — the opposite reading would deny the first call of every phase
// that did not opt in, turning an optional focus mechanism into a total outage.
//
// An out-of-range index is not exhausted either: the ceiling lookup already
// errors on it (Plan.Ceiling), and reporting exhaustion here would replace that
// specific, actionable error with a misleading one about budgets.
func (s State) BudgetExhausted(index int) bool {
	if index < 0 || index >= len(s.Plan.Phases) {
		return false
	}
	limit := s.Plan.Phases[index].Budget.Calls
	if limit <= 0 {
		return false
	}
	return s.CallsSpent[index] >= limit
}

// CompletedPhases reports which phases are finished, keyed by index.
//
// A phase counts only when the runtime saw it ENTERED and the agent DECLARED it
// done. Both halves are load-bearing and in opposite directions:
//
//   - Entry alone is what a requires edge meant before completion existed. Left
//     as the only test, completion would add nothing.
//   - Declaration alone would be WEAKER than that, not stronger: an agent could
//     declare a phase it never entered and open the one that depends on it,
//     turning a narrowing into a bypass.
//
// Requiring both is what makes this monotonically stricter than the rule it
// extends, which is the entire argument for letting an agent-authored signal
// gate anything at all. If that ever stops holding — if completion comes to
// GRANT something rather than withhold it — this is where it breaks.
func (s State) CompletedPhases() map[int]bool {
	out := make(map[int]bool, len(s.completedPhases))
	for idx := range s.completedPhases {
		if s.Spent[idx] > 0 {
			out[idx] = true
		}
	}
	return out
}
