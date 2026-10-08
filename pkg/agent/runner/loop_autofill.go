package runner

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/authz/slotspec"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// bindingScope returns the memory scope for this session.
func (l *Loop) bindingScope() memory.Scope {
	return memory.Scope{Kind: "session", ID: l.SessionKey.Namespace + "/" + l.SessionKey.Name}
}

// authzSessionRef returns the (namespace, name) coordinate SpiceDB-side authz
// is keyed on. Distinct from bindingScope: slot grants live in SpiceDB against
// the session, not in a memory document.
func (l *Loop) authzSessionRef() authz.SessionRef {
	return authz.SessionRef{Namespace: l.SessionKey.Namespace, Name: l.SessionKey.Name}
}

// SlotTransformsOf reads the per-type transform chains the AgentClass
// controller published on status.resolvedSlots[].valueTransforms, keyed by
// resourceType.
//
// Exported and called from three places — this package's own
// boundEntitySpecsForAutofill, internal/cmd/runner/main.go, and the in-process
// e2e harness (test/e2e/inprocess_runner_factory.go) — precisely so there is
// only ONE place that does this derivation. The three used to each hand-roll
// the same loop; the e2e copy's own comment named the exact failure it did not
// prevent — a nil or drifted map there would make every e2e bundle bind raw,
// untransformed values while production derives them correctly, and nothing
// would fail except review. Both other call sites already import this
// package, so this is the natural shared home: pkg/authz cannot host it
// (cannot import v1alpha1 — see boundEntitySpecsForAutofill's own doc).
func SlotTransformsOf(class *spiceboxv1alpha1.AgentClass) map[string][]string {
	out := make(map[string][]string, len(class.Status.ResolvedSlots))
	for _, rs := range class.Status.ResolvedSlots {
		if len(rs.ValueTransforms) > 0 {
			out[rs.ResourceType] = rs.ValueTransforms
		}
	}
	return out
}

// SlotStandingOf reads the per-type standing the AgentClass controller
// published on status.resolvedSlots[].standing, keyed by resourceType.
//
// A type with no declared standing (Standing == "") is deliberately left
// OUT of the map rather than entered with an empty string: approverCanDelegateSlots
// treats absence and StandingSessionOnly identically (anything that is not
// StandingRequired bypasses the lookup), so there is no behavior difference —
// but this mirrors SlotTransformsOf's presence-means-something convention and
// keeps the map's size proportional to what the class actually declared.
//
// Exported for the same reason SlotTransformsOf is: it is called from both
// Loop-construction sites (internal/cmd/runner/main.go,
// test/e2e/inprocess_runner_factory.go) and this is the one place either
// computes it, so there is nothing left to hand-roll and drift.
func SlotStandingOf(class *spiceboxv1alpha1.AgentClass) map[string]string {
	out := make(map[string]string, len(class.Status.ResolvedSlots))
	for _, rs := range class.Status.ResolvedSlots {
		if rs.Standing != "" {
			out[rs.ResourceType] = rs.Standing
		}
	}
	return out
}

// PermissionTitlesOf reads the declared permission titles the AgentClass
// controller published, keyed "<resourceType>/<permission>".
//
// Exported for the same reason SlotTransformsOf and SlotStandingOf are: it is
// called from both Loop-construction sites (internal/cmd/runner/main.go,
// test/e2e/inprocess_runner_factory.go) and this is the one place either
// computes it. A private copy per call site — or worse, a nil map at one of
// them — is exactly how a declared title silently stops reaching the card:
// plangate.CardInput.PermissionTitles would always miss, every line would
// fall back to the detokenized handle, and the feature would look like it
// works while doing nothing.
func PermissionTitlesOf(class *spiceboxv1alpha1.AgentClass) map[string]string {
	out := make(map[string]string, len(class.Status.ResolvedPermissionTitles))
	for _, rt := range class.Status.ResolvedPermissionTitles {
		if rt.Title != "" {
			out[rt.ResourceType+"/"+rt.Permission] = rt.Title
		}
	}
	return out
}

