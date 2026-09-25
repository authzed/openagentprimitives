// pkg/platform/relsync/sync.go
//
// Pass is the kind-agnostic reconcile algorithm: enumerate, resume, fetch,
// diff, write, and -- only against a provably complete enumeration -- reap.
// Everything here runs against a Kind, a Writer and a Reader; no controller,
// no network, and (in production) every write still passes through the
// relsource-guarded RelWriter a caller binds Writer to. Pass never writes
// anything a kind didn't itself fetch; it consults the kind's own claims in
// exactly one place, the cross-resource reap, where "may I write this?" is
// the only honest way to answer "may I delete this?" -- see
// reapAbsentCrossResourceTuples.
//
// Four properties this file exists to keep true (see AGENTS.md):
//
//  1. A truncated enumeration never reaps. ScopePage.Complete, not
//     emptiness, gates step 5.
//  2. A scope is never written half-fetched. FetchScope returns a complete
//     set or an error; on error, nothing is written or pruned for that
//     scope.
//  3. "Gone" and "empty" are different. ErrScopeGone sweeps the whole
//     scope object; an empty fetch prunes only that scope's members via
//     the ordinary diff.
//  4. Resume compares, never indexes. The next pass starts at the first
//     scope sorting after ResumeAfter -- a scope deleted between passes
//     cannot strand the cursor.
//
// A fifth, narrower to the cross-resource half (Ruling 4: a fetch may
// assert a tuple on a resource other than the scope's own object, e.g. a
// workspace-wide identity edge): that reap NEVER judges a resource by its
// id alone. A resource id can still be asserted by SOMETHING this pass
// while one of its OWN tuples goes stale (a changed subject), so
// reapAbsentCrossResourceTuples diffs at tuple granularity -- see its own
// doc, and PassResult.Pruned vs ReapedScopes for where each half's count
// lands.
package relsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// relhashRelation is the sentinel relation every kind's #relhash claim
// protects (pkg/authz/spicedb/schema/schema.zed's `definition string {}`,
// and each kind's Source().Claims -- see kind.go's doc on Kind.Source).
// Declared once so the read filter, the CAS precondition and the TOUCH
// update can never drift from each other by a typo.
const relhashRelation = "relhash"

// relhashSubjectType is the sentinel's subject type. SpiceDB has no
// string-valued relation, so a scope's content hash rides as a subject id
// on this shared, permission-less type.
const relhashSubjectType = "string"

// Writer is the guarded write surface. Satisfied by spicedb.RelWriter, so
// every write is checked against the kind's own claims.
type Writer interface {
	WriteRelationships(ctx context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error)
	DeleteRelationships(ctx context.Context, req *v1.DeleteRelationshipsRequest) (*v1.DeleteRelationshipsResponse, error)
}

// Reader reads back what a source owns. Satisfied by *spicedb.Client.
// Reads are never guarded — reading a managed relation is normal.
type Reader interface {
	ReadRelationships(ctx context.Context, req *v1.ReadRelationshipsRequest) (v1.PermissionsService_ReadRelationshipsClient, error)
}

// PassInput is everything one Pass needs. Nothing here is mutated; the
// caller reads the returned PassResult to decide what to persist.
type PassInput struct {
	Kind   Kind
	Creds  SourceParams
	Writer Writer
	Reader Reader
	// ResumeAfter is status.sync.resumeAfter, converted with ScopeID(s).
	ResumeAfter ScopeID
	// MaxScopes bounds how many scopes sorting after ResumeAfter
	// ("kept") enumeration collects before it stops paging further, per
	// pass. 0 = unbounded. This is an approximate cap, not an exact one:
	// enumerate checks whether upstream itself is done (page.Next == the
	// zero Cursor) BEFORE it checks the budget, so reaching the true end
	// always wins over stopping early and the final page can overshoot
	// maxScopes. Enumeration still accumulates and returns the FULL scope
	// list it paged through (including the already-processed prefix at or
	// before ResumeAfter) — see enumerate's doc comment — but step 4 only
	// ever processes the resume-filtered subset, so this is what actually
	// bounds how many scopes get fetched. A cap hit before the kind's own
	// pagination says it is done forces EnumComplete=false: a self-imposed
	// truncation is exactly as untrustworthy for reaping as an upstream
	// one (see kind.go's ScopePage.Complete).
	//
	// Opting into a bound also defers cross-resource reaping (see Pass's
	// own doc on extraResources): that half only ever runs on a pass that
	// FETCHED every currently enumerated scope, which a budget smaller than
	// the source makes true only once ResumeAfter is empty again — i.e.
	// never, for the life of a source whose scope count permanently
	// exceeds its bound. This is a real trade a bound makes, not a bug;
	// leave MaxScopes at 0 unless a source's scope count genuinely needs
	// the budget.
	MaxScopes int
	// IgnoreHashes forces a full verify pass: every scope is read and
	// diffed even when its sentinel matches. See step 3.
	IgnoreHashes bool
	// OnlyScopes restricts the pass to these scopes — the scoped wake an
	// upstream event produces. When set, reaping is skipped unconditionally:
	// a scope cannot be shown absent without enumerating, so a scoped pass
	// has no basis to delete anything it did not fetch. Enumeration still
	// runs (a scope's ResourceType has to come from somewhere), but the
	// resume cursor is left untouched: a scoped pass is orthogonal to the
	// background cycle, not a step in it.
	OnlyScopes []ScopeID
}

// ErrRefusedPrune is the (scope-less) ScopeError.Err step 5 reports when a
// complete enumeration came back with zero scopes — see Pass's own "four
// kinds of empty" comment below. A caller classifying ScopeErrors should
// use errors.Is(err, ErrRefusedPrune) rather than matching this package's
// message text, which is free to reword.
var ErrRefusedPrune = errors.New("relsync: enumeration returned zero scopes; refusing to reap")

