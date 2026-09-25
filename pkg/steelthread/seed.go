package steelthread

import (
	"context"
	"fmt"
	"slices"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
)

// Tuple is one SpiceDB relationship, in the same shape relwritesaudit.Tuple
// records: Resource and Subject as "type:id" (Subject optionally carrying a
// trailing "#relation"), Relation the relation name written between them.
type Tuple struct {
	Resource string
	Relation string
	Subject  string
}

// Expander expands one permission into the relationship tree SpiceDB used to
// resolve it — every tuple that contributed to the answer, not just the
// terminal leaf. Implementations call SpiceDB's Expand (or equivalent) and
// flatten the result; tests inject a closure and need no SpiceDB at all,
// which is what keeps the subtraction logic in DeriveSeed unit-testable.
type Expander func(ctx context.Context, resourceType, resourceID, permission string) ([]Tuple, error)

// SeedResult is what a capture needs to reproduce the authorization outcomes
// its session depended on.
type SeedResult struct {
	// Tuples are the pre-existing relationships to seed into the replay
	// fixture, deduped and sorted for a stable, byte-identical re-capture.
	Tuples []Tuple
	// Unseeded is every allowed (resourceType, resourceID, permission) key
	// whose expansion returned NOTHING AT ALL — the grant came from somewhere
	// this derivation cannot see, so the replay fixture will not reproduce it.
	// Reported rather than silently dropped: a later self-check turns this
	// into a hard finding instead of the divergence surfacing at replay time,
	// pointing at the wrong layer.
	//
	// An expansion whose every tuple was SUBTRACTED is not this, and the
	// distinction is the whole difference between the report being useful and
	// the report refusing the scenario the feature exists for. The three
	// subtraction rules below name tuples the REPLAY WRITES FOR ITSELF — the
	// session's own dispatches, the session object's own relationships, the
	// slot grant an approval mints. A permission reachable only through those
	// needs no seed at all: the replayed run re-approves, re-writes and is
	// allowed exactly as the captured one was, and seeding the tuple would
	// instead let the bundle pass with the code that writes it broken.
	//
	// The round-trip test found this: every plan-gate and slot bundle in the
	// bronze suite is allowed solely by an approval-minted grant, and all of
	// them were reported here — the capture refusing its own flagship case.
	//
	// A COLLECTED slot binding is the same answer arrived at with no tuple to
	// classify, and it is excluded here too — see collectedSlotGrant.
	Unseeded []string

	// DeclaredSlots is how many (resourceType, permission) pairs the class's
	// own AgentSessionGrants declared as slots, i.e. how large the exemption
	// above was. Zero means the derivation consulted no slot declaration at
	// all — either the class declares none, or none was gathered.
	//
	// Carried so the finding a surviving Unseeded key produces can tell a
	// reader WHICH of those it is. The two send them looking in different
	// places, and the key alone says neither.
	DeclaredSlots int
}