// ResourceDisplaysOf reads the declared resource-type presentations the
// AgentClass controller published, keyed by resourceType. The display
// sibling of PermissionTitlesOf, for the same reason: exported and called
// from both Loop-construction sites (internal/cmd/runner/main.go,
// test/e2e/inprocess_runner_factory.go) so there is only ONE place that
// converts the status field into the shape plangate.CardInput consumes. A
// private copy per call site — or worse, a nil map at one of them — is
// exactly how a declared display silently stops reaching the card:
// plangate.CardInput.ResourceDisplays would always miss, every resource line
// would fall back to the wire type name, and the feature would look like it
// works while doing nothing.
//
// Returns plangate.ResourceDisplay rather than the v1alpha1 status type
// directly: pkg/authz/hooks (the gate's own deps struct) deliberately does
// not import pkg/apis/v1alpha1, so this is the one conversion boundary
// between the CRD status shape and the plain type the gate consumes.
func ResourceDisplaysOf(class *spiceboxv1alpha1.AgentClass) map[string]plangate.ResourceDisplay {
	out := make(map[string]plangate.ResourceDisplay, len(class.Status.ResolvedResourceDisplays))
	for _, rd := range class.Status.ResolvedResourceDisplays {
		out[rd.ResourceType] = plangate.ResourceDisplay{Name: rd.Name, Icon: rd.Icon, Label: rd.Label}
	}
	return out
}

// boundEntitySpecsForAutofill adapts AgentClass.Spec.Authz.Slots to the
// local-shape []authz.BoundEntitySpec that pkg/authz consumes (pkg/authz
// can't import pkg/apis/v1alpha1 due to cycle).
//
// The conversion itself lives in pkg/authz/slotspec, which is the only place a
// BoundEntitySpec is built: a field forgotten in one of several hand-written
// copies is how a slot's precondition set silently fails to reach the gate.
// This wrapper supplies the transform chains and nothing else.
//
// Returns an error only when a slot's `requires[]` predicate will not compile —
// a class the AgentClass reconciler should already have refused. Callers bind
// nothing in that case.
func boundEntitySpecsForAutofill(class *spiceboxv1alpha1.AgentClass) ([]authz.BoundEntitySpec, error) {
	if class == nil || len(class.Spec.GetSlots()) == 0 {
		return nil, nil
	}
	return slotspec.FromClass(class, SlotTransformsOf(class))
}

// autofillTypesForTool returns the entity types whose AutoFillArgs match toolName.
// Empty result means "no autofill needed for this tool"; the wait site skips.
func autofillTypesForTool(cls *spiceboxv1alpha1.AgentClass, toolName string) []string {
	if cls == nil {
		return nil
	}
	var out []string
	for _, et := range cls.Spec.GetSlots() {
		for _, af := range et.AutoFillArgs {
			if af.ToolNamePattern == "" {
				out = append(out, et.ResourceType)
				break
			}
			if ok, err := filepath.Match(af.ToolNamePattern, toolName); err == nil && ok {
				out = append(out, et.ResourceType)
				break
			}
		}
	}
	return out
}

// ResourceStanding is how approval for one resource type is governed, as the
// runner consumes it.
type ResourceStanding struct {
	// Standing is StandingRequired or StandingSessionOnly. Never empty for a
	// type present in the map — the controller refuses to publish a blank one.
	Standing string
	// ApproverPermission is the permission an approver must hold, set only when
	// Standing is StandingRequired.
	ApproverPermission string
}

// ResourceStandingsOf reads the per-resource-type approval governance the
// AgentClass controller published.
//
// Exported for the same reason SlotTransformsOf and PermissionTitlesOf are: it
// is called from both Loop-construction sites and this is the one place either
// computes it. A type ABSENT from this map has no published answer, and the
// approval router must refuse rather than guess — which is why this returns the
// map as-is with no fallback entry synthesized for anything.
func ResourceStandingsOf(class *spiceboxv1alpha1.AgentClass) map[string]ResourceStanding {
	out := make(map[string]ResourceStanding, len(class.Status.ResolvedResourceStandings))
	for _, rs := range class.Status.ResolvedResourceStandings {
		out[rs.ResourceType] = ResourceStanding{
			Standing:           rs.Standing,
			ApproverPermission: rs.ApproverPermission,
		}
	}
	return out
}