// ErrEnumerationNotAdvancing is the ScopeError.Err reported when a kind's
// upstream keeps handing back a cursor claiming more scopes remain while
// serving pages that cannot move the enumeration forward. Classify with
// errors.Is; the message names the kind.
//
// It exists because a page can no longer end an enumeration by being FULL.
// ScopePage.Complete is deliberately never reflex-true for a full page — that
// reading let one page stand in for a whole directory and armed the reaper
// against everything past it — so the only remaining terminator a kind
// controls is a zero Next. An upstream that ignores its cursor parameter (or a
// proxy that strips it) therefore paginates forever: it returns page one every
// time, and the numeric cursor keeps advancing while the content does not.
// Probed at 5001 requests and 500,100 scopes in an in-memory slice inside a
// second, with no end in sight.
//
// The bound is structural rather than a page cap, and that is the whole point:
// a cap truncates enumeration silently, and a truncated enumeration that still
// reported Complete would re-arm the very reap this area exists to prevent. A
// stall is reported the same way any enumeration failure is — EnumComplete
// false, nothing reaped, the error on the CR's status — because "one scope
// failing is not a pass failure" applies to enumeration too.
var ErrEnumerationNotAdvancing = errors.New("relsync: enumeration is not advancing")

// ScopeError is one scope's failure during a Pass. Never fatal to the pass
// as a whole. Scope is empty when the failure isn't attributable to a
// single scope — the step-5 reap scan itself failing, for instance.
type ScopeError struct {
	Scope ScopeID
	Err   error
}

func (e ScopeError) Error() string {
	if e.Scope == "" {
		return e.Err.Error()
	}
	return fmt.Sprintf("scope %s: %s", e.Scope, e.Err.Error())
}

// PassResult reports what one Pass did, for the controller to fold into
// status and to decide what (if anything) to alert on.
type PassResult struct {
	// Processed is the count of scopes step 4 actually visited (after the
	// resume skip / OnlyScopes filter), regardless of outcome.
	Processed int
	// Written is the count of individual relationship tuples TOUCHed as
	// additions across every scope this pass diffed. The per-scope sentinel
	// TOUCH is not counted here — it isn't scope content, it's the sentinel
	// describing it.
	//
	// A scope's display-name #label IS counted, and the asymmetry with the
	// sentinel is the correct one rather than an oversight: a label is returned
	// by the kind inside ScopeContent.Tuples and is diffed, hashed and pruned as
	// ordinary content (see ScopeLabelTuple), so excluding it would mean
	// special-casing a relation this file otherwise knows nothing about.
	//
	// The visible consequence is one extra counted write per scope on the single
	// pass that backfills a directory synced before labels existed — after which
	// the content hash matches again and those scopes stop being written at all.
	// It does not churn anything: the only non-display consumer is the
	// reconciler's passWrote, which uses a non-zero count to force a status
	// write, and on that pass SpiceDB genuinely did change.
	Written int
	// Pruned is the count of individual relationship tuples deleted via the
	// ordinary per-scope diff (present before, absent from the fresh fetch)
	// OR via the cross-resource tuple-level reap (reapAbsentCrossResourceTuples
	// — a workspace-wide identity edge whose SUBJECT changed, say). Both are
	// tuple-level operations; a whole-scope sweep (ErrScopeGone, or the
	// scope-level half of step 5's reap) is counted in ReapedScopes instead,
	// not here — a scope-level fact, not a tuple-level one.
	Pruned int
	// ReapedScopes is the count of scopes whose entire resource object was
	// swept by one broad DeleteRelationships call — because FetchScope
	// reported ErrScopeGone, or (step 5) because a complete enumeration
	// never mentioned it.
	ReapedScopes int
	// JoinMisses sums ScopeContent.JoinMisses across every scope this pass
	// fetched — upstream members whose identity resolved to no platform
	// user, dropped rather than written. Summed even for a scope that hits
	// the hash short-circuit below: FetchScope has already run by the time
	// that decision is made, so the count is available regardless of
	// whether SpiceDB's stored content changed.
	JoinMisses int
	// ResumeAfter is the next pass's starting point: the last scope this
	// pass finished, or "" once a full cycle completes. Unchanged (equal to
	// PassInput.ResumeAfter) for a scoped (OnlyScopes) pass.
	ResumeAfter ScopeID
	// EnumComplete reports whether this pass's enumeration was provably
	// whole — the gate on step 5. False whenever the kind's own pagination
	// stopped early OR this pass's own MaxScopes cut it off first.
	EnumComplete bool
	// CycleComplete is true once a (non-scoped) pass has processed every
	// scope a complete enumeration reported. ResumeAfter resets to "" in
	// the same case, so the next pass starts a fresh cycle.
	CycleComplete bool
	// ScopeErrors is one entry per scope (or reap-scan) that failed. Never
	// fatal to the pass — see the no-silent-errors rule in AGENTS.md: every
	// failure here is recorded, none are swallowed.
	ScopeErrors []ScopeError
}