// DeriveSeed derives the SpiceDB relationships a captured session's
// authorization depended on.
//
// authz_decision records the ANSWER a Check received, never the PATH it
// resolved through — so the seed cannot be read off the log directly. For
// each distinct (resourceType, resourceID, permission) that carried at least
// one allowed decision, DeriveSeed expands the permission tree via expand and
// flattens it to tuples.
//
// Denied decisions are never expanded: absence is the fixture's default, and
// seeding a tuple to explain a denial would make the denial impossible to
// reproduce.
//
// The subtraction is the point. Three classes of tuple are written AT RUN
// TIME by the session itself, and seeding any of them would let a bundle pass
// with the code that writes them broken — replay would find the grant already
// present and never exercise the writer:
//
//  1. Everything in Records.RelWrites — the session's own MCP dispatches.
//  2. Any tuple naming an agentsession object on either side (Resource or
//     Subject) — channelsd's started_by and friends, written at session
//     start.
//  3. Any tuple whose relation is a slot grant, i.e. has the
//     authz.SlotGrantRelationPrefix prefix — written by the approval binder
//     when a human approves.
//
// These three are exhaustive only for what Records.Decisions can currently
// surface, not for every run-time-written tuple in the system. Every Decision
// comes from the single Check site scoped to a tool's declared resource type
// (pkg/agent/runner/pipeline_wiring.go). Info-leakage decisions
// (pkg/authz/guardian/approval/infoleakage_grant.go) write a fourth kind of
// run-time relationship —
// infoleakage_grant:<id>#audience_subject@user:<subject> — that is neither an
// agentsession tuple nor slot_grant_-prefixed, so none of the three filters
// above would catch it. It is harmless today only because those decisions
// land in a separate memory kind (infoleakagedecision) that Records never
// reads. If Records ever grows a field that surfaces info-leakage decisions,
// this derivation needs a fourth subtraction rule to match.
//
// An allowed decision whose expansion returns NOTHING is recorded in
// SeedResult.Unseeded rather than silently dropped — UNLESS its
// (resourceType, permission) is a slot the class declares, which makes the
// zero expansion a slot binding already collected rather than a grant nobody
// wrote. An allowed decision whose expansion was entirely subtracted is a
// third answer and is not recorded either — see that field's doc.
//
// grants is the session's own AgentSessionGrants, read for the slots it
// declares; nil is legitimate and means no slot is exempt — see
// collectedSlotGrant for what that exemption is and why it needs the CR.
func DeriveSeed(
	recs Records, expand Expander, grants *spiceboxv1alpha1.AgentSessionGrants,
) (SeedResult, error) {
	slots := declaredSlots(grants)

	written := map[Tuple]bool{}
	for _, audit := range recs.RelWrites {
		for _, tup := range audit.Tuples {
			written[Tuple{Resource: tup.Resource, Relation: tup.Relation, Subject: tup.Subject}] = true
		}
	}

	type key struct{ resourceType, resourceID, permission string }
	seen := map[key]bool{}
	var order []key
	for _, d := range recs.Decisions {
		if d.Outcome != authzdecision.OutcomeAllowed {
			continue
		}
		k := key{resourceType: d.ResourceType, resourceID: d.ResourceID, permission: d.Permission}
		if seen[k] {
			continue
		}
		seen[k] = true
		order = append(order, k)
	}

	ctx := context.Background()
	tuples := map[Tuple]bool{}
	var unseeded []string
	for _, k := range order {
		expanded, err := expand(ctx, k.resourceType, k.resourceID, k.permission)
		if err != nil {
			return SeedResult{}, fmt.Errorf("steelthread: expand %s:%s#%s: %w", k.resourceType, k.resourceID, k.permission, err)
		}

		// kept and subtracted answer two DIFFERENT questions, and conflating
		// them is what made this report fire on the flagship scenario. See
		// SeedResult.Unseeded.
		var kept, subtracted int
		for _, tup := range expanded {
			if written[tup] || isSessionTuple(tup) || isSlotGrantTuple(tup) {
				subtracted++
				continue
			}
			tuples[tup] = true
			kept++
		}
		if kept > 0 || subtracted > 0 {
			continue
		}
		// Nothing to classify at all — the third answer, and the one that is
		// ambiguous. See collectedSlotGrant.
		if collectedSlotGrant(slots, k.resourceType, k.permission) {
			continue
		}
		unseeded = append(unseeded, fmt.Sprintf("%s:%s#%s", k.resourceType, k.resourceID, k.permission))
	}

	out := make([]Tuple, 0, len(tuples))
	for t := range tuples {
		out = append(out, t)
	}
	slices.SortFunc(out, compareTuples)

	return SeedResult{Tuples: out, Unseeded: unseeded, DeclaredSlots: len(slots)}, nil
}