// approverSetFor returns the SpiceDB subject-set an approval for this resource
// instance must route to, and whether one exists at all.
//
// Three outcomes, and the difference between them is the whole point of
// removing the default:
//
//   - required   -> "<type>:<id>#<approverPermission>". SpiceDB governs, so an
//     empty pool later is a real refusal.
//   - session-only -> no resource set; the session's own approvers decide.
//   - undeclared -> ok=false. The class published no answer for this type, so
//     there is nothing to route to and nothing safe to assume.
//
// This replaces "<type>:<id>#owner" hardcoded at four call sites, which made a
// type whose `owner` is a real computed permission indistinguishable from one
// whose `owner` is a bare relation nothing ever writes.
func approverSetFor(standings map[string]ResourceStanding, resourceType, resourceID string) (set string, ok bool) {
	if resourceType == "" || resourceID == "" {
		return "", true // no resource named; the session approve-set is the pool
	}
	rs, declared := standings[resourceType]
	if !declared {
		return "", false
	}
	if rs.Standing != spiceboxv1alpha1.StandingRequired {
		return "", true
	}
	return resourceType + ":" + resourceID + "#" + rs.ApproverPermission, true
}

// preconditionWaiverApproverSets resolves the resource-owner subject-sets a
// precondition WAIVER card routes to. It is called IDENTICALLY at raise time
// (buildPreconditionWaiverAsk) and publish time (buildPreconditionWaiverPending),
// which is the whole reason it is a function: a card whose two halves resolved a
// different audience would raise to one set and then refuse its own clicker.
//
// A precondition's own declared Approvers win when set — they name the
// risk-answerer directly, which a userless session needs because the refused
// resource's owner set is often empty. Otherwise the resource's resolved
// standing decides, exactly as approverSetFor does for a tool_call: an undeclared
// standing is refused (declared=false), and a session-only or no-resource case
// yields the empty set, leaving the session approve-set as the pool.
func preconditionWaiverApproverSets(preApprovers []string, standings map[string]ResourceStanding, resourceType, resourceID string) (sets []string, declared bool) {
	if len(preApprovers) > 0 {
		return preApprovers, true
	}
	set, ok := approverSetFor(standings, resourceType, resourceID)
	if !ok {
		return nil, false
	}
	if set == "" {
		return nil, true
	}
	return []string{set}, true
}

// ConstantInstancesOf maps a resource type to the object id its checks name on
// EVERY call, when its permissions agree on one.
//
// Built from the same surface the plan gate already carries, so it needs no new
// plumbing: permsurface.Enumerate resolves a placeholder-free, expr-free
// template with nil args into Descriptor.ConstantResourceID.
//
// This exists because an AMENDMENT adds a permission, and the instance that
// permission reaches can be one the agent is structurally unable to name.
// git.yaml keys git_repo read/write on the literal "workspace" — the session's
// own checked-out copy — which no plan ever mentions because the toolkit fixes
// it, not the author. The amendment's slot ref therefore carried a bare type
// with an empty id, the approval bound nothing, and every later call escalated
// again: a human clicking Approve forever (observed live 2026-08-21).
//
// Types whose permissions DISAGREE on a constant are omitted rather than
// guessed. Binding one of several candidate instances would grant an instance
// the human was never shown, which is worse than the escalation it avoids.
func ConstantInstancesOf(surface []permsurface.Descriptor) map[string]string {
	seen := map[string]string{}
	conflict := map[string]bool{}
	for _, d := range surface {
		if d.ResourceType == "" || d.ConstantResourceID == "" {
			continue
		}
		if prev, ok := seen[d.ResourceType]; ok && prev != d.ConstantResourceID {
			conflict[d.ResourceType] = true
			continue
		}
		seen[d.ResourceType] = d.ConstantResourceID
	}
	for t := range conflict {
		delete(seen, t)
	}
	return seen
}

// PlanningNotesOf reads the per-permission planning guidance the AgentClass
// controller published, keyed "<resourceType>/<permission>" — the same key
// PermissionTitlesOf uses, off the same status list.
//
// The planning sibling of PermissionTitlesOf: a title is what a human reads
// when DECIDING, a note is what the agent reads when DECLARING. Exported and
// called from both Loop-construction sites for the same reason as its
// siblings — one place computes it, so there is nothing left to hand-roll and
// drift.
func PlanningNotesOf(class *spiceboxv1alpha1.AgentClass) map[string]string {
	out := make(map[string]string, len(class.Status.ResolvedPermissionTitles))
	for _, rt := range class.Status.ResolvedPermissionTitles {
		if rt.PlanningNote != "" {
			out[rt.ResourceType+"/"+rt.Permission] = rt.PlanningNote
		}
	}
	return out
}

