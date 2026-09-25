package schema

import (
	"fmt"
	"sort"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// FragmentTier ranks a fragment's author for conflict resolution. HIGHER wins.
//
// The zero value is deliberately the LEAST-trusted tier. A candidate
// construction site that forgets to set Tier therefore loses conflicts rather
// than winning them, so the failure mode of an omission is a fragment that does
// not compose rather than a tenant silently outranking the cluster operator.
type FragmentTier int

const (
	// TierTenant is agent- or bundle-authored: MCPServer, SidecarToolbox,
	// SpiceboxToolkit. It is the zero value on purpose.
	TierTenant FragmentTier = 0
	// TierOperator is cluster-operator-authored: SpiceDBBootstrap.
	TierOperator FragmentTier = 1
)

// IdentifiedFragment pairs a schema fragment with a stable identity key
// (e.g. an MCPServer's "namespace/name") and the trust Tier of its author.
// The key is used ONLY for deterministic ordering within a tier and for
// reporting which contributor a rejection belongs to —
// PartitionCompatibleFragments never parses it.
type IdentifiedFragment struct {
	Key      string
	Tier     FragmentTier
	Fragment *spiceboxv1alpha1.SpiceDBSchemaFragment
}

// RejectedFragment is an IdentifiedFragment the incremental partition
// could not accept because composing it on top of the already-accepted
// set failed (a cross-fragment conflict), carrying the compose error so
// the caller can surface WHICH contributor was excluded and WHY.
type RejectedFragment struct {
	Key string
	Err error
	// DisplacedBy names the already-accepted fragment(s) whose presence in
	// the trial caused the rejection — the winning side of the conflict, so
	// an operator debugging a rejection knows WHERE to look, not just WHY it
	// failed (design §3).
	//
	// Set by re-composing each already-accepted fragment ALONE with the
	// candidate: a single accepted fragment whose pairwise trial reproduces
	// the SAME failure is proof that fragment alone is sufficient to cause
	// it, and DisplacedBy names just that one. Two or more accepted
	// fragments can reproduce it alone when they declare byte-identical
	// bodies for the name the candidate collides on (EmitSpicedbSchema
	// permits that — only a MISMATCHED redeclaration conflicts) — in that
	// case DisplacedBy is the sorted, comma-joined set of all of them, which
	// is still exact, just not singular. If NO accepted fragment reproduces
	// the failure alone (the trial's failure depends on more than one
	// already-accepted fragment together — not observed by any conflict this
	// package can currently produce, since EmitSpicedbSchema's and
	// compiler.Compile's failures are both pairwise by construction, but a
	// future conflict class might not be), DisplacedBy falls back to the
	// full accepted set the candidate was trial-composed against: less
	// precise, but never a guess — a name this function cannot single out is
	// not manufactured into a specific one.
	DisplacedBy string
}

// PartitionCompatibleFragments greedily builds the maximal conflict-free
// subset of frags. Each individual fragment is assumed to already be valid ON
// ITS OWN (callers run ValidateFragment first); this pass exists to isolate a
// fragment that passes alone but conflicts with the rest of the set once
// they are all trial-composed together, on two axes:
//
//   - PARSE-CLASS conflicts between fragments: two fragments declaring the
//     same resource name with different bodies ("conflicting definitions for
//     resource X across schema fragments"), a cross-fragment compile
//     failure, or a fragment redeclaring a definition name the baseline
//     owns — anything composeAllParseOnly's own parse-only compile refuses
//     outright (the `err != nil` branch of the walk, below).
//   - Dangling references a candidate INTRODUCES: an arrow (or a direct
//     relation's subject type) naming something that resolves to nothing
//     even with the baseline and every sibling candidate present. This is
//     genuine, tested isolation — UnresolvedReferences run over each trial,
//     not a parse artifact; see
//     TestPartitionCompatibleFragments_IsolatesADanglingRefOntoTheBaseline.
//     It is best-effort: diffed against a baseline floor (baseUnresolved,
//     below) so a code-owned gap in the baseline itself can't reject every
//     tenant fragment, and attribution is order-INDEPENDENT (preRejected,
//     below) exactly when the full candidate set is known to compose
//     cleanly on its own (allKnown) — when it is not, because some OTHER
//     parse-class conflict is also present in the set, attribution falls
//     back to the order-dependent incremental walk.
//
// Such a conflict, fed unfiltered to RunAll, fails compose→WriteSchema for
// the WHOLE batch and freezes every AgentSessionGrants in the cluster —
// isolating either class to its own contributor's CR is what this partition
// closes.
//
// It does NOT isolate a defect only a FULL TYPE-SYSTEM validation can see.
// UnresolvedReferences asks whether a referenced NAME resolves to something
// declared; it does not ask whether a definition's own declared members are
// mutually consistent. The confirmed case: a RELATION and a PERMISSION
// sharing one name on the SAME definition (SpiceDB has one namespace per
// definition for both — this is the exact defect shape the githubRepoFragment
// integration-test fixture had, fixed elsewhere in this task). The name
// resolves fine — there is nothing dangling for UnresolvedReferences to
// find — so the collision survives this partition intact, and is caught only
// when composeFragmentSet's real type-system validation runs, where it fails
// the WHOLE RunAll write rather than being isolated to the fragment that
// caused it. The failure mode is still better than pre-Task-4 (no write is
// even attempted, rather than one SpiceDB rejects), but the blast radius —
// every AgentSessionGrants stamped not-included until the bad CR is fixed or
// removed — is unchanged for this specific defect class. Isolating it the way
// this function isolates the two classes above is possible now that the
// guardian owns the validating check, but it is NOT YET built.
//
// The algorithm sorts candidates by Tier (higher trust first), then by Key
// within a tier, then walks in that order trial-composing accepted+candidate
// via composeAllParseOnly — a PARTIAL-VIEW-SAFE assembly, not the validating
// composeFragmentSet RunAll itself composes through, because a candidate
// walked before the sibling declaring its type is judged against a set that
// does not yet contain that sibling; see composeAllParseOnly's own doc for
// why validating a partial view would misjudge exactly that case — accepting
// on success and recording the compose error on failure. RunAll's own
// assembly (composeFragmentSet) still validates the set this returns, in-
// process, before any write, so a reference this best-effort walk cannot see
// resolving is caught there rather than reaching SpiceDB.
//
// The sort is load-bearing, not tidiness: the accepted set feeds a
// server-side schema write that must be idempotent, so the accept/reject
// decision must not depend on cluster List() ordering. Ordering by Tier first
// is a trust decision, not a tidiness one — see FragmentTier.
//
// Tie-break: FIRST in sort order wins; the later candidate is rejected. Both
// halves of that sentence matter and neither reads as expected on its own:
// highest-trust sorts first (an operator-authored fragment is placed ahead of
// a tenant-authored one regardless of Key), and first-in-sort-order still
// wins the walk exactly as it always has. Rejecting BOTH sides would let a
// malicious tenant knock a chosen victim's fragment out of the schema just by
// publishing a colliding one. Rejecting only the later side bounds the blast
// radius to candidates sorting AFTER the victim. A residual remains — within
// the SAME tier, an earlier-sorting tenant can still displace a same-tier
// victim by declaring their resource name first — whose real fix is
// per-tenant resource-name namespacing.
//
// baseline is the channel-kind fragments (slack_channel, slack_user, agent,
// …) that RunAll ALWAYS composes alongside the accepted set. It is
// pre-accepted and highest-trust — never a candidate, never rejected — and it
// MUST be present in every trial, or the partition decides accept/reject over a
// strictly smaller schema than the one RunAll writes. Omitting it (passing nil,
// which every trial used to do) let a tenant fragment redeclaring a channel-kind
// definition pass the partition and then freeze the whole cluster's schema write
// in RunAll — the exact PARSE-CLASS cross-tenant DoS this partition closes. A
// tenant fragment that redeclares a baseline definition is isolated to its
// own CR the same way a dangling reference onto a baseline type is (see the
// function doc above for the narrower class — a relation/permission name
// collision WITHIN one definition — that this partition does not catch).
func PartitionCompatibleFragments(baseline []*spiceboxv1alpha1.SpiceDBSchemaFragment, frags []IdentifiedFragment) (accepted []IdentifiedFragment, rejected []RejectedFragment) {
	sorted := append([]IdentifiedFragment{}, frags...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Tier != sorted[j].Tier {
			return sorted[i].Tier > sorted[j].Tier // higher trust first
		}
		return sorted[i].Key < sorted[j].Key
	})

	// The unresolved references present with the baseline (over the scaffold)
	// and no tenant fragment — the reference floor a candidate is diffed
	// against. Subtracting it means a candidate is judged only on the dangling
	// references IT introduces, so a (code-owned) baseline that itself carried
	// one cannot reject every tenant fragment. Best-effort: a compose failure
	// here leaves the floor empty, which is strictly more conservative.
	baseUnresolved := map[UnresolvedReference]struct{}{}
	if base, err := composeAllParseOnly(nil, baseline, nil); err == nil {
		if refs, rerr := UnresolvedReferences(base); rerr == nil {
			for _, r := range refs {
				baseUnresolved[r] = struct{}{}
			}
		}
	}

	// The references still unresolved with the baseline and EVERY candidate
	// present — the set that is genuinely dangling no matter who is accepted.
	//
	// The walk below judges a candidate against the already-accepted PREFIX,
	// which is a fine proxy for a permission expression (it resolves locally)
	// and a bad one for a direct relation's SUBJECT TYPES (they resolve against
	// other definitions). A fragment typed onto a sibling candidate's type is
	// dangling only in the prefix view, and the verdict then depends on which
	// of the two sorts first — while the pair composes cleanly at the real
	// WriteSchema, which is the only judgement that matters.
	//
	// So a candidate is blamed only for a reference that does not resolve even
	// with every sibling present. This is the enforcement layer for those
	// findings, and the fix is the FLOOR rather than the check: skipping
	// SubjectType findings here would let a genuinely dangling tenant type
	// through to SpiceDB and freeze the cluster-wide schema, which is what this
	// partition exists to prevent.
	//
	// Best-effort, exactly like baseUnresolved above, and the fallback matters:
	// when the all-candidates compose FAILS (two candidates collide — the
	// ordinary case for this function), allKnown stays false and every
	// non-floor finding is blamed as before. An empty set treated as
	// authoritative would instead accept everything.
	allUnresolved := map[UnresolvedReference]struct{}{}
	var allUnresolvedOrdered []UnresolvedReference
	allKnown := false
	if full, err := composeAllParseOnly(sorted, baseline, nil); err == nil {
		if refs, rerr := UnresolvedReferences(full); rerr == nil {
			for _, r := range refs {
				allUnresolved[r] = struct{}{}
				if _, floor := baseUnresolved[r]; !floor {
					allUnresolvedOrdered = append(allUnresolvedOrdered, r)
				}
			}
			allKnown = true
		}
	}

	// Blame follows the DECLARATION, not the trial that happened to surface it.
	//
	// The incremental walk attributes a finding to whichever candidate's trial
	// exposed it, which was sound while a finding could only come from the
	// fragment just added. Sibling resolution breaks that: a reference like
	// `aaa_consumer { relation x: zz_provider#nosuchrel }` cannot even be
	// EVALUATED until the fragment declaring zz_provider joins the trial, so
	// the finding first appears while judging the innocent provider — and
	// rejecting the provider both punishes the wrong CR and leaves the real
	// dangling reference in the accepted set, where it reaches SpiceDB and
	// freezes the cluster-wide schema.
	//
	// So a reference that dangles with EVERY candidate present is settled here,
	// up front, against the fragment that declares the definition holding it.
	// Nothing about sort order can change that verdict.
	preRejected := map[string]error{}
	if allKnown {
		for _, c := range sorted {
			declared := declaredDefinitionNames(c.Fragment)
			for _, r := range allUnresolvedOrdered {
				if _, mine := declared[r.Definition]; !mine {
					continue
				}
				if _, already := preRejected[c.Key]; already {
					continue // first finding in deterministic order wins
				}
				preRejected[c.Key] = fmt.Errorf("fragment introduces a dangling permission reference: %s", r.String())
			}
		}
	}

	for _, c := range sorted {
		// Settled above, independently of sort order. Handled inside the walk
		// rather than in its own loop so `rejected` stays in candidate order.
		if err, bad := preRejected[c.Key]; bad {
			rejected = append(rejected, RejectedFragment{Key: c.Key, Err: err})
			continue
		}
		trial := append(append([]IdentifiedFragment{}, accepted...), c)
		// Compose against the channel-kind baseline, exactly as RunAll does, so
		// a collision with a channel-kind definition is caught HERE (costing one
		// CR's availability) rather than at the real write (costing the cluster).
		composed, err := composeAllParseOnly(trial, baseline, nil)
		if err != nil {
			rejected = append(rejected, RejectedFragment{
				Key:         c.Key,
				Err:         err,
				DisplacedBy: attributeDisplacement(baseline, accepted, c),
			})
			continue
		}
		// A dangling permission reference compiles cleanly but is rejected by
		// SpiceDB's WriteSchema — the same cluster-wide freeze a collision
		// causes. ValidateFragment catches the SELF-CONTAINED case (scaffold
		// only), but an arrow onto a baseline or already-accepted type resolves
		// only here, where those types are present. Rejecting the candidate that
		// introduces a NEW unresolved reference isolates it to its own CR.
		if refs, rerr := UnresolvedReferences(composed); rerr == nil {
			for _, r := range refs {
				if _, floor := baseUnresolved[r]; floor {
					continue // pre-existing, not this candidate's doing
				}
				if _, still := allUnresolved[r]; allKnown && !still {
					// Resolves once every sibling is present, so the prefix
					// view is what is wrong here, not the candidate.
					continue
				}
				rejected = append(rejected, RejectedFragment{
					Key: c.Key,
					Err: fmt.Errorf("fragment introduces a dangling permission reference: %s", r.String()),
				})
				goto nextCandidate
			}
		}
		accepted = append(accepted, c)
	nextCandidate:
	}

	accepted, rejected = convergeAccepted(baseline, accepted, rejected, baseUnresolved)
	return accepted, rejected
}