// slotKey is one (resourceType, permission) pair an AgentClass declares as an
// authz SLOT — the INSTANCE axis, where the grant relation lives on the
// RESOURCE and points back at the session
// (AgentSessionGrantsSpec.Slots, and pkg/controllers/agentclass extractSlotPairs
// which derives it from spec.authz.slots).
type slotKey struct{ resourceType, permission string }

// declaredSlots reads the slot pairs off the session's own AgentSessionGrants.
//
// From the CR, never from a list of type names: which resource types this
// cluster's agents bind by instance is the AgentClass author's declaration, and
// a copy of it kept here would be a second source of truth that silently drifts
// — a class adding a slot would keep hard-failing every capture until somebody
// remembered to edit a list in this package.
//
// A nil CR yields an EMPTY set, which is the strict answer: with nothing
// declared, collectedSlotGrant exempts nothing and every zero expansion is
// reported exactly as it was before. Absence must not be read as "assume it was
// a slot" — that would turn the check off for precisely the sessions whose
// class declaration could not be found.
func declaredSlots(grants *spiceboxv1alpha1.AgentSessionGrants) map[slotKey]bool {
	if grants == nil {
		return nil
	}
	out := make(map[slotKey]bool, len(grants.Spec.Slots))
	for _, s := range grants.Spec.Slots {
		out[slotKey{resourceType: s.ResourceType, permission: s.Permission}] = true
	}
	return out
}

// collectedSlotGrant reports whether an allowed decision that expanded to
// NOTHING is explained by a slot binding that has since been COLLECTED.
//
// Expansion runs against LIVE SpiceDB, long after the session concluded. A
// session-only slot binding is written when a human approves and deleted when
// the session ends, so by capture time the tuple that carried the allow is
// gone — and a permission held ONLY by that binding then expands to zero
// tuples. That is indistinguishable, tuple-for-tuple, from a grant nobody ever
// wrote: both produce kept == 0 and subtracted == 0, because there is nothing
// left to classify.
//
// The class's own declaration is what separates them. A pair declared as a slot
// is one this class mints a grant for AT RUN TIME on the resource — the exact
// tuple isSlotGrantTuple already subtracts when it is still present. So a
// declared pair that expands to nothing is the SAME answer as one whose every
// tuple was subtracted, arrived at a little later: the replay re-approves,
// re-mints and is allowed as the captured run was, and seeding anything for it
// would let the bundle pass with the binder broken.
//
// Keyed on the PAIR, not on the resource type, because the declaration is a
// pair: a class declaring git_repo#fetch and git_repo#read exempts those two
// and leaves git_repo#delete as strict as any other permission. And the
// exemption reaches only this branch — a pair with any surviving tuple is
// classified normally, so a declared slot never suppresses a seed that the
// expansion actually produced.
func collectedSlotGrant(slots map[slotKey]bool, resourceType, permission string) bool {
	return slots[slotKey{resourceType: resourceType, permission: permission}]
}

// isSessionTuple reports whether tup names an agentsession object on either
// side. The session object itself is never something a capture should seed —
// its relationships (started_by and the like) are written at session start
// by channelsd, not pre-existing state.
func isSessionTuple(tup Tuple) bool {
	return strings.HasPrefix(tup.Resource, "agentsession:") || strings.HasPrefix(tup.Subject, "agentsession:")
}

// isSlotGrantTuple reports whether tup's relation is a slot grant, derived
// from authz.SlotGrantRelationPrefix rather than a literal so this rule
// cannot drift from the relation name the approval binder actually writes.
func isSlotGrantTuple(tup Tuple) bool {
	return strings.HasPrefix(tup.Relation, authz.SlotGrantRelationPrefix)
}

func compareTuples(a, b Tuple) int {
	if c := strings.Compare(a.Resource, b.Resource); c != 0 {
		return c
	}
	if c := strings.Compare(a.Relation, b.Relation); c != 0 {
		return c
	}
	return strings.Compare(a.Subject, b.Subject)
}