// HashTuples returns a hex sha256 digest of tuples, sorted by Tuple.Key()
// first — so the same set of tuples hashes identically regardless of fetch
// order. Without the sort, an upstream API with no stable ordering would
// make the sentinel short-circuit (step 3) treat an unchanged scope as
// changed on every single pass.
func HashTuples(tuples []spicedb.Tuple) string {
	keys := make([]string, 0, len(tuples))
	for _, t := range tuples {
		keys = append(keys, t.Key())
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0}) // separator: keys can never collide across a boundary
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Pass runs one sync pass. See the package doc for the four properties it
// exists to keep true, and the plan brief
// (.superpowers/sdd/2026-09-10-relationship-source-slack/task-4-brief.md)
// for the six-step algorithm this implements verbatim.
//
// The returned error is reserved for a failure this function cannot even
// attempt to recover from; every failure the algorithm anticipates —
// ListScopes erroring or truncating, a scope's fetch or write failing, the
// step-5 reap scan failing — is folded into PassResult.ScopeErrors instead,
// never returned here. That is deliberate: "one scope failing is not a pass
// failure" (see TestPass_FetchErrorWritesAndPrunesNothingForThatScope)
// applies just as much to an enumeration hiccup as to one scope's fetch.
func Pass(ctx context.Context, in PassInput) (PassResult, error) {
	var result PassResult

	scopes, enumComplete, enumErr := enumerate(ctx, in.Kind, in.Creds, in.MaxScopes, in.ResumeAfter)
	result.EnumComplete = enumComplete
	if enumErr != nil {
		result.ScopeErrors = append(result.ScopeErrors, ScopeError{Err: fmt.Errorf("relsync: enumerate scopes: %w", enumErr)})
	}

	sort.Slice(scopes, func(i, j int) bool { return scopes[i].ID < scopes[j].ID })

	scoped := len(in.OnlyScopes) > 0
	var toProcess []Scope
	if scoped {
		want := make(map[ScopeID]struct{}, len(in.OnlyScopes))
		for _, id := range in.OnlyScopes {
			want[id] = struct{}{}
		}
		matched := make(map[ScopeID]struct{}, len(in.OnlyScopes))
		for _, s := range scopes {
			if _, ok := want[s.ID]; ok {
				toProcess = append(toProcess, s)
				matched[s.ID] = struct{}{}
			}
		}
		// A requested scope absent from enumeration is not nothing — the
		// credential may not be able to see it any more, or it may be gone
		// — and a scoped wake that silently does nothing for it is exactly
		// the no-silent-errors rule's failure mode: the event that asked
		// for this scope gets no sync AND no signal that anything was
		// missed.
		for _, id := range in.OnlyScopes {
			if _, ok := matched[id]; !ok {
				result.ScopeErrors = append(result.ScopeErrors, ScopeError{
					Scope: id,
					Err:   errors.New("relsync: requested for a scoped pass but absent from enumeration"),
				})
			}
		}
	} else {
		// Resume COMPARES: the first scope sorting AFTER ResumeAfter, never
		// a positional index — a scope deleted between passes (precisely
		// the case orphan reaping exists for) must not strand the cursor or
		// skip its neighbour.
		start := 0
		for start < len(scopes) && scopes[start].ID <= in.ResumeAfter {
			start++
		}
		toProcess = scopes[start:]
	}

	// extraResources accumulates, across every scope this pass actually
	// FETCHED (or confirmed ErrScopeGone — see fetchCoverage below), every
	// tuple whose resource is NOT that scope's own object (Ruling 4's
	// cross-resource case — a workspace-wide identity edge, say). Ruling 4
	// grants that these ride additively through whichever scope still
	// asserts them, reaped only on a pass that fetched every enumerated
	// scope — this is that union.
	//
	// Keyed first by resource type, then by the tuple's own Key() — not by
	// resource id alone. A resource id surviving in the union says nothing
	// about which of ITS OWN tuples survived: a changed SUBJECT (a Slack
	// user's email changing, say) produces a tuple with a different Key()
	// entirely, so the OLD tuple silently drops out of the union the moment
	// the fetch stops asserting it, even though the resource id itself
	// (still asserted, just with a new subject) never leaves. An id-keyed
	// union could only ever decide whether to sweep a whole object; this
	// tuple-keyed one is what lets the cross-resource reap delete exactly
	// the stale edge and nothing else (see reapAbsentCrossResourceTuples).
	extraResources := map[string]map[string]spicedb.Tuple{}

	// fetchCoverage counts scopes this pass actually OBSERVED — FetchScope
	// either succeeded or reported ErrScopeGone (which legitimately asserts
	// nothing, a true empty contribution to the union). A scope whose fetch
	// failed any OTHER way contributes nothing to extraResources
	// (processScope returns before ever reaching recordExtraResources), so
	// counting pass COVERAGE by scope-list length alone — as an earlier
	// version of this fix did — is wrong: toProcess/scopes can be equal in
	// SIZE while one of those scopes' current cross-resource assertions
	// are simply unknown this pass. The union is trustworthy only when
	// fetchCoverage reaches len(scopes) exactly — every enumerated scope
	// either reported its current truth or confirmed it has none.
	var fetchCoverage int

	resumeAfter := in.ResumeAfter
	for _, scope := range toProcess {
		result.Processed++
		covered, err := processScope(ctx, in, scope, &result, extraResources)
		if err != nil {
			result.ScopeErrors = append(result.ScopeErrors, ScopeError{Scope: scope.ID, Err: err})
		}
		if covered {
			fetchCoverage++
		}
		if !scoped {
			resumeAfter = scope.ID
		}
	}

	switch {
	case scoped:
		// A scoped pass is orthogonal to the background cycle: it neither
		// advances nor resets the cursor the cycle uses.
		result.ResumeAfter = in.ResumeAfter
	case enumComplete:
		// step 4 always runs to the end of the (already MaxScopes-bounded)
		// accumulated list, so reaching here after a complete enumeration
		// means the whole cycle is done.
		result.CycleComplete = true
		result.ResumeAfter = ""
	default:
		result.ResumeAfter = resumeAfter
	}

	// Reaping, only against a complete, unscoped enumeration: a scope
	// cannot be shown absent without enumerating, and a scope cannot be
	// trusted absent from a truncated enumeration either.
	if !scoped && enumComplete {
		if len(scopes) == 0 {
			// The design's own "four kinds of empty" table: ListScopes
			// returning nothing at all means the API is unreliable right
			// now, not that this source genuinely has zero scopes — refuse
			// the prune explicitly and say so, rather than relying on the
			// known-resource map happening to be empty (which it currently
			// would be regardless, but a refactor that derives scan targets
			// from the source's claims instead of this pass's own
			// enumeration would remove that accidental safety silently).
			result.ScopeErrors = append(result.ScopeErrors, ScopeError{
				Err: ErrRefusedPrune,
			})
		} else {
			// Scope-level reap: built PURELY from enumeration, and always
			// run when we get here — existence needs only enumerating,
			// never fetching, so it has no coverage gap of its own. This
			// map is kept entirely separate from the cross-resource one
			// below (see that block's own comment for why merging them
			// used to be unsafe).
			known := map[string]map[string]struct{}{}
			for _, s := range scopes {
				m, ok := known[s.ResourceType]
				if !ok {
					m = map[string]struct{}{}
					known[s.ResourceType] = m
				}
				m[string(s.ID)] = struct{}{}
			}
			reapAbsentScopes(ctx, in, known, &result)

			// Cross-resource reap: built PURELY from extraResources, and
			// run only when fetchCoverage proves every enumerated scope
			// reported its current truth this pass (fetched, or confirmed
			// ErrScopeGone). Deferred otherwise — see MaxScopes's own doc
			// comment on what a bound costs a source. No cross-cycle
			// accumulation is attempted: that would need PERSISTED state
			// (surviving restarts, threaded back in via a new PassInput
			// field) that nobody has designed, versus simply waiting for
			// an eventual pass that covers everything in one go.
			//
			// A resource type that is ALSO a scope type (present in known)
			// is excluded here entirely, never merged in. A stray tuple a
			// fetch asserts against another object of a SCOPE type (not
			// just a different type — e.g. one channel's fetch mentioning
			// a DIFFERENT channel's id) used to get merged into the same
			// map the scope-level reap used, which could PROTECT an
			// unenumerated object from the scope-level reap on a pass
			// where the merge ran, while a pass where it didn't reaped the
			// very same object — a mode-dependent divergence for identical
			// real state, for the exact reason a merge is unsafe (see
			// IMPORTANT 2, fix round 2, and its follow-up here). Keeping
			// the maps disjoint means the scope-level reap's answer for
			// any given type never depends on whether the cross-resource
			// gate happened to be open this pass. See
			// TestPass_SameTypeStrayReferenceNeverProtectsAnUnenumeratedScope.
			//
			// reapAbsentCrossResourceTuples, not reapAbsentScopes: a
			// cross-resource type has no scope of its own, so "absent"
			// cannot be judged by resource id alone the way a vanished
			// scope object can — the id can survive a pass while one of
			// its OWN tuples goes stale (a changed subject). This is the
			// tuple-granularity half of the whole-branch review's Critical
			// 2; see reapAbsentCrossResourceTuples's own doc.
			if fetchCoverage == len(scopes) {
				crossKnown := map[string]map[string]spicedb.Tuple{}
				for resourceType, tuples := range extraResources {
					if _, isScopeType := known[resourceType]; isScopeType {
						continue
					}
					crossKnown[resourceType] = tuples
				}
				reapAbsentCrossResourceTuples(ctx, in, crossKnown, &result)
			}
		}
	}

	return result, nil
}

