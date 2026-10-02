package authz

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"

	"github.com/authzed/openagentprimitives/pkg/authz/precondition"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/envelopefact"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observedfact"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
)

const bindScopeMaxAttempts = 3

// slotCandidate is one (type, id) an instance could occupy a slot with, paired
// with the permission that decides whether it may.
type slotCandidate struct {
	ResourceType string
	Permission   string
	// ResourceID is the DERIVED object id — see authz.ObjectID.
	ResourceID ObjectID
	// RawID is the PRE-TRANSFORM value ResourceID was derived from — the
	// provider identifier an observation recorded, before ValueTransforms ran.
	//
	// Both halves are carried because they answer different questions and are
	// not interchangeable: ResourceID is what SpiceDB is asked about, RawID is
	// what a fact is keyed by. Looking a fact up by the derived form cannot
	// ever match (spicedb_escape exists because `#` is illegal in an object id)
	// and fails as an empty result, which reads as "nothing was observed" —
	// fail-closed, but permanently and invisibly. See precondition.LookupSubject.
	//
	// Empty for a candidate whose raw value was never carried — the WAIVED
	// approval path (an approval that consents to the gate) and every other
	// caller with no precondition to evaluate. The ENFORCING approval path
	// (plan-gate) DOES carry it, reconstructed from SlotBinding.RawID, because
	// re-checking its precondition reads facts keyed by the raw form.
	RawID string
	// Requires are the declared preconditions — predicate plus the two messages
	// its author wrote — that must all be Satisfied before this candidate may
	// bind, copied from its BoundEntitySpec.
	Requires []precondition.Rule
	// NoGrantRelation mirrors SlotBinding.NoGrantRelation: the type has no
	// slot_grant relation to write, so this candidate narrows scope only.
	NoGrantRelation bool
	// Occupancy and Rebind mirror SlotBinding's fields, copied from the
	// candidate's BoundEntitySpec so bindSlots can stamp them onto the
	// SlotBinding the GrantSlots pinning gate reads. Empty Occupancy is single.
	Occupancy string
	Rebind    string

	// PriorID mirrors SlotBinding.PriorID: when non-zero, this candidate MOVES a
	// filled single-occupancy slot from PriorID to ResourceID. Carried through
	// checkPreconditions so the move executes only for a candidate that actually
	// survived the gate — a held instance must not have the pin moved to it, and
	// the prior instance's grants must not be revoked, for a bind that never
	// happens.
	PriorID ObjectID
}

// admissibleCandidates is the one filter chain every fill source runs its
// candidates through before bindSlots writes anything.
//
// It exists as a single function rather than as two calls repeated at each fill
// source for the reason this package keeps re-learning: three hand-maintained
// copies of a filter chain is how one source silently stops applying a gate the
// others do, and nothing goes red. bindSlots is shared for exactly this reason;
// so is this.
//
// # Preconditions run FIRST, and the order is deliberate
//
// A candidate the requester cannot reach is dropped by checkBindable SILENTLY,
// because at that point nobody has asked for it. A precondition-held candidate
// must be RECORDED instead, because someone WILL ask and the recorded reason is
// what makes the eventual denial explicable — the design says exactly this, and
// it is the one way the two filters differ.
//
// Running checkBindable first would therefore lose the precondition line for
// any candidate that is both unreachable and gate-held: an operator asking "why
// is this slot empty" would see the Check denial and nothing about the gate,
// which is the harder half to reconstruct. Preconditions first means every
// proposed candidate's verdict reaches the log exactly once.
//
// Do not reverse this to save a fact read. A candidate with no `requires` costs
// no read at all (checkPreconditions returns it untouched), so the only reads
// this ordering can add are for candidates a gate was declared over — which is
// the case the reads exist to decide.
func admissibleCandidates(
	ctx context.Context,
	mem memory.Memory,
	memScope memory.Scope,
	chk Checker,
	subject, source string,
	logger logr.Logger,
	cands []slotCandidate,
) []slotCandidate {
	return checkBindable(ctx, chk, subject, source, logger,
		checkPreconditions(ctx, mem, memScope, source, logger, cands))
}