// convergeAccepted makes "the accepted set composes with nothing dangling" a
// PROPERTY of what this function returns, rather than something that happens to
// fall out of the walk order.
//
// The walk judges each candidate once, against the set as it stood at that
// moment, and never revisits. That is fine until a fragment leaves the set
// after someone was accepted on the strength of it: the pre-rejection removes a
// fragment for a reference of its OWN, and every definition it also declared
// goes with it. A sibling that typed onto one of those was judged while it was
// still a candidate, so its reference resolved then and it was accepted —
// correctly, at that moment — and nothing looks at it again.
//
// RunAll then composes the accepted set and writes it. Its own
// ValidateComposedSchema gate would refuse that reference in-process, before
// WriteSchema is ever called — but a refusal there still freezes the schema
// for every AgentSessionGrants in the cluster, because the gate has no
// fragment to isolate and blame at that point: the single failure this whole
// partition exists to prevent.
//
// So the invariant is enforced directly. Compose what is accepted, take the
// references that dangle (minus the baseline floor, which is nobody's fault),
// reject the fragments DECLARING the definitions that hold them — same
// declaration-based blame the pre-rejection uses, so an operator is sent to the
// CR that actually names the missing type — and repeat, because removing those
// can strand a further layer. It terminates: every round removes at least one
// fragment from a finite set.
//
// Best-effort in the same sense as baseUnresolved and allUnresolved: a compose
// or resolve failure leaves `accepted` exactly as it stands. Emptying the set
// because a compose failed would turn a diagnostic gap into a cluster-wide
// outage, which is the opposite of this function's job.
//
// Cost is one extra compose in the common case — the first round finds nothing
// and returns.
func convergeAccepted(
	baseline []*spiceboxv1alpha1.SpiceDBSchemaFragment,
	accepted []IdentifiedFragment,
	rejected []RejectedFragment,
	baseUnresolved map[UnresolvedReference]struct{},
) ([]IdentifiedFragment, []RejectedFragment) {
	for len(accepted) > 0 {
		composed, err := composeAllParseOnly(accepted, baseline, nil)
		if err != nil {
			return accepted, rejected
		}
		refs, rerr := UnresolvedReferences(composed)
		if rerr != nil {
			return accepted, rejected
		}

		// definition name → the accepted fragments declaring it. Built once per
		// round rather than per finding: declaredDefinitionNames re-runs a
		// regex over a fragment's rawZed, and the naive nesting would repeat
		// that for every finding. A name can legitimately map to several
		// fragments — EmitSpicedbSchema permits a byte-identical
		// redeclaration — and blaming all of them is what keeps one round
		// enough for that case.
		declaredBy := map[string][]string{}
		for _, a := range accepted {
			for def := range declaredDefinitionNames(a.Fragment) {
				declaredBy[def] = append(declaredBy[def], a.Key)
			}
		}

		// blame: accepted key → the first dangling reference it is answerable
		// for, in the deterministic order UnresolvedReferences returns.
		blame := map[string]UnresolvedReference{}
		for _, r := range refs {
			if _, floor := baseUnresolved[r]; floor {
				continue
			}
			for _, key := range declaredBy[r.Definition] {
				if _, already := blame[key]; !already {
					blame[key] = r
				}
			}
		}
		if len(blame) == 0 {
			return accepted, rejected
		}

		// Filter in place, preserving the accepted order the walk established —
		// RunAll composes this slice, and the tier/Key ordering is what makes a
		// rejection attributable.
		survivors := accepted[:0]
		for _, a := range accepted {
			r, bad := blame[a.Key]
			if !bad {
				survivors = append(survivors, a)
				continue
			}
			rejected = append(rejected, RejectedFragment{
				Key: a.Key,
				Err: fmt.Errorf(
					"fragment has a dangling permission reference once the fragment declaring its type was itself rejected: %s",
					r.String()),
			})
		}
		accepted = survivors
	}
	return accepted, rejected
}