// enumerate loops ListScopes from the zero cursor, in memory, accumulating
// every DISTINCT scope seen (the full set, unfiltered — step 5's reap needs
// the complete inventory whenever EnumComplete ends up true, not just the
// slice this pass will process). The returned bool is EnumComplete: true
// only when the kind's own pagination said it was done (Next zero, that
// page's Complete true) before maxScopes ever cut it short. An error mid
// loop is returned alongside whatever scopes were already accumulated —
// the caller treats that exactly like a truncated (Complete: false)
// enumeration: sync what was seen, reap nothing.
//
// The budget is counted against scopes that sort AFTER resumeAfter only
// ("kept"), never against the total seen. Counting the total was the
// deadlock this replaced: enumeration always restarts at Cursor{} every
// pass (an upstream cursor expires and is deliberately never persisted —
// see kind.go's Cursor doc), so a budget counted from the beginning would
// re-derive the SAME prefix every single pass. Once ResumeAfter had
// advanced past that whole prefix, the budget was exhausted by scopes that
// were immediately filtered back out by Pass's own resume-skip, "kept"
// would be zero, and a source with more scopes than the budget would never
// converge — see TestPass_ConvergesAcrossRepeatedPassesWithASmallBudget,
// which fails against that version of this function. Enumeration itself
// still restarts from the beginning every pass (that part stays correct:
// it is the price of not persisting an expiring upstream cursor) — it now
// simply keeps paging PAST the already-processed prefix, without letting
// that prefix count against the budget, until it has collected maxScopes
// scopes beyond the cursor (or upstream genuinely runs out).
//
// # Two structural bounds, because a full page can no longer end the loop
//
// This is a bare `for {}` whose only terminators are a zero Next, an error,
// and the OPTIONAL maxScopes — and unbounded is the recommended default. Once
// a full page correctly stopped reporting Complete (ScopePage.Complete's own
// doc: a reflex-true there reaps everything past page one), an upstream that
// ignores its cursor parameter paginates forever, handing back page one with
// an ever-advancing cursor. Both bounds below refuse rather than cap: see
// ErrEnumerationNotAdvancing.
//
//   - A NON-EMPTY page contributing zero new scopes, with a non-zero Next, is
//     repeating itself. Emptiness is deliberately not part of it: an empty page
//     mid-enumeration is legal and must keep paging — Slack's
//     conversations.list applies ExcludeArchived server-side and genuinely
//     returns one with a live next_cursor.
//   - A Next equal to the cursor just SENT cannot advance by construction, and
//     is the one stuck shape an empty page can take. It is what keeps the
//     exemption above from being an unbounded loop of its own.
//
// Scopes are deduped as they accumulate, on the whole Scope rather than on ID
// alone: a kind enumerating several resource types may legitimately reuse an
// upstream id across them, and dropping one of those would be a silently
// missing scope — the bug the guard exists to prevent, introduced by the
// guard. A repeated PAGE repeats the whole value, so the stall is caught
// either way.
func enumerate(ctx context.Context, k Kind, creds SourceParams, maxScopes int, resumeAfter ScopeID) ([]Scope, bool, error) {
	var scopes []Scope
	seen := make(map[Scope]struct{})
	var kept int
	cursor := Cursor{}
	for {
		page, err := k.ListScopes(ctx, creds, cursor)
		if err != nil {
			return scopes, false, err
		}
		added := 0
		for _, s := range page.Scopes {
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			scopes = append(scopes, s)
			added++
			if s.ID > resumeAfter {
				kept++
			}
		}
		if page.Next == (Cursor{}) {
			return scopes, page.Complete, nil
		}
		if len(page.Scopes) > 0 && added == 0 {
			return scopes, false, fmt.Errorf(
				"%w: kind %q returned a page of %d scopes, none of them new, while handing back a cursor claiming more remain",
				ErrEnumerationNotAdvancing, k.Name(), len(page.Scopes))
		}
		if page.Next == cursor {
			return scopes, false, fmt.Errorf(
				"%w: kind %q handed back the cursor it was just given (%q)",
				ErrEnumerationNotAdvancing, k.Name(), page.Next.Token)
		}
		if maxScopes > 0 && kept >= maxScopes {
			// Self-imposed budget, not the kind's own signal: this pass
			// deliberately stopped before upstream said it was finished, so
			// EnumComplete must read false exactly as if the page itself had
			// reported it.
			return scopes, false, nil
		}
		cursor = page.Next
	}
}