// checkPreconditions returns the candidates whose declared preconditions are
// all Satisfied against the facts recorded so far.
//
// This is where the gate holds. A slot whose precondition is Refused or
// Undetermined simply has no instance in it, so every permissioned call against
// that type fails closed through the ordinary authorization path with the
// ordinary audit record — no new dispatch-time enforcement point, and no way
// for a model to decline the rule.
//
// # It stores NOTHING, on purpose
//
// A verdict is a pure function of durable, write-once facts and a declared
// expression, so dispatch recomputes it rather than reading a cache. Recording
// it would create a second source of truth that can disagree with the first,
// and the disagreement would be invisible: the stored verdict would be what
// explains the denial while the live one is what caused it.
//
// # Undetermined never binds, and that is what makes the gate monotonic
//
// Because facts are write-once, a verdict can move undetermined→satisfied or
// undetermined→refused, never satisfied→refused. So a bound slot cannot be
// invalidated by a later fact — no revocation path is needed — and the gate is
// order-independent: "call the tool early and you are gated, late and you are
// not" is a real bug class in dispatch-time gates, and holding on undetermined
// rules it out structurally rather than by discipline.
//
// Every hold is logged at INFO with the verdict, the slot type and id, and —
// when undetermined — the references that are missing. Without that line "the
// slot is empty" is unexplainable from logs, which is the failure mode this
// whole subsystem is built to avoid.
func checkPreconditions(
	ctx context.Context,
	mem memory.Memory,
	memScope memory.Scope,
	source string,
	logger logr.Logger,
	cands []slotCandidate,
) []slotCandidate {
	out := make([]slotCandidate, 0, len(cands))
	for _, c := range cands {
		if len(c.Requires) == 0 {
			// The overwhelmingly common case, and the regression guard for
			// every class that predates preconditions: a slot declaring none
			// binds exactly as it did before, and costs no fact read to do it.
			out = append(out, c)
			continue
		}
		if mem == nil {
			// Unreachable from the three fill sources today — each returns early
			// on a nil Memory before building a candidate — so this is insurance,
			// not a live path. It is spelled out anyway because the alternative
			// to an explicit hold is a nil-map read that reports every fact
			// absent, i.e. a silent Undetermined: fail-closed, but
			// indistinguishable from an observation that has not happened yet.
			// "Could not look" must never be answered as a verdict.
			logger.Info("slot candidate held: its precondition could not be evaluated because no memory store is wired",
				"source", source, "resourceType", c.ResourceType, "rawResourceID", c.RawID,
				"resourceID", c.ResourceID.String(), "preconditions", len(c.Requires))
			continue
		}
		subj, facts, err := candidateFacts(ctx, mem, memScope, c.ResourceType, c.RawID)
		if err != nil {
			// A read failure is not "no facts": treating it as one would report
			// undetermined, which is a legitimate verdict and so would hide a
			// broken store behind a gate that looks like it is working.
			logger.Info("slot candidate held: its facts could not be read, so no precondition could be decided",
				"source", source, "resourceType", c.ResourceType, "rawResourceID", c.RawID,
				"resourceID", c.ResourceID.String(), "err", err.Error())
			continue
		}
		if preconditionsSatisfied(c, subj, facts, source, logger) {
			out = append(out, c)
		}
	}
	return out
}