// declaredDefinitionNames is every object definition a fragment declares, from
// both halves a fragment can declare one in: the structured Resources[].Name
// list and any `definition <name>` in its RawZed. Same pair ValidateFragment
// checks reserved names against, so the two can never disagree about what a
// fragment owns.
func declaredDefinitionNames(frag *spiceboxv1alpha1.SpiceDBSchemaFragment) map[string]struct{} {
	out := map[string]struct{}{}
	if frag == nil {
		return out
	}
	for _, r := range frag.Resources {
		out[r.Name] = struct{}{}
	}
	for _, name := range rawZedDefinitionNames(frag.RawZed) {
		out[name] = struct{}{}
	}
	return out
}

// attributeDisplacement identifies which already-accepted fragment(s) caused
// candidate's trial compose to fail, for RejectedFragment.DisplacedBy. See
// that field's doc comment for the exact rule and its limits.
func attributeDisplacement(baseline []*spiceboxv1alpha1.SpiceDBSchemaFragment, accepted []IdentifiedFragment, candidate IdentifiedFragment) string {
	var reproduced []string
	for _, a := range accepted {
		pair := []IdentifiedFragment{a, candidate}
		if _, err := composeAllParseOnly(pair, nil, nil); err != nil {
			reproduced = append(reproduced, a.Key)
		}
	}
	// A collision with the channel-kind baseline reproduces against NO accepted
	// tenant fragment, so without this it would misattribute to the whole
	// accepted set (or "" when none is accepted yet). Name the real cause.
	if _, err := composeAllParseOnly([]IdentifiedFragment{candidate}, baseline, nil); err != nil {
		reproduced = append(reproduced, "channel-kind schema")
	}
	if len(reproduced) == 0 {
		// No single accepted fragment reproduces the failure alone — fall
		// back to the whole set the candidate was trial-composed against.
		reproduced = make([]string, 0, len(accepted))
		for _, a := range accepted {
			reproduced = append(reproduced, a.Key)
		}
	}
	sort.Strings(reproduced)
	return strings.Join(reproduced, ", ")
}