// processScope fetches, diffs and writes (or prunes) one scope. Every
// return path either writes nothing or writes the whole diff in one call —
// a scope is never written half-fetched.
//
// covered reports whether this scope contributed its current truth to
// extraResources (see Pass's own doc on the variable and on
// fetchCoverage) — true once FetchScope has either succeeded or reported
// ErrScopeGone (a scope confirmed gone legitimately asserts nothing, a
// real empty contribution), false for any OTHER fetch error. covered is
// NOT affected by anything that goes wrong afterward (a sentinel read, a
// scope read, the write itself): the fetch is what determines the
// scope's current cross-resource assertions, and that already happened by
// the time any of those later steps could fail. Callers must not infer
// coverage from "this scope produced no error" — a fetch that succeeded
// and then failed to write is still covered; a fetch that itself failed
// is not, no matter how small an error it was.
func processScope(ctx context.Context, in PassInput, scope Scope, result *PassResult, extraResources map[string]map[string]spicedb.Tuple) (covered bool, err error) {
	content, err := in.Kind.FetchScope(ctx, in.Creds, scope)
	if err != nil {
		if errors.Is(err, ErrScopeGone) {
			// Gone is distinct from empty: sweep the whole resource object,
			// sentinel included.
			if delErr := pruneWholeScope(ctx, in.Writer, scope.ResourceType, string(scope.ID)); delErr != nil {
				return true, fmt.Errorf("prune gone scope: %w", delErr)
			}
			result.ReapedScopes++
			return true, nil
		}
		// Any other error: write and prune NOTHING for this scope, and — as
		// important as the write/prune refusal — this scope's cross-resource
		// assertions are UNKNOWN this pass, not empty. Treating this scope
		// as "covered" here is exactly the bug fix round 3 closed: a
		// transient fetch failure (a 429, a timeout) would otherwise make
		// every cross-resource tuple only this scope still needed look
		// orphaned, and the union reap would sweep it — a live-data
		// deletion caused by an upstream hiccup. See
		// TestPass_FailedFetchNeverDefeatsTheCrossResourceUnion.
		return false, fmt.Errorf("fetch: %w", err)
	}
	// Recorded regardless of what happens next (including the short-circuit
	// below): the fetch already ran, so the miss count is already known,
	// and it describes upstream reality independent of whether SpiceDB's
	// stored content changed.
	result.JoinMisses += content.JoinMisses
	fetched := content.Tuples
	recordExtraResources(extraResources, scope, fetched)

	oldHash, hadOldHash, err := readSentinel(ctx, in.Reader, scope)
	if err != nil {
		return true, fmt.Errorf("read sentinel: %w", err)
	}

	newHash := HashTuples(fetched)
	if !in.IgnoreHashes && hadOldHash && oldHash == newHash {
		// Unchanged: no read of the scope's owned tuples, no write. This is
		// the ENTIRE point of the sentinel — asserted by the absence of the
		// read below, not by the absence of a write.
		return true, nil
	}

	owned, err := readScopeTuples(ctx, in.Reader, scope)
	if err != nil {
		return true, fmt.Errorf("read scope: %w", err)
	}

	updates, additions, removals := diffScope(scope, owned, fetched, oldHash, hadOldHash, newHash)
	precond := sentinelPrecondition(scope, oldHash, hadOldHash)

	if _, err := in.Writer.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates:               updates,
		OptionalPreconditions: []*v1.Precondition{precond},
	}); err != nil {
		return true, fmt.Errorf("write: %w", err)
	}

	result.Written += additions
	result.Pruned += removals
	return true, nil
}

// recordExtraResources folds every tuple fetched for scope whose resource
// is NOT scope's own object into extraResources — see Ruling 4: a fetch
// can return a tuple on another resource entirely (a workspace-wide
// identity edge), which readScopeTuples/diffScope deliberately never see,
// so it has to be accounted for here or the resource it rides on is never
// reaped once nothing references it any more.
//
// Keyed by the tuple's own Key(), not by resource id: two tuples sharing a
// resource id but differing subjects (the same Slack user, old and new
// email) must both be representable in the union, so the reap can tell
// them apart later instead of only ever judging the id as a whole.
func recordExtraResources(extraResources map[string]map[string]spicedb.Tuple, scope Scope, fetched []spicedb.Tuple) {
	for _, t := range fetched {
		if t.ResourceType == scope.ResourceType && t.ResourceID == string(scope.ID) {
			continue
		}
		m, ok := extraResources[t.ResourceType]
		if !ok {
			m = map[string]spicedb.Tuple{}
			extraResources[t.ResourceType] = m
		}
		m[t.Key()] = t
	}
}