// preconditionsSatisfied reports whether EVERY precondition on the candidate is
// Satisfied, logging the first one that is not.
//
// It stops at the first non-Satisfied verdict because one hold is a hold: the
// slot does not bind, and continuing would emit a second line about a predicate
// that changes nothing. The one that stopped it is the one an operator needs.
//
// `subj` is the SAME FactSubject the facts were read with, threaded in rather
// than re-derived from the candidate. CEL's `slot.resourceID` and the fact
// lookup key must be the same value: an author's `facts.observed.repo ==
// slot.resourceID` compares a recorded fact against it, and a fact is recorded
// under the lookup key. Deriving it twice — once in candidateFacts, once here —
// is correct only while LookupSubject is the identity function, and the day it
// normalizes anything the two would diverge SILENTLY, which is the exact
// failure RULING P-2 exists to prevent. One derivation, two consumers.
func preconditionsSatisfied(c slotCandidate, subj precondition.FactSubject, facts precondition.Facts, source string, logger logr.Logger) bool {
	// The SAME iteration the dispatch-time explanation runs. Sharing it is what
	// keeps "which rule held this candidate" and "which rule the agent is told
	// about" the same answer — see precondition.FirstUnsatisfied.
	u, held, err := precondition.FirstUnsatisfied(c.Requires, facts, subj.ResourceType, subj.ResourceID)
	if err != nil {
		// An eval error is a BROKEN EXPRESSION, never a verdict. Admission
		// compiles every predicate, so reaching here means the class was
		// written past validation or the expression fails only at runtime
		// (a `slot` field typo type-checks as dyn and dies here). Holding
		// the candidate is right; holding it silently would leave an author
		// hunting for a missing observation instead of a broken rule.
		logger.Info("slot candidate held: its precondition could not be evaluated",
			"source", source, "resourceType", c.ResourceType, "rawResourceID", subj.ResourceID,
			"resourceID", c.ResourceID.String(), "err", err.Error())
		return false
	}
	if !held {
		return true
	}
	// The expression text is logged because it is the rule an operator has
	// to go read; the fact VALUES deliberately are not, since they come out
	// of a signed payload or a tool result and can carry anything upstream
	// returned.
	//
	// BOTH ids are logged, under distinct keys. checkBindable's own drop
	// line carries the DERIVED id under `resourceID`, and for any slot
	// declaring spicedb_escape the two forms differ — so reusing that key
	// for the raw value would put two adjacent lines about the same
	// candidate into apparent disagreement, in front of the one reader who
	// is here precisely because a slot is empty and they do not know why.
	logger.Info("slot candidate held by a precondition; the slot stays empty and every permissioned call against it fails closed",
		"source", source, "resourceType", c.ResourceType,
		"rawResourceID", subj.ResourceID, "resourceID", c.ResourceID.String(),
		// Compiled is non-nil here by construction: Evaluate refuses a nil
		// program with an ERROR, which the arm above already returned on.
		"verdict", u.Verdict.String(), "expression", u.Rule.Compiled.Expression(),
		"missingFacts", factRefStrings(u.Missing))
	return false
}

// factRefStrings renders the missing references for a log line. Non-empty
// exactly when the verdict is Undetermined, which is what tells an operator the
// gate is answerable — by an observation, not by a person.
func factRefStrings(refs []precondition.FactRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Provenance+"."+r.Name)
	}
	return out
}

// candidateFacts reads everything both fact Kinds have recorded about one
// candidate instance.
//
// The two Kinds are read through their OWN wrappers rather than through
// factcontent's generic form, because the generic form takes the (kind name, id
// prefix) pair by hand and observedfact's pair is a perfectly valid thing to
// pass to an envelope read: the mistake is silently accepted and collapses the
// trust-grade boundary the two-Kind split exists to keep apart.
//
// Both reads must succeed or the candidate is held. A partial answer would
// under-report facts, and an under-reported fact reads as undetermined — which
// looks exactly like an observation that has not happened yet, so a broken read
// would present as a gate quietly doing its job.
//
// It returns the FactSubject it read with, not just the facts, so that the
// caller binds CEL's `slot` from the same value rather than re-deriving it. That
// is the whole point of RULING P-2's single helper: one derivation with two
// consumers cannot drift, one helper plus an open-coded copy can.
// It takes the (resourceType, rawID) pair rather than a slotCandidate because
// dispatch has no candidate to hand it — the candidate is precisely what the
// precondition held out of the slot — and reads the facts of the instance a
// denied CALL named. One reader, so the bind-time filter and the dispatch-time
// explanation cannot come to read different Kinds, or one of the two.
func candidateFacts(ctx context.Context, mem memory.Memory, memScope memory.Scope, resourceType, rawID string) (precondition.FactSubject, precondition.Facts, error) {
	// One derivation, shared with the dispatch-time recomputation, so the two
	// sides cannot disagree about which instance they are deciding.
	subj, err := precondition.LookupSubject(resourceType, rawID)
	if err != nil {
		return precondition.FactSubject{}, precondition.Facts{}, err
	}
	env, err := envelopefact.ForSubject(ctx, mem, memScope, subj.ResourceType, subj.ResourceID)
	if err != nil {
		return precondition.FactSubject{}, precondition.Facts{}, fmt.Errorf("read envelope facts for %s:%s: %w", subj.ResourceType, subj.ResourceID, err)
	}
	obs, err := observedfact.ForSubject(ctx, mem, memScope, subj.ResourceType, subj.ResourceID)
	if err != nil {
		return precondition.FactSubject{}, precondition.Facts{}, fmt.Errorf("read observed facts for %s:%s: %w", subj.ResourceType, subj.ResourceID, err)
	}
	// Both provenances are read even when the predicate names only one. Deriving
	// which to skip is a second place the reference set could be got wrong, and
	// getting it wrong there is silent: an unread provenance is an absent map,
	// which holds the slot undetermined forever with nothing reporting a fault.
	// Two reads per GATED candidate — a candidate with no requires never gets
	// here — is not worth that.
	return subj, precondition.Facts{Envelope: env, Observed: obs}, nil
}