// AuthoredExamplesOf reads the class author's worked plan examples off
// spec.authz.planGate.examples, flattened into the runner's own shape.
//
// Exported and called from both Loop-construction sites, for the same reason as
// its siblings here: one place does the conversion, so there is nothing left to
// hand-roll and drift. Every handle in these was checked at admission
// (validatePlanExamples), so a class that got this far cannot be teaching a
// handle its agent may not declare.
func AuthoredExamplesOf(class *spiceboxv1alpha1.AgentClass) []AuthoredExample {
	if class == nil || class.Spec.Authz == nil || class.Spec.Authz.PlanGate == nil {
		return nil
	}
	src := class.Spec.Authz.PlanGate.Examples
	if len(src) == 0 {
		return nil
	}
	out := make([]AuthoredExample, 0, len(src))
	for _, ex := range src {
		phases := make([]AuthoredExamplePhase, 0, len(ex.Phases))
		for _, ph := range ex.Phases {
			phases = append(phases, AuthoredExamplePhase{
				ID: ph.ID, Label: ph.Label, Permissions: ph.Permissions, Slots: ph.Slots,
			})
		}
		out = append(out, AuthoredExample{Task: ex.Task, Phases: phases})
	}
	return out
}

// promoteObservedSlots turns the facts this session has recorded into slot
// bindings, for the slots whose fillFrom admits the observed source.
//
// Called once per dispatch round from dispatchToolUses, after every tool has
// returned — see the call site for why post-dispatch and why not inside the
// per-call goroutines. Without this call nothing anywhere produces a candidate
// for a `fillFrom: [observed]` slot, so the slot is permanently empty and the
// whole precondition gate is inert while every test still passes; the call site
// is therefore load-bearing in a way no unit test of PromoteObservedSlots can
// establish on its own.
//
// It is NOT gated on whether any slot admits `observed`. That looks like a free
// optimization and would cost the one diagnostic that matters most: a class
// that declares `observes` on a tool but forgets `observed` in the slot's
// fillFrom is the likeliest authoring mistake here, and it is precisely the
// case a "no slot admits observed, skip" gate would silence. Promotion logs
// that drop by resource id instead, so "the slot is empty" is answerable from
// logs.
//
// Advisory: every failure is logged and swallowed. An unpromoted binding leaves
// the gated call to be denied by the ordinary authorization path with the
// ordinary audit record — never opened — so a promotion failure must not also
// fail the tool results this round already produced.
func (l *Loop) promoteObservedSlots(ctx context.Context) {
	if l.Engine == nil || l.AgentClass == nil || len(l.AgentClass.Spec.GetSlots()) == 0 {
		return
	}
	// No canonical subject means there is nobody to Check the candidate for, and
	// binding without a Check is the escalation this whole path exists to
	// prevent. Same guard the cold-start BindClassDefaults call site applies.
	if l.AuthSubject() == "" || l.toolAuthMode() == ToolAuthModeDisabled {
		return
	}
	specs, err := boundEntitySpecsForAutofill(l.AgentClass)
	if err != nil {
		// A class whose slot preconditions will not compile promotes NOTHING —
		// binding without the gate the class declared is the one outcome that
		// must not happen. Surfaced rather than swallowed: this is a class the
		// reconciler should have refused, and the slot going empty is otherwise
		// indistinguishable from nothing having been observed.
		slog.Default().Info("observed slot promotion skipped: the class's slot preconditions did not compile",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", err.Error())
		return
	}
	if err := l.Engine.PromoteObservedSlots(ctx, l.bindingScope(), l.authzSessionRef(),
		specs, l.AuthSubject()); err != nil {
		if errors.Is(err, authz.ErrSlotPinned) {
			// An authorization outcome, not a hiccup: a single-occupancy pin
			// refused this instance. Warn (louder than a mechanism error), keep
			// the Info-trail grep key below for an operator, and record the
			// refusal so the next gated call on this type explains itself to the
			// model rather than reading as a bare denial.
			slog.Default().Warn("promoting observed slot candidates refused by a single-occupancy pin; surfacing to the model",
				"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", err.Error())
			l.recordSlotPinRefusal(ctx, specs, err.Error())
			return
		}
		slog.Default().Info("promoting observed slot candidates failed; any gated call stays unbound",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", err.Error())
		return
	}
	// A promotion that succeeded supersedes any recorded refusal for these
	// types: the facts that refused no longer hold, and a stale "pinned to A"
	// explanation must not be appended to a later denial.
	l.clearSlotPinRefusalsForSpecs(specs)
	// Mirror any pin the promotion just produced onto status.slotPins —
	// display-only, read back from SpiceDB per not-yet-mirrored
	// single-occupancy type (steady state: no SpiceDB reads once mirrored,
	// but still one AgentSession GET; see slot_pin_mirror.go).
	l.mirrorBoundSlotPins(ctx, specs)
}

// explainSlotPrecondition builds the ToolCallAuthz hook's ExplainPrecondition
// dep: for a call the checker denied, the slot precondition that says why the
// slot it names is empty.
//
// It returns a nil FUNC — not a func that returns nil — when this session can
// never have a precondition to explain: no class, or no declared slots. The
// hook's own nil check then skips the whole path, so a class that declares no
// gate pays nothing and behaves exactly as it did before preconditions existed.
//
// # A missing memory store is deliberately NOT one of those conditions
//
// "No gate is declared over this session" and "the gate's facts cannot be read"
// are different answers, and a nil FUNC can only say the first. Switching the
// whole explanation off because l.Mem is nil therefore reports a gated class as
// ungated — and under toolCalls.mode: permissive that lets a gated call through,
// which is the fail-open this gate exists to remove.
// ExplainPreconditionDenial's own nil-memory branch is the correct answer
// (Unevaluatable, enforced), and leaving the func wired is what reaches it.
//
// A store-less session is a property of the STRUCT, not of any shipped binary:
// Loop.Mem is an optional field, and this package's tests construct Loops
// without it. internal/cmd/runner builds its signing memory unconditionally and
// always assigns it, and the two other production Loops that omit Mem — the
// toolspec discovery agent and the identity setup agent — declare no AgentClass,
// so the guard above already hands them a nil func. Fail-closed here is about
// what the struct permits, not about a session anyone has observed.
//
// One consequence of reaching that branch is worth naming: the refusal leaves
// NO durable record. recordAuthzDecision (pipeline_wiring.go) returns early on
// a nil Mem, so the call is refused with EnforceAlways and no authzdecision row
// is written — invisible to `oap audit verify`, and unclassifiable by
// steelthread's gateRefused, which reads exactly those rows. That is inherent
// rather than a gap to close here (no store, no record), and refusing without a
// record is still strictly better than proceeding without one.
//
// # Why the specs are rebuilt per call rather than captured once
//
// boundEntitySpecsForAutofill compiles the class's predicates, and this closure
// outlives many turns. Capturing the compiled set here would freeze it at
// pipeline-construction time; rebuilding keeps the explanation derived from the
// class as it is now, at the cost of a compile on a call that is ALREADY being
// refused. The compile only runs for a slot that declares `requires[]` at all.
//
// It is not advisory. A denial it cannot EXPLAIN is left as the check phrased
// it; a gate it cannot EVALUATE comes back as a denial of its own, because a
// gate that cannot be evaluated has to be treated as a gate that said no.
func (l *Loop) explainSlotPrecondition() func(context.Context, authz.Permission, map[string]any) *authz.PreconditionDenial {
	if l.AgentClass == nil || len(l.AgentClass.Spec.GetSlots()) == 0 {
		return nil
	}
	return func(ctx context.Context, perm authz.Permission, args map[string]any) *authz.PreconditionDenial {
		if perm.Check == nil {
			return nil
		}
		specs, err := boundEntitySpecsForAutofill(l.AgentClass)
		if err != nil {
			// The same class the reconciler should have refused. It already
			// promotes nothing (promoteObservedSlots logs and returns), so every
			// slot it declares is empty — and a gate whose own declaration will
			// not compile is a gate that could not be evaluated, which is a gate
			// that said no. Returning nil for a GATED type would be
			// indistinguishable from "no gate is declared", so permissive mode
			// would run the call: the same fail-open
			// ExplainPreconditionDenial's four branches close one layer down,
			// reached one layer up.
			//
			// Scoped to a type this class actually gated, read off the SPEC.
			// The conversion failed wholesale so there are no compiled specs to
			// consult, but compilability was never needed to answer the
			// question: requires[] is a plain spec field, and which types the
			// AUTHOR gated is knowable whether or not their predicates build.
			// Refusing a type the author never gated would do to it exactly what
			// ExplainPreconditionDenial's !held branch refuses to do — turn an
			// ordinary, appealable permission denial into an unappealable one —
			// and for an External call it would delete the approval card that is
			// the human's entire route.
			if !l.classDeclaresGateOn(perm.Check.ResourceType) {
				// Not silent: the class IS broken, and an operator reading a
				// denial on this session should see that even though the gate
				// is not what refused this particular call.
				slog.Default().Info("a denied call is left as the checker phrased it: the class's slot preconditions did not compile, but this call's type declares none",
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
					"resourceType", perm.Check.ResourceType, "err", err.Error())
				return nil
			}
			slog.Default().Info("a denied call is refused as unevaluatable: the class's slot preconditions did not compile",
				"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
				"resourceType", perm.Check.ResourceType, "err", err.Error())
			// The compile error is a fault in the CLASS's declaration and is
			// addressed to whoever wrote it, so it stays in the log — the same
			// split ExplainPreconditionDenial's own CEL-error branch makes.
			return &authz.PreconditionDenial{
				Unevaluatable: true,
				Message: "the agent class governing this call declares a slot precondition that does not compile," +
					" so the gate could not be evaluated. The call is refused rather than let past a gate nobody" +
					" could read. The declaration is at fault, not this call: no action here can satisfy it, and" +
					" its author has to fix the class.",
			}
		}
		return authz.ExplainPreconditionDenial(ctx, l.Mem, l.bindingScope(), specs, *perm.Check, args)
	}
}

// classDeclaresGateOn reports whether the class declares a precondition over
// resourceType, read straight off the SPEC.
//
// It exists for the one caller that has no compiled specs to consult: when
// slotspec's conversion fails, every gate in the class is unbuildable, but
// AuthzSlot.Requires is a plain spec field and needs no compilation to inspect.
// So "which types did the author gate" stays answerable even when "what do
// those gates say" does not — which is what keeps the compile-failure refusal
// off types nobody gated.
//
// Mirrors slotRules (pkg/authz/precondition_denial.go): the FIRST slot matching
// the type answers, because a class declaring the same resource type twice is
// refused at admission and there is no second slot to merge with.
func (l *Loop) classDeclaresGateOn(resourceType string) bool {
	if l.AgentClass == nil || resourceType == "" {
		return false
	}
	for _, s := range l.AgentClass.Spec.GetSlots() {
		if s.ResourceType == resourceType {
			return len(s.Requires) > 0
		}
	}
	return false
}

// slotOccupancyRebindFor returns the occupancy and rebind modes the class
// declared for resourceType, read straight off the spec slot — plain spec
// fields that need no status derivation, the same way classDeclaresGateOn reads
// `requires`.
//
// These are recorded onto a tool_approval / precondition_waiver card's details
// at RAISE time, because the channelsd decision handler that eventually binds
// the grant holds no AgentClass to resolve them from. Empty for a type the
// class does not declare — which also has no slot_grant to write, so there is
// no bind to pin.
//
// Mirrors classDeclaresGateOn: the FIRST slot matching the type answers, since
// admission refuses a class declaring the same type twice.
func (l *Loop) slotOccupancyRebindFor(resourceType string) (occupancy, rebind string) {
	if l.AgentClass == nil || resourceType == "" {
		return "", ""
	}
	for _, s := range l.AgentClass.Spec.GetSlots() {
		if s.ResourceType == resourceType {
			return s.Occupancy, s.Rebind
		}
	}
	return "", ""
}

// slotPinRefusal is one recorded single-occupancy pin refusal: the user-visible
// text (authz.GrantSlots' own message, naming the pinned instance and the route
// out) plus the pinned instance id read back from SpiceDB at record time —
// which is what lets the denial attachment skip a call that named the pinned
// instance itself (see pinRefusalApplies). PinnedID is "" when the pin could
// not be read (no SlotPinner wired, or the advisory read failed); the
// attachment then proceeds without the instance-skip.
type slotPinRefusal struct {
	Text     string
	PinnedID string
}

// recordSlotPinRefusal remembers a single-occupancy pin's refusal so a later
// gated tool call denied on the same resource type can explain itself to the
// model. Keyed per resource type (the declared spec types the text names, via
// the same token-boundary match slotPinRefusalFor used to do at read time) with
// NEWEST-WINS overwrite — two refusals for one type across turns keep only the
// latest, so the model is never handed an explanation older than the most
// recent ruling. The entry is cleared again when the type binds or its pin
// moves (clearSlotPinRefusals at the promote/approval success paths).
func (l *Loop) recordSlotPinRefusal(ctx context.Context, specs []authz.BoundEntitySpec, text string) {
	if text == "" || len(specs) == 0 {
		return
	}
	pinner, _ := l.SlotBinder.(authz.SlotPinner)
	for _, et := range specs {
		if et.ResourceType == "" || !mentionsResourceType(text, et.ResourceType) {
			continue
		}
		ref := slotPinRefusal{Text: text}
		if pinner != nil {
			id, err := pinner.ReadPin(ctx, et.ResourceType, l.authzSessionRef())
			if err != nil {
				// Advisory read: without it the attachment just loses the
				// named-the-pinned-instance skip, never the explanation itself.
				// Said out loud per the no-silent-errors rule.
				slog.Default().Info("slot pin refusal recorded without the pinned id (ReadPin failed); the denial attachment proceeds without the pinned-instance skip",
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
					"resourceType", et.ResourceType, "err", err.Error())
			} else {
				ref.PinnedID = id
			}
		}
		l.slotPinRefusalMu.Lock()
		if l.slotPinRefusals == nil {
			l.slotPinRefusals = map[string]slotPinRefusal{}
		}
		l.slotPinRefusals[et.ResourceType] = ref // newest wins
		l.slotPinRefusalMu.Unlock()
	}
}

// clearSlotPinRefusals drops the recorded refusal for each named resource type.
// Called at every path where the type's pin state just CHANGED or was
// reaffirmed — a nil-error promotion (the facts the refusal described no longer
// refuse), and narrowToApproved's successful bind/move (the pin may now be on a
// different instance) — so a stale "pinned to A" explanation cannot be appended
// to a denial after an approved A→B move.
func (l *Loop) clearSlotPinRefusals(types []string) {
	if len(types) == 0 {
		return
	}
	l.slotPinRefusalMu.Lock()
	defer l.slotPinRefusalMu.Unlock()
	for _, rt := range types {
		delete(l.slotPinRefusals, rt)
	}
}

// clearSlotPinRefusalsForSpecs is clearSlotPinRefusals over the resource types
// a promotion's specs declare — the shape the two promote nil-error call sites
// have in hand.
func (l *Loop) clearSlotPinRefusalsForSpecs(specs []authz.BoundEntitySpec) {
	if len(specs) == 0 {
		return
	}
	types := make([]string, 0, len(specs))
	for _, et := range specs {
		if et.ResourceType != "" {
			types = append(types, et.ResourceType)
		}
	}
	l.clearSlotPinRefusals(types)
}

// bindingResourceTypes returns the distinct resource types a binding set names
// — the shape narrowToApproved has in hand when its bind/move succeeds.
func bindingResourceTypes(bindings []authz.SlotBinding) []string {
	seen := make(map[string]bool, len(bindings))
	var out []string
	for _, b := range bindings {
		if b.ResourceType != "" && !seen[b.ResourceType] {
			seen[b.ResourceType] = true
			out = append(out, b.ResourceType)
		}
	}
	return out
}

// slotPinRefusalFor returns the recorded pin refusal for resourceType, and
// whether one exists. A map lookup, not a scan: the record is keyed per type
// at write time, holding only the newest refusal for each.
func (l *Loop) slotPinRefusalFor(resourceType string) (slotPinRefusal, bool) {
	if resourceType == "" {
		return slotPinRefusal{}, false
	}
	l.slotPinRefusalMu.Lock()
	defer l.slotPinRefusalMu.Unlock()
	ref, ok := l.slotPinRefusals[resourceType]
	return ref, ok
}

// mentionsResourceType reports whether text names resourceType as a WHOLE
// token — bounded on each side by a non-identifier byte or a string edge — so a
// declared type whose name is a tail of another (a "pull_request" slot against
// a "github_pull_request" refusal) is not matched by accident. Both refusal
// shapes end the type on a non-identifier byte (a ':' or a space), so the one
// rule covers both.
func mentionsResourceType(text, resourceType string) bool {
	for from := 0; ; {
		i := strings.Index(text[from:], resourceType)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(resourceType)
		leftOK := i == 0 || !isResourceTypeByte(text[i-1])
		rightOK := end == len(text) || !isResourceTypeByte(text[end])
		if leftOK && rightOK {
			return true
		}
		from = i + 1
	}
}

// isResourceTypeByte reports whether b can appear inside a SpiceDB object type
// name ([a-zA-Z0-9_]). Used to require a token boundary around a type match.
func isResourceTypeByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}