// readSentinel reads back the scope's #relhash tuple, if any — a single
// narrow read, distinct from (and always cheaper than) readScopeTuples'
// full-object read below. This is the read that always happens; the
// full-object read is the one step 3's short-circuit skips.
//
// Returns an error if more than one #relhash tuple is present. A changed
// hash is a changed SUBJECT (the hash rides as the subject id), so a
// TOUCH of the new one is a distinct relationship from the old one —
// diffScope now DELETEs the old sentinel in the same write that TOUCHes
// the new one (see diffScope's doc), specifically so a scope can never
// hold more than one at a time. Seeing two here again means that
// invariant broke somewhere, and "pick the last one Recv returned" would
// make "the current hash" silently depend on stream order — exactly the
// bug this replaces (Content A -> B -> A left a stored hA matching a
// fresh hA while SpiceDB still held B's tuples, so the scope stopped
// converging). Fail loudly instead.
func readSentinel(ctx context.Context, r Reader, scope Scope) (hash string, found bool, err error) {
	stream, err := r.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency: fullyConsistent(),
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       scope.ResourceType,
			OptionalResourceId: string(scope.ID),
			OptionalRelation:   relhashRelation,
		},
	})
	if err != nil {
		return "", false, err
	}
	var count int
	for {
		resp, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return "", false, rerr
		}
		count++
		if count > 1 {
			return "", false, fmt.Errorf("relsync: scope %s has more than one #relhash tuple — this should be impossible; the old sentinel must always be deleted in the same write that touches a new one. Nothing here self-heals a doubly-sentinelled scope: an operator must prune the whole resource object (a DeleteRelationships with no relation filter, exactly what pruneWholeScope issues) before this scope can sync again", scope.ID)
		}
		hash = resp.GetRelationship().GetSubject().GetObject().GetObjectId()
		found = true
	}
	return hash, found, nil
}

// readScopeTuples reads back every tuple SpiceDB currently holds on the
// scope's OWN resource object — bounded to <ResourceType>:<ScopeID>#*, and
// nothing else. This is Ruling 4's bound: a fetch can return tuples on
// OTHER resources entirely (an identity edge that is workspace-wide, not
// scope-owned), and this read must never see them, or a per-scope diff
// would treat another scope's still-needed edge as absent from THIS
// scope's old set and prune it. See
// TestPass_SharedResourceTupleAcrossScopesIsNeverPruned.
//
// The sentinel tuple itself is excluded — it's diffed separately via
// readSentinel/sentinelPrecondition, never as ordinary scope content.
func readScopeTuples(ctx context.Context, r Reader, scope Scope) ([]spicedb.Tuple, error) {
	stream, err := r.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency: fullyConsistent(),
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       scope.ResourceType,
			OptionalResourceId: string(scope.ID),
		},
	})
	if err != nil {
		return nil, err
	}
	var out []spicedb.Tuple
	for {
		resp, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
		rel := resp.GetRelationship()
		if rel.GetRelation() == relhashRelation {
			continue
		}
		subj := rel.GetSubject()
		out = append(out, spicedb.Tuple{
			ResourceType:    rel.GetResource().GetObjectType(),
			ResourceID:      rel.GetResource().GetObjectId(),
			Relation:        rel.GetRelation(),
			SubjectType:     subj.GetObject().GetObjectType(),
			SubjectID:       subj.GetObject().GetObjectId(),
			SubjectRelation: subj.GetOptionalRelation(),
		})
	}
	return out, nil
}

// diffScope computes the additions (in fetched, not in owned), the
// removals (in owned, not in fetched), the DELETE of the old sentinel
// (when one existed and the content changed) and the new sentinel TOUCH,
// as one ordered update list — the whole point being that all four ride
// in a single WriteRelationships call. owned is assumed already bounded to
// the scope's own resource (see readScopeTuples); diffScope adds no
// further bound itself.
//
// The old-sentinel DELETE is not optional. A relationship is keyed by
// (resource, relation, subject), and the hash rides as the subject id, so
// TOUCHing a new hash creates a NEW tuple rather than updating the old
// one — readScopeTuples deliberately excludes #relhash from owned (it's
// diffed here, separately, not as ordinary scope content), so nothing else
// in this pass ever computes a removal for it. Left alone, the scope
// accumulates one #relhash tuple per content state it has ever had,
// readSentinel's "the current hash" becomes stream-order dependent once
// more than one exists (see its own doc), and the MUST_MATCH precondition
// below ends up satisfiable by a stale sentinel forever, from the second
// write on — it stops guarding anything. See
// TestPass_ChangedSentinelDeletesTheOldOne.
func diffScope(scope Scope, owned, fetched []spicedb.Tuple, oldHash string, hadOldHash bool, newHash string) (updates []*v1.RelationshipUpdate, additions, removals int) {
	oldByKey := make(map[string]spicedb.Tuple, len(owned))
	for _, t := range owned {
		oldByKey[t.Key()] = t
	}
	newByKey := make(map[string]spicedb.Tuple, len(fetched))
	for _, t := range fetched {
		newByKey[t.Key()] = t
	}

	for k, t := range newByKey {
		if _, existed := oldByKey[k]; !existed {
			updates = append(updates, touchUpdate(t))
			additions++
		}
	}
	for k, t := range oldByKey {
		if _, still := newByKey[k]; !still {
			updates = append(updates, deleteUpdate(t))
			removals++
		}
	}
	if hadOldHash && oldHash != newHash {
		updates = append(updates, sentinelDeleteUpdate(scope, oldHash))
	}
	updates = append(updates, sentinelTouchUpdate(scope, newHash))

	sort.Slice(updates, func(i, j int) bool { return updateSortKey(updates[i]) < updateSortKey(updates[j]) })
	return updates, additions, removals
}

// sentinelPrecondition is the CAS guard tying the sentinel to the write it
// rides with: MUST_MATCH the hash this pass read, so a hash changed by
// anyone else between the read and this write fails the whole call rather
// than silently overwriting it; MUST_NOT_MATCH (any subject) when no
// sentinel existed yet, so a first write can't race a write nobody read
// back.
func sentinelPrecondition(scope Scope, oldHash string, hadOldHash bool) *v1.Precondition {
	filter := &v1.RelationshipFilter{
		ResourceType:       scope.ResourceType,
		OptionalResourceId: string(scope.ID),
		OptionalRelation:   relhashRelation,
	}
	if hadOldHash {
		filter.OptionalSubjectFilter = &v1.SubjectFilter{
			SubjectType:       relhashSubjectType,
			OptionalSubjectId: oldHash,
		}
		return &v1.Precondition{Operation: v1.Precondition_OPERATION_MUST_MATCH, Filter: filter}
	}
	return &v1.Precondition{Operation: v1.Precondition_OPERATION_MUST_NOT_MATCH, Filter: filter}
}