// checkBindable returns the candidates the subject may actually reach.
//
// The Check is what keeps every fill source honest: an instance binds only when
// the REQUESTER already has standing on it, so binding is never an escalation —
// it is the session catching up to authority the user already held. A candidate
// the subject cannot reach is dropped, not escalated to an approval, because at
// this point nobody has asked for it yet.
func checkBindable(ctx context.Context, chk Checker, subject, source string, logger logr.Logger, cands []slotCandidate) []slotCandidate {
	out := make([]slotCandidate, 0, len(cands))
	for _, c := range cands {
		perm := Permission{
			StateImpact: Readonly,
			Check: &PermissionCheck{
				ResourceType:       c.ResourceType,
				Permission:         c.Permission,
				ResourceIDTemplate: "{__resourceID__}",
			},
		}
		args := map[string]any{"__resourceID__": c.ResourceID.String()}
		res := chk.Check(ctx, perm, Inputs{Args: args, Subject: subject})
		if res.Outcome != OutcomeAllowed {
			logger.Info("slot binding dropped (Check denied)",
				"source", source,
				"resourceType", c.ResourceType,
				"resourceID", c.ResourceID.String(),
				"subject", subject,
				"msg", res.Message,
			)
			continue
		}
		out = append(out, c)
	}
	return out
}

// bindSlots writes the two halves of a binding: the Layer-2 ScopeResource that
// narrows, and the slot grant that authorizes.
//
// Shared by every fill source, because the ORDER and the failure handling are
// the part that is easy to get subtly wrong, and getting it wrong in one source
// but not another would be invisible. Scope goes first and unconditionally: a
// grant write fails against any resource whose definition lacks the slot_grant
// relation (a type no fragment declares, so the composer skipped it) or when
// SpiceDB is unreachable, and CheckScopeWithRefs (not wired at dispatch
// today) reads a type that is ABSENT from scope.Resources as "scope does
// not narrow this type" — so aborting on a grant failure would WIDEN Layer-2
// rather than fail closed. The grant error still propagates.
//
// A candidate marked NoGrantRelation is partitioned out of the grant write but
// kept in the scope write. That is the case where the type is not a declared
// slot at all, so the relation the grant needs was never composed: attempting
// it can only fail, and propagating that failure would abandon an approval a
// human already gave.
//
// Writing the ScopeResource is not optional for a mid-session source either: a
// class with defaults already puts its type into scope.Resources, so a grant
// added without the matching ScopeResource would be refused by the scope layer
// and the binding would do nothing.
func bindSlots(
	ctx context.Context,
	mem memory.Memory,
	memScope memory.Scope,
	sess SessionRef,
	w RelWriter,
	src scope.Source,
	logger logr.Logger,
	now func() time.Time,
	expiresAt time.Time,
	bound []slotCandidate,
) error {
	if len(bound) == 0 {
		return nil
	}
	refs := make([]scope.ResourceRef, 0, len(bound))
	bindings := make([]SlotBinding, 0, len(bound))
	for _, c := range bound {
		// Scope records EVERY binding, grantable or not: absence of a type from
		// scope.Resources reads as "scope does not narrow this type", so
		// omitting an approved instance would be the wide answer, not the safe
		// one.
		refs = append(refs, scope.ResourceRef{ResourceType: c.ResourceType, ID: c.ResourceID.String()})
		if c.NoGrantRelation {
			continue
		}
		bindings = append(bindings, SlotBinding{
			ResourceType: c.ResourceType,
			ResourceID:   c.ResourceID,
			Permission:   c.Permission,
			Occupancy:    c.Occupancy,
			Rebind:       c.Rebind,
		})
	}

	var scopeErr error
	for attempt := 1; attempt <= bindScopeMaxAttempts; attempt++ {
		cur, _, err := sessionscope.Get(ctx, mem, memScope)
		if err != nil {
			return fmt.Errorf("bindSlots(%s): read session_scope: %w", src, err)
		}
		next := scope.ApplyDelta(cur, scope.ScopeDelta{Add: scope.ScopePartial{Resources: refs}}, src, now())
		// Compare-and-swap on the version read above. authzd read-modify-writes
		// this same document when a human decides a scope change, and this write
		// runs on every dispatch round once any observed fact exists — so a blind
		// overwrite here can silently discard a HardDeny the owner just clicked,
		// with no error and with the scope_audit record still saying it applied.
		scopeErr = sessionscope.PutIfVersion(ctx, mem, memScope, next, cur.ScopeVersion)
		if scopeErr == nil {
			break
		}
		if !errors.Is(scopeErr, sessionscope.ErrVersionConflict) {
			return fmt.Errorf("bindSlots(%s): write session_scope: %w", src, scopeErr)
		}
		// A version conflict means another writer won, not that the write is
		// invalid. Re-read and recompute so its narrowing (especially HardDeny)
		// is preserved before this binding can create a relationship grant.
	}
	if scopeErr != nil {
		return fmt.Errorf("bindSlots(%s): write session_scope after %d attempts: %w",
			src, bindScopeMaxAttempts, scopeErr)
	}

	if w == nil {
		logger.Info("no relationship writer wired; bindings narrow scope but grant nothing",
			"source", src, "session", sess.String(), "bindings", len(bindings))
		return nil
	}
	if err := GrantSlots(ctx, w, sess, bindings, expiresAt); err != nil {
		logger.Info("bindings did not become slot grants; the instance axis is inert for these",
			"source", src, "session", sess.String(), "bindings", len(bindings), "err", err.Error())
		return fmt.Errorf("bindSlots(%s): grant slots: %w", src, err)
	}
	return nil
}