// pruneWholeScope deletes every relation on one resource object in a
// single broad DeleteRelationships call — no OptionalRelation, so it
// sweeps the sentinel along with everything else. Used both when
// FetchScope reports ErrScopeGone and by step 5's orphan reap.
func pruneWholeScope(ctx context.Context, w Writer, resourceType, resourceID string) error {
	_, err := w.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       resourceType,
			OptionalResourceId: resourceID,
		},
	})
	return err
}

// reapAbsentScopes prunes every resource object SpiceDB still holds, on a
// resource type present in known, whose id known does not list — the
// SCOPE-level reap, called ONCE per pass by Pass, unconditionally, against
// the enumeration-derived map (every currently enumerated scope's own
// (ResourceType, ID)). A whole-object sweep is the right granularity here:
// a scope object (a Slack channel) really is gone as a unit once it drops
// out of enumeration, the same way ErrScopeGone sweeps it wholesale mid-
// pass. The cross-resource case (Ruling 4's workspace-wide identity edges)
// is handled separately by reapAbsentCrossResourceTuples below, at TUPLE
// rather than resource-id granularity — see that function's doc for why a
// resource id surviving a pass does not mean every one of ITS OWN tuples
// did. A resource type absent from known entirely is never scanned:
// nothing this call's caller observed gives it a basis to judge that type
// at all.
func reapAbsentScopes(ctx context.Context, in PassInput, known map[string]map[string]struct{}, result *PassResult) {
	for resourceType, knownIDs := range known {
		present, err := scanResourceIDs(ctx, in.Reader, resourceType)
		if err != nil {
			result.ScopeErrors = append(result.ScopeErrors, ScopeError{
				Err: fmt.Errorf("relsync: reap scan of %q: %w", resourceType, err),
			})
			continue
		}
		for resourceID := range present {
			if _, ok := knownIDs[resourceID]; ok {
				continue
			}
			if err := pruneWholeScope(ctx, in.Writer, resourceType, resourceID); err != nil {
				result.ScopeErrors = append(result.ScopeErrors, ScopeError{
					Scope: ScopeID(resourceID),
					Err:   fmt.Errorf("relsync: reap %s:%s: %w", resourceType, resourceID, err),
				})
				continue
			}
			result.ReapedScopes++
		}
	}
}

// scanResourceIDs reads every tuple of one resource type (any resource id,
// any relation, any subject) and returns the distinct resource ids
// currently present — the set reapAbsentScopes diffs against the
// enumerated scopes of that type.
func scanResourceIDs(ctx context.Context, r Reader, resourceType string) (map[string]struct{}, error) {
	stream, err := r.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency:        fullyConsistent(),
		RelationshipFilter: &v1.RelationshipFilter{ResourceType: resourceType},
	})
	if err != nil {
		return nil, err
	}
	out := map[string]struct{}{}
	for {
		resp, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return out, rerr
		}
		out[resp.GetRelationship().GetResource().GetObjectId()] = struct{}{}
	}
	return out, nil
}

// reapAbsentCrossResourceTuples deletes every currently-held tuple, on a
// resource type present in union, whose (resource, relation, subject) key
// is absent from union — the CROSS-RESOURCE reap, called once per pass
// when fetchCoverage proves trustworthy (see Pass's own comment), at TUPLE
// granularity rather than resource-id granularity.
//
// This is the whole-branch review's Critical 2 fix. The old version of
// this reap (reapAbsentScopes, reused here before) judged a resource
// PRESENT and left it entirely alone the moment any tuple still named its
// id — so a changed SUBJECT (a Slack user's email changing between
// passes) was invisible to it: slack_user:U1 stayed "known" because SOME
// tuple on it was still asserted, and the STALE tuple (the old email)
// never got a second look. That is the fail-OPEN direction: the stale
// edge keeps granting whatever it always granted, forever, because
// nothing ever judged it absent.
//
// Diffing at tuple granularity subsumes the old id-level sweep rather
// than replacing it with a narrower behaviour: an id with NONE of its
// tuples surviving the union has every one of them fall out of this same
// diff, which is exactly the old whole-object-absent case, just reached
// by the general rule instead of a special one.
//
// Deletes across every resource type in union are batched into a single
// WriteRelationships call (DELETE-operation updates, no precondition —
// none of these types carries a #relhash sentinel to CAS against, per
// Ruling 4's "no synthetic scope invented to hang a hash on"), which is
// also why this goes through Writer.WriteRelationships rather than
// Writer.DeleteRelationships: a precise per-tuple delete needs the
// relation+subject a DeleteRelationships resource-id sweep does not name.
//
// # A sync may only DELETE what it may WRITE
//
// Only tuples on a relation the source itself CLAIMS are ever deleted, and
// the intuition that gets this backwards is worth stating outright,
// because it is the one that shipped: leaving a relation unclaimed does
// NOT protect it. relsource.CheckWrite refuses a relation claimed by
// ANOTHER source, so an unclaimed relation is the one every writer —
// including this reap — is permitted to touch. Unclaimed means unguarded,
// not reserved.
//
// The union is built purely from what this source's own fetches assert, so
// every relation the source does not write is absent from it BY
// CONSTRUCTION, never because upstream stopped saying so. Diffing against
// it without this bound therefore reads "a relation I know nothing about"
// as "a relation upstream retracted", and deletes it. The final
// whole-branch review found exactly that: github_repo_url carries the
// GitHub sync's #repo bridge edge alongside #owner (a human's approval,
// read by the approval gate and by a slot expression's owner leg) and
// slot_grant_<perm> (a live grant held by a running session). All three
// were swept on the first full-coverage pass. Slack was only ever spared
// because slack_user has one relation and one writer, so there was
// nothing else on the type to destroy — the defect was the engine's from
// the start and every kind is now bounded by its own claims.
//
// The bound narrows nothing for a relation the source DOES claim: those
// are diffed against the union exactly as before, which is what makes a
// stale bridge edge still reapable.
func reapAbsentCrossResourceTuples(ctx context.Context, in PassInput, union map[string]map[string]spicedb.Tuple, result *PassResult) {
	src := in.Kind.Source()
	var deletes []*v1.RelationshipUpdate
	for resourceType, stillAsserted := range union {
		current, err := scanResourceTypeTuples(ctx, in.Reader, resourceType)
		if err != nil {
			result.ScopeErrors = append(result.ScopeErrors, ScopeError{
				Err: fmt.Errorf("relsync: cross-resource reap scan of %q: %w", resourceType, err),
			})
			continue
		}
		for _, t := range current {
			if !relsource.Owns(src, t.ResourceType, t.Relation) {
				continue
			}
			if _, ok := stillAsserted[t.Key()]; ok {
				continue
			}
			deletes = append(deletes, deleteUpdate(t))
		}
	}
	if len(deletes) == 0 {
		return
	}
	if _, err := in.Writer.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{Updates: deletes}); err != nil {
		result.ScopeErrors = append(result.ScopeErrors, ScopeError{
			Err: fmt.Errorf("relsync: cross-resource reap write: %w", err),
		})
		return
	}
	result.Pruned += len(deletes)
}