// PreconditionPolicy states whether BindApproved must re-evaluate a binding's
// slot preconditions before writing its grant, or treat them as already waived.
//
// It is a named type with two constants, not a bare bool, for one reason: the
// dangerous answer must be impossible to give by accident. The zero value is
// EnforcePreconditions — a caller that forgets the argument, or a future one
// that copies a call site without thinking, re-checks the gate and fails
// closed. Waiving is the exceptional, load-bearing choice, so it has to be
// SPELLED (PreconditionsWaived) at the one call site allowed to make it, where
// a reviewer will see it. Compare a bool: `true`/`false` at a call carries no
// hint of which is safe, and the silent-waiver this whole task closes is
// exactly what a mis-set bool would reopen.
type PreconditionPolicy bool

const (
	// EnforcePreconditions makes BindApproved run checkPreconditions over the
	// bindings, so an instance whose declared precondition is Refused or
	// Undetermined is DROPPED rather than bound. The plan-gate path uses this:
	// approving a plan phase consents to the phase, not to the risk a
	// precondition guards, so a gated instance stays unbound and escalates to
	// the human waiver card at its next tool call. The zero value, so it is the
	// fail-closed default for any caller that does not say otherwise.
	EnforcePreconditions PreconditionPolicy = false
	// PreconditionsWaived makes BindApproved skip the precondition check
	// entirely and bind every proposed instance. The human WAIVER path uses
	// this and ONLY this: approving that card IS the consent to a Refused
	// verdict, so re-applying the gate would make the consent unusable — the
	// card appears, the human approves, and nothing binds.
	PreconditionsWaived PreconditionPolicy = true
)

// BindApproved records a binding a human cleared: it writes an
// INSTANCE-SCOPED SpiceDB slot grant for the approved (resourceType,
// resourceID, permission) triple, and records a matching scope.Resources
// entry in session_scope (Layer 2) alongside it.
//
// The exported door onto bindSlots, and the reason it exists is a gap rather
// than a convenience. Before this existed, the two APPROVAL paths (plan-gate,
// JIT tool approval) either wrote no SpiceDB grant at all or called
// GrantSlots directly without ever touching session_scope; the two fill
// sources that go through bindSlots (class defaults, promoted extractions)
// already did both. So an approval could leave a permissioned call
// authorization-inert (plan-gate, pre-this-slice) or grant one instance
// without any record of having excluded another (JIT). What THIS function
// makes true today: the grant is scoped to the specific instance a human
// was shown, so calling the same permission against a DIFFERENT instance of
// the same type is refused for want of a grant on that instance — not
// because a second, independently-enforced scope check ran.
//
// The session_scope write is NOT an independent enforcement layer yet.
// scope.CheckScopeWithRefs — the function that would check a resolved
// resource ref against scope.Resources — has no caller in either live
// dispatch path (pkg/authz/check_tool_call.go's `check`, pkg/authz/hooks/
// scope.go's `Scope.Eval`); both call the ref-less scope.CheckScope instead.
// So the scope.Resources entry this call adds is recorded for a future
// enforcement wiring, not read at dispatch today. Do not read "narrows the
// session's scope" (used loosely elsewhere in this file, predating this
// audit) as a claim that session_scope alone excludes anything right now —
// the exclusion in force is entirely the grant's own instance-scoping.
//
// Ordering, failure handling and the "scope first, unconditionally" rule are
// bindSlots'; this only supplies the source. Do not reimplement them here — a
// second copy that drifts is invisible until an approval silently widens.
func BindApproved(
	ctx context.Context,
	mem memory.Memory,
	memScope memory.Scope,
	sess SessionRef,
	w RelWriter,
	bindings []SlotBinding,
	policy PreconditionPolicy,
	expiresAt time.Time,
	logger logr.Logger,
	now func() time.Time,
) error {
	if len(bindings) == 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	// checkBindable never runs here whatever the policy: a human with standing
	// has already said yes, so re-Checking would drop instances an approval
	// explicitly granted.
	//
	// checkPreconditions is what `policy` selects, and the two callers it
	// distinguishes have OPPOSITE needs. The human WAIVER path passes
	// PreconditionsWaived: approving that card IS the consent to a Refused
	// verdict, so re-applying the gate would make the consent unusable. The PLAN
	// GATE passes EnforcePreconditions: it binds an approved phase's slot refs
	// through here, before the tool-call hook that would have explained a
	// precondition ever runs, and a plan-phase approval consents to the PHASE,
	// not to the risk a precondition guards. So a gated instance the phase names
	// must NOT bind silently — it is dropped, stays unbound, and escalates to
	// the waiver card at its next tool call, which is the one place risk consent
	// is asked for. Phase-approval and risk-waiver stay separate consents.
	//
	// The plan-gate route that used to bind such an instance unconditionally
	// is now CLOSED: it carries each binding's RawID and compiled Requires,
	// this runs the same evaluator every fill source runs, and
	// Refused/Undetermined hold the instance out exactly
	// as they do at bind time everywhere else. A binding with no Requires — the
	// waiver's, and any type declaring no precondition — passes through
	// untouched, so the enforcing default costs nothing for the common case.
	bound := make([]slotCandidate, 0, len(bindings))
	for _, b := range bindings {
		bound = append(bound, slotCandidate{
			ResourceType:    b.ResourceType,
			Permission:      b.Permission,
			ResourceID:      b.ResourceID,
			RawID:           b.RawID,
			Requires:        b.Requires,
			NoGrantRelation: b.NoGrantRelation,
			Occupancy:       b.Occupancy,
			Rebind:          b.Rebind,
			PriorID:         b.PriorID,
		})
	}
	if policy == EnforcePreconditions {
		bound = checkPreconditions(ctx, mem, memScope, string(scope.SourceApproved), logger, bound)
		if len(bound) == 0 {
			return nil
		}
	}
	// The approved MOVE runs BEFORE bindSlots, over the candidates that survived
	// the precondition gate: it repoints each filled single-occupancy slot off
	// its old instance and revokes that instance's grants, so the subsequent
	// GrantSlots finds the pin already on the new instance (a same-instance
	// bind) rather than a different one (which it would refuse). A move failure
	// propagates — the human's approval produced nothing, and bindSlots must not
	// then write the new grant.
	if err := moveApprovedPins(ctx, w, sess, bound, logger); err != nil {
		return err
	}
	return bindSlots(ctx, mem, memScope, sess, w, scope.SourceApproved, logger, now, expiresAt, bound)
}