// scanResourceTypeTuples reads every relationship SpiceDB currently holds
// on resourceType (any resource id, any relation, any subject) as full
// spicedb.Tuple values — scanResourceIDs' counterpart, needed because
// reapAbsentCrossResourceTuples diffs at tuple granularity and so must see
// the relation and subject, not merely which resource ids exist.
func scanResourceTypeTuples(ctx context.Context, r Reader, resourceType string) ([]spicedb.Tuple, error) {
	stream, err := r.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency:        fullyConsistent(),
		RelationshipFilter: &v1.RelationshipFilter{ResourceType: resourceType},
	})
	if err != nil {
		return nil, err
	}
	var out []spicedb.Tuple
	for {
		resp, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return out, rerr
		}
		rel := resp.GetRelationship()
		subj := rel.GetSubject()
		out = append(out, spicedb.Tuple{
			ResourceType:    rel.GetResource().GetObjectType(),
			ResourceID:      rel.GetResource().GetObjectId(),
			Relation:        rel.GetRelation(),
			SubjectType:     subj.GetObject().GetObjectType(),
			SubjectID:       subj.GetObject().GetObjectId(),
			SubjectRelation: subj.GetOptionalRelation(),
		})
	}
	return out, nil
}

// fullyConsistent is the consistency level for every relsync read — the
// same level (*spicedb.Client).ListDeniedUsers and the bootstrap drift
// detector's readManagedRelation use, and for the same reason: a sync pass
// racing a write it should have observed is worse than a slower pass.
func fullyConsistent() *v1.Consistency {
	return &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}}
}

// touchUpdate/deleteUpdate/relationshipFor/sentinelTouchUpdate convert
// between spicedb.Tuple and the v1 wire shape, mirroring
// pkg/authz/spicedb/bootstrap.go's TouchBootstrapRelationshipVia — SubjectRelation
// is only set on the wire when non-empty, matching that convention.

func touchUpdate(t spicedb.Tuple) *v1.RelationshipUpdate {
	return &v1.RelationshipUpdate{Operation: v1.RelationshipUpdate_OPERATION_TOUCH, Relationship: relationshipFor(t)}
}

func deleteUpdate(t spicedb.Tuple) *v1.RelationshipUpdate {
	return &v1.RelationshipUpdate{Operation: v1.RelationshipUpdate_OPERATION_DELETE, Relationship: relationshipFor(t)}
}

func relationshipFor(t spicedb.Tuple) *v1.Relationship {
	subj := &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: t.SubjectType, ObjectId: t.SubjectID}}
	if t.SubjectRelation != "" {
		subj.OptionalRelation = t.SubjectRelation
	}
	return &v1.Relationship{
		Resource: &v1.ObjectReference{ObjectType: t.ResourceType, ObjectId: t.ResourceID},
		Relation: t.Relation,
		Subject:  subj,
	}
}

// sentinelTuple builds the #relhash tuple for scope carrying hash as its
// subject id. Shared by sentinelTouchUpdate (the new hash) and
// sentinelDeleteUpdate (the old one, when it changed) so the two can never
// drift apart on relation/subject-type spelling.
func sentinelTuple(scope Scope, hash string) spicedb.Tuple {
	return spicedb.Tuple{
		ResourceType: scope.ResourceType,
		ResourceID:   string(scope.ID),
		Relation:     relhashRelation,
		SubjectType:  relhashSubjectType,
		SubjectID:    hash,
	}
}

func sentinelTouchUpdate(scope Scope, hash string) *v1.RelationshipUpdate {
	return touchUpdate(sentinelTuple(scope, hash))
}

// sentinelDeleteUpdate removes the OLD sentinel tuple — a hash rides as a
// subject id, so a changed hash is a changed (resource, relation, subject)
// triple entirely, and TOUCHing the new one never removes the old one.
// See diffScope's doc for why this is not optional.
func sentinelDeleteUpdate(scope Scope, hash string) *v1.RelationshipUpdate {
	return deleteUpdate(sentinelTuple(scope, hash))
}

// updateSortKey renders a RelationshipUpdate as a deterministic sort key,
// so the updates ride a WriteRelationshipsRequest in a stable order
// regardless of Go's randomized map iteration in diffScope above — the
// same reason bootstrap_drift.go sorts its report before logging it.
func updateSortKey(u *v1.RelationshipUpdate) string {
	rel := u.GetRelationship()
	subj := rel.GetSubject()
	return fmt.Sprintf("%d|%s:%s#%s@%s:%s#%s",
		u.GetOperation(),
		rel.GetResource().GetObjectType(), rel.GetResource().GetObjectId(), rel.GetRelation(),
		subj.GetObject().GetObjectType(), subj.GetObject().GetObjectId(), subj.GetOptionalRelation(),
	)
}