// moveApprovedPins executes the human-approved pin MOVE for every
// single-occupancy candidate that carries a PriorID — the only way a filled
// single-occupancy slot legitimately changes instance.
//
// One MovePin per moving type: it repoints the pin fromID (PriorID) -> toID
// (ResourceID) and revokes the prior instance's grants in the SAME request, so
// the slot is never momentarily held by two instances and the displaced
// instance keeps no residual authority. The revoke set is the prior instance's
// ACTUAL grants, read back via ListGrantsFor — not rebuilt from the moving
// approval's permission list, which would leave behind any grant the new
// approval does not re-name (the asymmetric-permission leak).
//
// Nil writer: today's no-op. A nil binder grants nothing and moves nothing; the
// later bindSlots nil-writer branch logs that the approval narrowed scope with
// no grant written. Fail closed ONLY on a non-nil writer that cannot express the
// pin, and ONLY when a move is actually needed: a writer that is not a
// SlotPinner cannot repoint a pin, so routing the move through it would silently
// drop the single-occupancy guarantee — refuse instead.
//
// # The ListGrantsFor → MovePin window is inherent and bounded
//
// Each move reads the displaced instance's current grants (ListGrantsFor) and
// then revokes exactly those in the MovePin request. A grant written to the
// displaced instance BETWEEN that read and that write survives the move — the
// revoke set was computed before it existed. This is inherent to a non-atomic
// read-then-revoke and is not closed here: the only writers of a displaced
// instance's grants are this session's own bind paths, a straggler is rare, and
// every slot grant carries a mandatory expiry (SlotGrantExpiry), so a survivor
// lapses rather than lingering forever. The pin itself moves atomically (the
// MUST_MATCH guards that), so the single-occupancy identity is never in doubt;
// only a stray expiring grant on the old instance can briefly outlive the move.
func moveApprovedPins(ctx context.Context, w RelWriter, sess SessionRef, bound []slotCandidate, logger logr.Logger) error {
	type pinMove struct {
		fromID string
		toID   ObjectID
	}
	moves := make(map[string]pinMove)
	order := make([]string, 0)
	for _, c := range bound {
		if c.PriorID.IsZero() || occupancyOf(SlotBinding{Occupancy: c.Occupancy}) == SlotOccupancyMulti {
			// No move: an empty PriorID is a first-fill, and a multi-occupancy
			// slot holds no pin to move (PriorID is only ever set for single).
			continue
		}
		if c.PriorID.String() == c.ResourceID.String() {
			// fromID == toID is not a move: the pin already names this instance.
			// Executing a MovePin would DELETE then re-TOUCH the same pin and
			// revoke the instance's own grants for nothing — the subsequent
			// same-instance GrantSlots re-binds it. Skip it.
			continue
		}
		if existing, ok := moves[c.ResourceType]; ok {
			if existing.toID.String() != c.ResourceID.String() {
				// Two DISTINCT move targets for one single-occupancy type in one
				// approval cannot both occupy the slot. Refuse the WHOLE move set
				// BEFORE any MovePin runs — executing even the first would commit
				// the slot to whichever target happened to be ordered first, an
				// instance the approver was not necessarily shown as THE move.
				// errors.Is(…, ErrSlotPinned) so the plan-gate failure path treats
				// it as a pin refusal, not a store error.
				return fmt.Errorf("%w: slot type %s has two distinct move targets in one approval (%s and %s); it is single-occupancy",
					ErrSlotPinned, c.ResourceType, existing.toID.String(), c.ResourceID.String())
			}
			// Same target named twice (a second permission on the moving
			// instance) — one move covers both.
			continue
		}
		moves[c.ResourceType] = pinMove{fromID: c.PriorID.String(), toID: c.ResourceID}
		order = append(order, c.ResourceType)
	}
	if len(moves) == 0 {
		return nil
	}
	if w == nil {
		return nil
	}
	pinner, ok := w.(SlotPinner)
	if !ok {
		return fmt.Errorf("authz: moving a filled single-occupancy slot needs a SlotPinner-capable writer, got %T", w)
	}
	for _, rt := range order {
		mv := moves[rt]
		revoke, err := pinner.ListGrantsFor(ctx, rt, mv.fromID, sess)
		if err != nil {
			return fmt.Errorf("authz: list prior grants for %s:%s to revoke on move: %w", rt, mv.fromID, err)
		}
		logger.Info("approved slot-pin move: repointing a filled single-occupancy slot and revoking the prior instance's grants",
			"session", sess.String(), "slotType", rt, "from", mv.fromID, "to", mv.toID.String(), "revokeGrants", len(revoke))
		if err := pinner.MovePin(ctx, rt, mv.fromID, mv.toID.String(), revoke, sess); err != nil {
			return fmt.Errorf("authz: execute approved slot move for %s: %w", rt, err)
		}
	}
	return nil
}
