package hooks

import (
	"context"
	"encoding/json"
	"time"

	"fmt"
	"sort"
	"strconv"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// HandleResolver maps a CALL to the permission handle its dispatch will be
// checked against. Returns ok=false for calls with no handle (meta tools,
// passthrough tools) — those are not a plan-gate concern.
//
// Per CALL, not per tool. A sandbox tool is one tool for a whole CLI whose base
// permission is the toolkit default, so resolving from the tool alone gave
// `gh pr view` no handle at all and the gate skipped it as unpermissioned —
// while the call itself resolves to perm:read:github_repo. args is what
// distinguishes `pr view` from `pr create` on the same tool.
//
// The runner supplies this by projecting the same envelope permsurface
// enumerated, so the gate and the dispatcher agree on what a call means by
// construction rather than by two parallel derivations.
type HandleResolver func(toolName string, args map[string]any) (permsurface.Handle, bool)

// PlanGateRecorder persists one plan-gate event.
//
// The gate holds its own recorder rather than returning pipeline.AuditRecords,
// following the authzdecision/approval family: AuditRecord.Fields is a
// map[string]any and would erase plangateaudit.Content's typing, and the
// runnerHost.writeAudit switch is scoped to the infoleakage family.
type PlanGateRecorder interface {
	Record(ctx context.Context, c plangateaudit.Content) error
}

// PlanGateLogger is the minimal logging surface the gate needs. Satisfied by
// logr and slog adapters alike.
type PlanGateLogger interface {
	Info(msg string, keysAndValues ...any)
}

// PlanGateDeps are the plan gate's dependencies.
type PlanGateDeps struct {
	// Mode is the RESOLVED plan-gate mode, from
	// EffectiveSettings.Authz.PlanGate — never AgentClass.spec.authz. Reading
	// the class spec directly would let a class opt out past a cluster floor by
	// being the only value anyone consults.
	Mode string

	// Plan is the FALLBACK plan, used until the agent declares phases. It is
	// the synthesized one-phase session plan whose ceiling is the whole
	// surface, so a session that never plans behaves exactly as it does today.
	Plan plangate.Plan

	// RequirePlan denies every permissioned call that is not attributable to a
	// plan the agent actually declared.
	//
	// Without it, "don't plan" is a total bypass: an agent that simply never
	// calls update_plan falls through to the synthesized whole-surface fallback
	// and is completely unconstrained — an escape hatch from the one thing the
	// feature exists to do. Only meaningful under enforcing.
	RequirePlan bool

	// CurrentPlan returns the frozen plan now in force, if any.
	//
	// This MUST be a function, not a value. The frozen plan comes into
	// existence mid-session when the agent declares phases, and a snapshot
	// taken at hook-construction time would pin the gate to the fallback
	// forever — the ceiling would never narrow, the feature would be silently
	// inert, and every unit test would still pass because they construct the
	// hook with the plan they want. That is exactly what happened before
	// TestE2E_planGateMultiPhaseChainRunsEndToEnd existed.
	CurrentPlan func() (plangate.Plan, bool)

	// Records returns this session's plan-gate log, newest last.
	//
	// The gate derives the active phase by FOLDING this rather than holding it:
	// approval state cannot live in memory (a restart or idle re-hydrate would
	// lose a human's decision) and cannot live in the plan document (the agent
	// rewrites that every call). Nil ⇒ the gate falls back to phase 0, which is
	// the single-phase shape.
	Records func() []plangateaudit.Content

	// ActivePhase is the fallback index used when Records is nil.
	ActivePhase int

	// Surface resolves handles to their state impact so the card can call out
	// external reach. Optional.
	Surface []permsurface.Descriptor

	// Approvers is the pre-rendered approver population for cards
	// (channelkinds.DescribeApprovers output). Optional.
	Approvers string

	// MaxSingleCardHandles folds a long ceiling on a Routine card. From the
	// resolved PlanRendering limits; zero ⇒ no folding.
	MaxSingleCardHandles int

	// SlotStanding maps each declared slot resource type to how an approval on
	// it gets its authority — v1alpha1.StandingSessionOnly (a human vouched) or
	// StandingRequired (a human delegated something they already held). The
	// SAME per-type map the runner holds (Loop.PlanGateSlotStanding); threaded
	// through so the card's per-slot sentence and the audit record agree with
	// what approverCanDelegateSlots will actually do. Absent (nil, or a type
	// missing from it) means session-only — never assume `required` for an
	// unresolved type, matching the runner's own bypass rule.
	SlotStanding map[string]string

	// SlotPermissions maps each declared slot resource type to the SpiceDB
	// permission an approval on it grants (Loop.PlanGateSlotPermissions).
	// Display only: used to phrase the per-slot sentence ("push", "contact",
	// …), never an input to any decision.
	SlotPermissions map[string]string

	// SlotValueTransforms maps each declared slot resource type to the chain
	// that turns a value into its SpiceDB object id (Loop.PlanGateSlotTransforms).
	// Display only, and only for the card: it is how two slots of different
	// types naming ONE instance are rendered as one line instead of two.
	SlotValueTransforms map[string][]string

	// PermissionTitles maps each declared permission's "<resourceType>/<permission>"
	// key to the human phrase the AgentClass controller published (the SAME map
	// the runner holds — Loop.PlanGatePermissionTitles, from
	// runner.PermissionTitlesOf). Display only: threaded straight into every
	// card built here so a declared title reaches the approver instead of the
	// detokenized fallback. Absent (nil, or a pair missing from it) means no
	// title was declared for that pair.
	PermissionTitles map[string]string

	// ResourceDisplays maps each declared resource type to its icon/label/name
	// presentation (the SAME map the runner holds —
	// Loop.PlanGateResourceDisplays, from runner.ResourceDisplaysOf). Display
	// only: threaded straight into every card built here so a resource line
	// carries a derived label and a real link instead of the bare wire type
	// name. Absent (nil, or a type missing from it) means no display was
	// declared for that type.
	ResourceDisplays map[string]plangate.ResourceDisplay

	Resolve  HandleResolver
	Recorder PlanGateRecorder
	Logger   PlanGateLogger

	// Now is injectable so records are deterministic in tests. Nil ⇒
	// time.Now. Stamped ONCE per record and reused on retry: the memory
	// facade's append-only idempotency compares marshaled bytes, so a
	// re-stamped At turns a safe retry into ErrAppendOnlyConflict.
	Now func() time.Time
}

// PlanGate gates a permissioned tool call on membership in the active phase's
// ceiling.
//
// Slice 1 never halts. It computes the real ceiling, performs the real
// membership test, and records the real outcome — including "enforcing would
// have denied this" — while letting every call through. That is what lets the
// gate be rolled out without behavior risk while still producing the dataset
// that decides whether phase-shaped approval is viable.
//
// The halting path lands with the justified-retry ladder. It is deliberately
// NOT stubbed here: a dead `if mode == enforcing` branch would read as tested
// when nothing exercises it.
type PlanGate struct {
	deps PlanGateDeps
}

func NewPlanGate(deps PlanGateDeps) *PlanGate { return &PlanGate{deps: deps} }

func (h *PlanGate) Name() string             { return "plan_gate" }
func (h *PlanGate) Points() []pipeline.Point { return []pipeline.Point{pipeline.PreToolCall} }

func (h *PlanGate) now() time.Time {
	if h.deps.Now != nil {
		return h.deps.Now()
	}
	return time.Now().UTC()
}

func (h *PlanGate) logf(msg string, kv ...any) {
	if h.deps.Logger != nil {
		h.deps.Logger.Info(msg, kv...)
	}
}

func (h *PlanGate) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	// disabled means off entirely: no records, no logs, no overhead. An
	// unrecognized or empty mode reaching here is a wiring bug (the runner only
	// registers the hook for a known non-disabled mode), so it behaves as
	// disabled rather than as enforcing.
	if h.deps.Mode != "logging" && h.deps.Mode != "enforcing" {
		return pipeline.Decision{}
	}
	// Input.Tool is nil at every point except PreToolCall/PostToolCall.
	if in.Tool == nil {
		return pipeline.Decision{}
	}

	rec := plangateaudit.Content{
		Tool:       in.Tool.Name,
		UseID:      in.Tool.UseID,
		Mode:       h.deps.Mode,
		PlanDigest: h.activePlan().Digest(),
		Provenance: h.Name(),
		At:         h.now(),
	}

	// ONE fold per Eval, shared by the ceiling, the budget and the approval
	// check — each used to walk every record independently.
	st, folded, foldErr := h.foldOnce()
	activePhase, ceiling, err := h.resolveCeiling(st, folded, foldErr)
	phase := int32(activePhase)
	rec.PhaseIndex = &phase

	if err != nil {
		// The gate could not establish what it is gating against. The ceiling
		// is EMPTY, not stale — see plangate.State.ActiveCeiling for why the
		// opposite default fails open — so every call is out of it and records
		// as a would-deny. Under logging that is loud and non-fatal; it must
		// never be silent in either direction.
		h.logf("plan_gate: cannot resolve the active phase ceiling; holding nothing until re-selection",
			"session", in.Session.String(),
			"tool", in.Tool.Name,
			"phase", activePhase,
			"err", err.Error())
		rec.Event = plangateaudit.EventGateWouldDeny
		rec.Outcome = plangateaudit.OutcomeWouldDeny

		if h.enforcing() {
			// Logging proceeds here because nothing is at stake. Enforcing
			// cannot: the alternative is running a call whose authority nobody
			// can establish.
			rec.Outcome = plangateaudit.OutcomeDenied
			h.write(ctx, in, rec)
			return pipeline.Decision{
				Verdict: pipeline.Deny,
				Reason: "plan gate: the approved-plan state could not be established, so you hold " +
					"nothing right now. Call select_phase to re-establish which phase you are in.",
			}
		}
		h.write(ctx, in, rec)
		return pipeline.Decision{}
	}

	if h.deps.Resolve != nil {
		if handle, ok := h.deps.Resolve(in.Tool.Name, argsMapOf(in)); ok {
			rec.Handle = handle.String()
			// requirePlan: an undeclared plan is itself the denial condition, so
			// the whole-surface fallback must NOT authorize anything. Checked
			// before ceiling membership because the fallback would otherwise
			// admit every handle and the bypass would stand.
			// Refused in EVERY mode, logging included — unlike everything below
			// it, which logging only records.
			//
			// The two claims requirePlan makes are separable: "you must declare a
			// plan" and "you must stay within its ceiling". Welding them to
			// enforcement made logging mode unable to produce the dataset it
			// exists for. Five live sessions yielded nothing usable, and the last
			// one showed why: the system prompt carried "You MUST declare phases
			// with update_plan BEFORE your first permissioned tool call" —
			// verified present in the running prompt — and the agent cloned a
			// repository anyway. Nothing depended on planning, so the model was
			// reading the situation correctly; no wording could have fixed that.
			//
			// Enforcing the declaration alone yields real phases, ceilings,
			// severity and approver data with no call ever refused for being
			// OUT of ceiling — which is the evidence enforcement is gated on,
			// without switching on the enforcement it is meant to justify.
			if h.deps.RequirePlan && !h.hasDeclaredPlan() {
				// Denied, not would_deny: the call genuinely did not run, and a
				// log that said otherwise would overstate what agents got away
				// with in exactly the dataset used to judge enforcement.
				rec.Event = plangateaudit.EventGateDenied
				rec.Outcome = plangateaudit.OutcomeDenied
				h.write(ctx, in, rec)
				return pipeline.Decision{
					Verdict: pipeline.Deny,
					Reason: "plan gate: this session requires an approved plan before any " +
						"permissioned tool runs, and none is declared yet. Call update_plan " +
						"with `phases`, naming the permissions each stage needs and a `why` " +
						"for each. A narrow read-only first phase is approved automatically.",
				}
			}

			if _, inCeiling := ceiling[handle]; inCeiling {
				rec.Event = plangateaudit.EventGateAllowed
				rec.Outcome = plangateaudit.OutcomeAllow

				// The FOCUS gate, and it is independent of the ceiling: a phase
				// can be perfectly within its permissions and still be grinding.
				// Forty calls out of a legal readonly ceiling breaks no
				// permission rule, so no permission model can see it — which is
				// exactly why the budget is checked separately here rather than
				// folded into the membership test.
				//
				// BEFORE the approval check, deliberately. A phase that has
				// spent its budget cannot run whatever a human says, so asking
				// one to approve it would spend their attention on a decision
				// that changes nothing — and would hand the agent an approval
				// prompt when the actionable answer is "re-plan".
				if folded && st.BudgetExhausted(activePhase) {
					rec.Event = plangateaudit.EventGateWouldDeny
					rec.Outcome = plangateaudit.OutcomeWouldDeny
					if h.enforcing() {
						rec.Outcome = plangateaudit.OutcomeDenied
						h.write(ctx, in, rec)
						return pipeline.Decision{
							Verdict: pipeline.Deny,
							Reason:  h.budgetReason(activePhase),
						}
					}
					h.write(ctx, in, rec)
					return pipeline.Decision{}
				}

				// Ceiling and approval are INDEPENDENT gates and BOTH must pass.
				// Order matters: a handle outside the ceiling is refused no matter
				// who approved the phase, so the ceiling is tested first. Asking
				// first and allowing on a yes would make one approved phase
				// authorize the whole surface — the opposite of a ceiling.
				if needs, denied := h.phaseNeedsApproval(st, folded, activePhase); needs {
					return h.requestPhaseApproval(ctx, in, rec, activePhase, denied)
				}
			} else {
				rec.Event = plangateaudit.EventGateWouldDeny
				rec.Outcome = plangateaudit.OutcomeWouldDeny
			}
		} else {
			// No handle: a meta or passthrough tool the gate does not govern.
			// Recorded without a handle rather than with a fabricated one.
			rec.Event = plangateaudit.EventGateAllowed
			rec.Outcome = plangateaudit.OutcomeAllow
		}
	} else {
		rec.Event = plangateaudit.EventGateAllowed
		rec.Outcome = plangateaudit.OutcomeAllow
	}

	// A would-deny is what a human would have been asked about, so the card is
	// BUILT — not sketched, not counted — and recorded. That is what makes
	// logging mode a real exercise of the path: the escaper, the severity
	// classifier, the fold and the renderer all run against production traffic.
	// The only steps skipped are publishing it and blocking on the answer.
	//
	// No grant can be written as a consequence, and there is no conditional
	// saying so: a grant follows a human approving a PUBLISHED card, and
	// nothing here publishes.
	if rec.Outcome == plangateaudit.OutcomeWouldDeny {
		sev := plangate.Classify(plangate.Request{
			Kind:              plangate.KindPlanApproval,
			HasExternalHandle: h.hasExternal(rec.Handle),
			// Slice 1 records no envelope (there is no supersede path yet), and
			// the missing-envelope rule is scoped to supersedes precisely so it
			// does not escalate a first plan. Stated explicitly rather than
			// left to a zero value.
			EnvelopeRecorded: false,
		})
		rec.Severity = string(sev)

		card := plangate.BuildCard(plangate.CardInput{
			Plan:                 h.activePlan(),
			PhaseIndex:           h.deps.ActivePhase,
			Severity:             sev,
			Surface:              h.deps.Surface,
			Approvers:            h.deps.Approvers,
			MaxSingleCardHandles: h.deps.MaxSingleCardHandles,
			SlotStanding:         h.deps.SlotStanding,
			SlotPermissions:      h.deps.SlotPermissions,
			SlotValueTransforms:  h.deps.SlotValueTransforms,
			PermissionTitles:     h.deps.PermissionTitles,
			ResourceDisplays:     h.deps.ResourceDisplays,
		})
		if js, err := card.JSON(); err != nil {
			h.logf("plan_gate: rendering the approval card failed",
				"session", in.Session.String(), "tool", in.Tool.Name, "err", err.Error())
		} else {
			rec.CardJSON = js
		}
	}

	// A denied call carrying a justification is a JUSTIFIED RETRY: the agent
	// was refused, said why it needs the reach anyway, and is asking for the
	// plan to be amended.
	//
	// This is the rung that stops a legitimate mid-task discovery from being
	// unrecoverable. An agent that genuinely needs reach it did not anticipate
	// explains itself and a human decides — rather than the agent either giving
	// up or grinding against a wall it cannot move.
	// Deliberately NOT gated on phaseNeedsApproval, and the reason is worth
	// stating because the opposite looks safer.
	//
	// phaseNeedsApproval runs only on the in-ceiling arm, so a justified
	// out-of-ceiling call reaches here with the phase never challenged. Asking
	// for the PHASE here instead was tried and is a BYPASS: requestPhaseApproval
	// returns Allow-with-ask (correct on the in-ceiling arm, where the handle is
	// already within the ceiling), and the executor falls through with that
	// verdict on a yes — so approving the phase would let the OUT-of-ceiling
	// call run, which is strictly worse than the amendment it replaced.
	//
	// What actually closes the escape is in the fold: an approved amendment
	// records only the handle it named and no longer falls through to the
	// whole-phase grant. The phase therefore stays un-approved, and the next
	// in-ceiling call hits phaseNeedsApproval and raises the phase card for
	// real. The human who approved the amendment got exactly what the card
	// said: one handle.
	if rec.Outcome == plangateaudit.OutcomeWouldDeny && in.Tool.Justification != "" {
		return h.raiseAmendment(ctx, in, rec, activePhase)
	}

	// Under enforcing, an out-of-ceiling call is refused with an actionable
	// reason. Under logging the identical computation happened and the call
	// proceeds — that difference, and only that difference, is what the two
	// modes mean.
	if h.enforcing() && rec.Outcome == plangateaudit.OutcomeWouldDeny {
		rec.Outcome = plangateaudit.OutcomeDenied
		handle, _ := h.deps.Resolve(in.Tool.Name, argsMapOf(in))
		reason := h.denialReason(handle, activePhase)
		h.write(ctx, in, rec)
		return pipeline.Decision{Verdict: pipeline.Deny, Reason: reason}
	}

	h.write(ctx, in, rec)
	return pipeline.Decision{}
}

// resolveCeiling derives the active phase and its ceiling.
//
// With a Records source it FOLDS the log, which is the real path: the active
// phase is a runtime-recorded selection over a frozen list, never a value the
// gate holds in memory or reads from the agent's document. Without one it falls
// back to the configured phase, which is the single-phase shape.
func (h *PlanGate) resolveCeiling(st plangate.State, folded bool, foldErr error) (int, map[permsurface.Handle]struct{}, error) {
	if foldErr != nil {
		// A log that cannot be interpreted is NOT the same as no log. Falling
		// back to the configured phase here would hand back a real ceiling on
		// unreadable state — fail OPEN — so the error propagates and the caller
		// holds nothing.
		return h.deps.ActivePhase, nil, foldErr
	}
	if !folded {
		ceiling, err := h.activePlan().Ceiling(h.deps.ActivePhase)
		return h.deps.ActivePhase, ceiling, err
	}
	ceiling, cerr := st.ActiveCeiling()
	return st.ActivePhase, ceiling, cerr
}

// foldOnce replays the log ONCE per Eval.
//
// The ceiling, the approval check and the budget all need the same folded
// state, and each used to fold independently — three walks of every record on
// every governed call, growing with session length. One fold, passed down.
//
// folded=false means there was no log to read or it could not be interpreted;
// each caller decides what that means for its own question, because the safe
// default differs: an unreadable log means the ceiling is EMPTY (fail closed),
// the phase is UNAPPROVED (fail closed), and the budget is NOT exhausted (a
// budget the runtime cannot count must not deny work that was never done).
func (h *PlanGate) foldOnce() (plangate.State, bool, error) {
	if h.deps.Records == nil {
		return plangate.State{}, false, nil
	}
	st, err := plangate.Fold(h.activePlan(), h.deps.Records())
	if err != nil {
		// Distinct from "no log": an UNREADABLE log must fail closed, while an
		// absent one falls back to the single-phase shape. Collapsing the two
		// would turn a corrupt log into a full ceiling.
		return plangate.State{}, false, err
	}
	return st, true, nil
}

// hasExternal reports whether the handle carries irreversible side effects.
func (h *PlanGate) hasExternal(handle string) bool {
	for _, d := range h.deps.Surface {
		if d.Handle.String() == handle {
			return d.StateImpact == authz.External
		}
	}
	return false
}

// write persists a record, logging rather than failing on error. Losing an
// audit write must not take down a call the gate was not going to block anyway
// — but it must not vanish either, or the dataset silently under-counts.
func (h *PlanGate) write(ctx context.Context, in pipeline.Input, rec plangateaudit.Content) {
	if h.deps.Recorder == nil {
		return
	}
	if err := h.deps.Recorder.Record(ctx, rec); err != nil {
		h.logf("plan_gate: recording the gate decision failed",
			"session", in.Session.String(),
			"tool", rec.Tool,
			"handle", rec.Handle,
			"outcome", rec.Outcome,
			"err", err.Error())
	}
}

// planInForce returns the plan that governs this session and whether one
// exists: the agent's DECLARED frozen plan, or — for a delegated child, which
// never declares one because update_plan is withheld from it — the plan
// reconstructed from the inherited root the operator wrote into its plan-gate
// log (DeriveForChild). Without the second source a plan-gated child fails the
// requirePlan check forever ("requires an approved plan", but it has no
// update_plan to satisfy it) even though it holds a real inherited ceiling —
// the wedge the plangate-child-amends-its-own-ceiling bundle caught.
//
// A root is unaffected: its CurrentPlan wins the first branch. The log fallback
// fires only when nothing was declared AND the log carries a plan, which is
// exactly the delegated-child shape.
func (h *PlanGate) planInForce() (plangate.Plan, bool) {
	if h.deps.CurrentPlan != nil {
		if p, ok := h.deps.CurrentPlan(); ok && len(p.Phases) > 0 {
			return p, true
		}
	}
	if h.deps.Records != nil {
		if p, ok := plangate.PlanFromRecords(h.deps.Records()); ok && len(p.Phases) > 0 {
			return p, true
		}
	}
	return plangate.Plan{}, false
}

// activePlan returns the plan in force (declared or inherited), else the
// synthesized session-scope fallback.
func (h *PlanGate) activePlan() plangate.Plan {
	if p, ok := h.planInForce(); ok {
		return p
	}
	return h.deps.Plan
}

// enforcing reports whether this gate refuses out-of-ceiling calls.
func (h *PlanGate) enforcing() bool { return h.deps.Mode == "enforcing" }

// denialReason builds the message the agent sees when a call is refused.
//
// This is the adoptability surface, and it is worth the care. A gate whose
// first contact with a well-behaved agent is an opaque "denied" produces
// flailing and burned turns — and the overwhelmingly common cause of a denial
// is a PLANNING MISS, not an attack: the agent did not realise it had left the
// phase it holds. So the message states three things it cannot otherwise know:
// what it asked for, what it currently holds, and the one move that would work.
//
// The remedy differs, and getting it wrong sends the agent in circles:
//
//   - Some approved phase holds this reach → name it, and point at select_phase.
//   - No approved phase holds it → selecting cannot help, so point at
//     update_plan to amend, which is a decision a human will see.
func (h *PlanGate) denialReason(handle permsurface.Handle, activePhase int) string {
	plan := h.activePlan()

	held := "no phase"
	if activePhase >= 0 && activePhase < len(plan.Phases) {
		held = "phase " + strconv.Itoa(activePhase)
		if lbl := plan.Phases[activePhase].Label; lbl != "" {
			held += " (" + lbl + ")"
		}
	}

	for i, ph := range plan.Phases {
		if i == activePhase {
			continue
		}
		for _, h2 := range ph.Permissions {
			if h2 != handle {
				continue
			}
			target := "phase " + strconv.Itoa(i)
			if ph.Label != "" {
				target += " (" + ph.Label + ")"
			}
			return fmt.Sprintf(
				"plan gate: %s needs %s, which belongs to %s — you are in %s. "+
					"Call select_phase with index %d to move there, then retry.",
				handle.String(), handle.String(), target, held, i)
		}
	}

	return fmt.Sprintf(
		"plan gate: %s is not in any phase of the approved plan (you are in %s). "+
			"Selecting a different phase will not help. Call update_plan to add this "+
			"permission to a phase, with a `why` — the change needs approval.",
		handle.String(), held)
}

// hasDeclaredPlan reports whether the agent actually declared a plan, as
// opposed to running under the synthesized whole-surface fallback.
func (h *PlanGate) hasDeclaredPlan() bool {
	_, ok := h.planInForce()
	return ok
}

// describedAddition names a handle the way an amendment card's own What
// already does — a declared title, or DescribeWithTitle's detokenized
// fallback — computed against the SAME surface and PermissionTitles the card
// beneath it was built from, rather than printing the handle's wire form.
//
// This is not re-deriving from the handle: it is the identical
// declared-title-or-fallback resolution BuildCard's described map performs
// for every line the card renders, applied here to the one handle an
// amendment is asking to add. An unresolvable handle falls back to its own
// String() — the same honest last resort Describe() itself uses when nothing
// on the surface matches.
func (h *PlanGate) describedAddition(handle permsurface.Handle) string {
	for _, d := range h.deps.Surface {
		if d.Handle != handle {
			continue
		}
		return d.DescribeWithTitle(h.deps.PermissionTitles[d.ResourceType+"/"+d.Permission])
	}
	return handle.String()
}

// raiseAmendment turns a justified retry into a request a human can act on.
//
// Under enforcing it returns an ApprovalAsk and the executor publishes it and
// blocks. Under logging the identical card is built and recorded and NOBODY is
// asked — the same contract every other card in this feature follows.
func (h *PlanGate) raiseAmendment(
	ctx context.Context, in pipeline.Input, rec plangateaudit.Content, activePhase int,
) pipeline.Decision {
	handle, _ := h.deps.Resolve(in.Tool.Name, argsMapOf(in))

	sev := plangate.Classify(plangate.Request{
		Kind:              plangate.KindAmendment,
		HasExternalHandle: h.hasExternal(rec.Handle),
		// The agent has necessarily consumed tool output by now — it was denied
		// on a call it chose to make — so a widening proposed here is a
		// post-exposure one.
		AfterFirstToolResult: true,
		AddsExternalReach:    h.hasExternal(rec.Handle),
		EnvelopeRecorded:     false,
	})

	card := plangate.BuildCard(plangate.CardInput{
		Plan:                 h.activePlan(),
		PhaseIndex:           activePhase,
		Severity:             sev,
		Surface:              h.deps.Surface,
		Approvers:            h.deps.Approvers,
		MaxSingleCardHandles: h.deps.MaxSingleCardHandles,
		AddedHandles:         []string{rec.Handle},
		AgentJustification:   in.Tool.Justification,
		SlotStanding:         h.deps.SlotStanding,
		SlotPermissions:      h.deps.SlotPermissions,
		SlotValueTransforms:  h.deps.SlotValueTransforms,
		PermissionTitles:     h.deps.PermissionTitles,
		ResourceDisplays:     h.deps.ResourceDisplays,
	})

	rec.Event = plangateaudit.EventAmendmentRequested
	rec.Severity = string(sev)
	if js, err := card.JSON(); err != nil {
		h.logf("plan_gate: rendering the amendment card failed",
			"session", in.Session.String(), "tool", in.Tool.Name, "err", err.Error())
	} else {
		rec.CardJSON = js
	}

	if !h.enforcing() {
		// Logging: recorded, nobody asked, call proceeds.
		h.write(ctx, in, rec)
		return pipeline.Decision{}
	}

	rec.Outcome = plangateaudit.OutcomeDenied
	h.write(ctx, in, rec)

	// The reach an approved amendment would leave the phase holding: what it
	// already has, plus the addition. Recording the union is what lets a later
	// re-plan that declares the amended handle carry the approval forward rather
	// than asking the same person the same question again.
	plan := h.activePlan()
	authority := plangate.PhaseAuthorityRecord(plan, activePhase, h.deps.SlotStanding)
	amended := map[permsurface.Handle]struct{}{handle: {}}
	if activePhase >= 0 && activePhase < len(plan.Phases) {
		for _, hh := range plan.Phases[activePhase].Permissions {
			amended[hh] = struct{}{}
		}
	}
	reach := make([]string, 0, len(amended))
	for hh := range amended {
		reach = append(reach, hh.String())
	}
	sort.Strings(reach)

	// ALLOW for the same reason as requestPhaseApproval: the executor turns a
	// refusal into Deny, and a YES must let the call through — otherwise the
	// human approves an amendment that changes nothing.
	return pipeline.Decision{
		Verdict: pipeline.Allow,
		Reason:  fmt.Sprintf("plan gate: adding %s to your plan was not approved.", handle.String()),
		Approval: &pipeline.ApprovalAsk{
			Kind: "plan_amendment",
			// COMPUTED, never the agent's words. ApprovalAsk.Summary is
			// documented as already injection-safe, and the agent's reasoning
			// rides in the payload as its claim — the same trust split the card
			// enforces between What and Why.
			//
			// Names the permission the same way the card's own What already does
			// (see describedAddition) rather than printing handle.String()'s wire
			// form — an amendment lead is read by the same human as the other two
			// leads, and it must state plainly that this is asking to ADD reach
			// beyond what the plan already holds.
			Summary: fmt.Sprintf("The agent is asking to add %s to its approved plan.",
				h.describedAddition(handle)),
			// One decision per (phase, addition). A turn's tool calls run
			// concurrently, so several can want the SAME addition at once and
			// each used to raise its own card — observed live as two identical
			// amendment prompts in the same second.
			//
			// The handle is IN the key on purpose: two calls wanting DIFFERENT
			// additions are two decisions, and merging them would let one click
			// grant authority the approver never separately considered.
			CoalesceKey: fmt.Sprintf("amend:%s#%d+%s", plan.Digest(), activePhase, handle.String()),
			Payload: map[string]any{
				"card":       card,
				"handle":     handle.String(),
				"severity":   string(sev),
				"planDigest": plan.Digest(),
				"phase":      activePhase,
				"ceiling":    reach,
				"maxCount":   authority.MaxCount,
				"requires":   authority.Requires,
				"slots":      authority.Slots,
			},
		},
	}
}

// phaseNeedsApproval reports whether the active phase is declared but not yet
// cleared to run, and returns the folded state for the caller to reuse.
//
// Only applies to a plan the AGENT declared. The synthesized fallback needs no
// approval — requiring one would deadlock every enforcing session whose agent
// has not planned yet, asking a human to approve a ceiling nobody authored.
func (h *PlanGate) phaseNeedsApproval(st plangate.State, folded bool, activePhase int) (needs, denied bool) {
	// Approval is an ENFORCING concern. Under logging the call proceeds either
	// way, and short-circuiting here would replace the gate's own outcome in the
	// record — corrupting the dataset with a card where an allow/would_deny
	// belongs. The tier is already recorded per phase, so "this would have
	// needed a human" stays derivable without a second record.
	if !h.enforcing() {
		return false, false
	}
	if !h.hasDeclaredPlan() {
		return false, false
	}
	if h.deps.Records == nil {
		// A declared plan with no log to read approvals from. Treating this as
		// "approved" would fail OPEN — absence of evidence that a human cleared
		// the phase is not evidence that they did — so it needs approval, and
		// the missing log source is loud because it is a wiring fault, not an
		// agent one.
		h.logf("plan_gate: a plan is declared but no audit log is readable; " +
			"phase approval cannot be established")
		return true, false
	}
	if !folded {
		// An unreadable log cannot establish that a human cleared this phase,
		// and absence of that evidence is not evidence of approval.
		return true, false
	}
	ref := plangate.PhaseRef{PlanDigest: h.activePlan().Digest(), Index: activePhase}
	// Denial FIRST, and the order is load-bearing. A refusal outlives the plan
	// it was made against, so it has to be consulted before any carry-over —
	// otherwise re-planning around a "no" would launder it into a yes.
	if st.IsDenied(ref) {
		return true, true
	}
	if st.PhaseApproved(activePhase) {
		return false, false
	}
	// Not approved under THIS digest. Any edit to any phase re-keys the whole
	// plan, so a re-plan that merely dropped reach arrives here looking exactly
	// like a brand-new ceiling. Ask whether a human's earlier answer already
	// covers it before spending another click.
	if plangate.CarriesOver(h.activePlan(), activePhase, h.deps.Records()) {
		h.logf("plan_gate: phase %d carries an earlier approval forward (it widens nothing)", activePhase)
		return false, false
	}
	return true, false
}

// addedSinceApproved returns the handles this phase holds beyond the most
// recent version of it a human approved, newest prior plan first.
//
// One card, not N: every addition a re-plan makes lands in a single decision.
// The alternative — which is what happens without this — is that the agent is
// denied at dispatch on the first new handle, asks about it, then is denied
// again on the second, and a widening of five handles costs five interruptions.
//
// Returns nil when nothing earlier is comparable, which makes the card state the
// full ceiling. That is right for a first approval: all of it is new.
func (h *PlanGate) addedSinceApproved(
	plan plangate.Plan, activePhase int,
) (handles []string, slots []plangate.Slot) {
	if h.deps.Records == nil || activePhase < 0 || activePhase >= len(plan.Phases) {
		return nil, nil
	}
	records := h.deps.Records()
	// The phase as it was GRANTED, not as the prior plan declared it: a handle
	// the approver held back is new authority again when the agent re-declares
	// it, and a card that diffed against the declaration would hide exactly the
	// item the human refused.
	granted, ok := plangate.MostRecentApprovedPhase(records, activePhase, plan.Digest())
	if !ok {
		return nil, nil
	}
	d, changed := plangate.DiffPhase(granted, plan.Phases[activePhase], activePhase)
	if !changed {
		return nil, nil
	}
	out := make([]string, 0, len(d.Added))
	for _, hh := range d.Added {
		out = append(out, hh.String())
	}
	// Slot additions ride the SAME delta, WITH their instances. A re-plan that
	// adds only a slot changes no handle at all, so a caller reading handles
	// alone would find an empty delta and render the full ceiling — burying the
	// one line the approver is actually being interrupted for. And a re-POINT
	// adds no type either, so dropping the instance here left the card naming a
	// resource it had, moments earlier, been told exactly.
	return out, d.SlotsAdded
}

// requestPhaseApproval asks a human to clear the active phase before it runs.
//
// This is the feature's namesake: the ceiling an agent declared does not
// authorize anything until somebody says so. Without it the gate constrains the
// agent to its own words — real, but not the same claim.
func (h *PlanGate) requestPhaseApproval(
	ctx context.Context, in pipeline.Input, rec plangateaudit.Content, activePhase int, denied bool,
) pipeline.Decision {
	plan := h.activePlan()

	impact := map[permsurface.Handle]authz.StateImpact{}
	for _, d := range h.deps.Surface {
		impact[d.Handle] = d.StateImpact
	}
	var handles []plangate.HandleImpact
	if activePhase >= 0 && activePhase < len(plan.Phases) {
		for _, hh := range plan.Phases[activePhase].Permissions {
			handles = append(handles, plangate.HandleImpact{Handle: hh, StateImpact: impact[hh]})
		}
	}

	// What ONE answer to this card clears, and therefore whether the card shows
	// one phase or the plan.
	covered, wholePlan := coveredPhases(plan, activePhase, h.deps.SlotStanding)

	sev := plangate.Classify(plangate.Request{
		Kind:              plangate.KindPlanApproval,
		HasExternalHandle: plangate.StillNeedsPerCallApproval(plangate.TierInput{Handles: handles}),
		EnvelopeRecorded:  true,
	})
	// When this phase is a WIDENING of one a human already cleared, show the
	// delta rather than the whole ceiling. Re-reading a list they approved
	// yesterday to find the two lines that are new is how a card stops being
	// read at all; the additions are the decision.
	//
	// Empty means there is nothing earlier to diff against — a first approval —
	// and the card states the full ceiling, which is correct: all of it is new.
	added, addedSlots := h.addedSinceApproved(plan, activePhase)

	cardIn := plangate.CardInput{
		Plan: plan, PhaseIndex: activePhase, Severity: sev,
		Surface: h.deps.Surface, Approvers: h.deps.Approvers,
		MaxSingleCardHandles: h.deps.MaxSingleCardHandles,
		AddedHandles:         added,
		AddedSlots:           addedSlots,
		SlotStanding:         h.deps.SlotStanding,
		SlotPermissions:      h.deps.SlotPermissions,
		SlotValueTransforms:  h.deps.SlotValueTransforms,
		PermissionTitles:     h.deps.PermissionTitles,
		ResourceDisplays:     h.deps.ResourceDisplays,
	}
	if wholePlan {
		// Severity is left for the card to derive, and that is the whole point of
		// deferring it: the plan-scoped price is the WORST phase in the plan, not
		// the phase that happened to trigger the card. Passing the active phase's
		// severity here would render a plan that ends in a push as routine
		// because it starts with a read — which is the common case.
		cardIn.WholePlan = true
		cardIn.Severity = ""
	}
	card := plangate.BuildCard(cardIn)
	sev = plangate.Severity(card.Severity)

	rec.Event = plangateaudit.EventCardBuilt
	rec.Severity = string(sev)
	if js, err := card.JSON(); err == nil {
		rec.CardJSON = js
	}

	if !h.enforcing() {
		h.write(ctx, in, rec)
		return pipeline.Decision{}
	}

	rec.Outcome = plangateaudit.OutcomeDenied
	h.write(ctx, in, rec)

	label := "this phase"
	if activePhase >= 0 && activePhase < len(plan.Phases) && plan.Phases[activePhase].Label != "" {
		label = plan.Phases[activePhase].Label
	}

	if denied {
		// A refusal already happened. Re-asking would let an agent grind a human
		// into reversing a decision they made — so this is a plain denial with
		// no card attached.
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason: fmt.Sprintf("plan gate: %q was declined, so it cannot run. "+
				"Call update_plan to propose a different approach.", label),
		}
	}

	var reach []string
	for _, hi := range handles {
		reach = append(reach, hi.Handle.String())
	}
	sort.Strings(reach)

	// The active phase's own authority, which is what a DENIAL is recorded
	// against: refusing is refusing the call in front of the human, and the
	// denial has to be self-contained enough to survive the plan being reshaped
	// around it.
	active := plangate.PhaseAuthorityRecord(plan, activePhase, h.deps.SlotStanding)

	// leadCard carries just enough structure for planGateLead to read a
	// strongest-tier tag off — never the raw handles a wire-format join would
	// have spelled out. BuildCard leaves Phases empty in two cases: a
	// single-phase card (its WholePlan branch never runs), and an AMENDMENT —
	// including a whole-plan card that is really a widening, where AddedHandles
	// wipes Phases so the structured half doesn't contradict the delta prose
	// beside it (see BuildCard's AddedHandles branch). Both need a leadCard
	// synthesized here instead of reaching back into card.go; only a whole-plan
	// card that covers every phase in full already has the structure and can
	// use the built card as-is.
	leadCard := card
	if len(card.Phases) == 0 {
		src := handles
		if wholePlan {
			// A widening: the phrase must reflect what is actually being ADDED,
			// since that delta — not the full ceiling — is all the card's What
			// states.
			src = nil
			for _, raw := range added {
				h, err := permsurface.ParseHandle(raw)
				if err != nil {
					continue
				}
				src = append(src, plangate.HandleImpact{Handle: h, StateImpact: impact[h]})
			}
		}
		lines := make([]plangate.CardLine, 0, len(src))
		for _, hi := range src {
			lines = append(lines, plangate.CardLine{Impact: string(hi.StateImpact)})
		}
		leadCard = plangate.Card{Phases: []plangate.CardPhase{{Permissions: lines}}}
	}

	// COMPUTED, never the agent's words, and never a join of raw wire-format
	// handles (perm:read:tracker_issue, …) — that string is exactly what a human
	// approver cannot parse at a glance. The lead instead names the strongest
	// state-impact tier the card carries, so a plan that ends in an irreversible
	// push cannot render as if it only reads.
	summary := planGateLead(label, 1, leadCard)
	// ONE decision per phase, or per PLAN when the card is plan-scoped. A turn's
	// tool calls are dispatched concurrently, so several can reach the same
	// unapproved phase at once; each raised its own card, and the user clicked
	// twice for one decision.
	coalesce := fmt.Sprintf("phase:%s#%d", plan.Digest(), activePhase)
	if wholePlan {
		summary = planGateLead("", len(plan.Phases), leadCard)
		coalesce = fmt.Sprintf("plan:%s", plan.Digest())
	}

	// ALLOW, not Deny. The executor publishes this, awaits, and on a NO sets
	// Deny itself with the Reason below; on a YES it falls through with the
	// verdict here. Returning Deny alongside an Approval would mean an approved
	// phase is still refused — the approval could never take effect.
	return pipeline.Decision{
		Verdict: pipeline.Allow,
		Reason:  fmt.Sprintf("plan gate: %q was not approved, so it cannot run.", label),
		Approval: &pipeline.ApprovalAsk{
			Kind:        "plan_phase",
			Summary:     summary,
			CoalesceKey: coalesce,
			Payload: map[string]any{
				"card": card, "phase": activePhase, "severity": string(sev),
				"planDigest": plan.Digest(),
				// The phase's AUTHORITY key, captured here so the approval is
				// recorded against what the approver actually saw. Recomputing it
				// at resolution would key the decision to whatever the agent has
				// rewritten the plan into since.
				"phaseKey": active.PhaseKey,
				"ceiling":  reach,
				"maxCount": active.MaxCount,
				"requires": active.Requires,
				"slots":    active.Slots,
				// The INSTANCE each slot type named, so an approval can narrow to
				// it rather than merely permit the type. Types alone cannot narrow:
				// scope.Resources keys on (type, id).
				"slotValues": slotValuesOf(plan, activePhase),
				// Every phase this one answer clears, each already projected into
				// the record it will be written as. Carried rather than recomputed
				// at resolution for the same reason the fields above are: the agent
				// keeps rewriting its plan while a human reads the card.
				"covered": covered,
			},
		},
	}
}

// planGateLead is the first thing a human reads on an approval card. It is
// COMPUTED from the card's own structure and never joins raw wire-format
// handles (perm:read:tracker_issue, …) the way a card once did — that string
// is exactly what a human approver skims past, not what decides them.
//
// subject names the ask when phaseCount is 1 — a single phase's label, quoted
// as the agent declared it. Above 1 the card is plan-scoped and subject is
// unused: naming the first phase would understate a plan that ends in a push
// while it started with a read, so the lead names the plan itself instead.
func planGateLead(subject string, phaseCount int, card plangate.Card) string {
	ask := fmt.Sprintf("%q", subject)
	if phaseCount > 1 {
		ask = fmt.Sprintf("its %d-phase plan", phaseCount)
	}
	return fmt.Sprintf("The agent is asking to run %s, which %s.", ask, planGateTierPhrase(card.Phases))
}

// planGateTierPhrase names the strongest authz.StateImpact tier present
// across every line of every phase, so the lead cannot understate the ask by
// reporting a milder line that happens to come first. Nothing recognized
// (no phases, or a StillNeedsPerCallApproval-worthy line the surface never
// classified) renders at the floor, matching a card that carries no
// permissioned reach at all.
func planGateTierPhrase(phases []plangate.CardPhase) string {
	rank := map[authz.StateImpact]int{
		authz.Readonly:  0,
		authz.Readwrite: 1,
		authz.External:  2,
	}
	best, bestRank := authz.Readonly, -1
	for _, ph := range phases {
		for _, line := range ph.Permissions {
			impact := authz.StateImpact(line.Impact)
			if r, ok := rank[impact]; ok && r > bestRank {
				best, bestRank = impact, r
			}
		}
	}
	switch best {
	case authz.External:
		return "leaves this session and cannot be undone"
	case authz.Readwrite:
		return "changes state in this session"
	default:
		return "only reads"
	}
}

// coveredPhases decides what ONE answer to this card clears, and with it
// whether the card shows a phase or the plan.
//
// The whole plan needs two things to be true. It has to have more than one
// phase, or "the whole plan" and "this phase" are the same statement and the
// per-phase wording is the one every card already speaks. And the phase that
// RAISED the card has to be among the ones cleared — miss that and the human
// approves, the fold still finds the active phase unapproved, and the very next
// call raises the same card, forever.
//
// The second is what a deferred phase breaks. A phase naming no target is
// deliberately left out of a plan-scoped card's coverage, because its authority
// key covers the type and not the instance and pre-clearing it would authorize
// a resource nobody was shown. That is right for a phase two steps away and
// fatal for the one running now, so when the active phase is the deferred one
// this falls back to asking about that phase alone — which is honest: the card
// then describes exactly what the answer buys.
func coveredPhases(plan plangate.Plan, activePhase int, standing map[string]string) ([]plangateaudit.Content, bool) {
	if activePhase < 0 || activePhase >= len(plan.Phases) {
		return nil, false
	}
	active := plangate.PhaseAuthorityRecord(plan, activePhase, standing)
	if len(plan.Phases) < 2 || !plangate.PhaseFullySpecified(plan.Phases[activePhase]) {
		return []plangateaudit.Content{active}, false
	}

	var out []plangateaudit.Content
	for i, ph := range plan.Phases {
		if !plangate.PhaseFullySpecified(ph) {
			continue
		}
		out = append(out, plangate.PhaseAuthorityRecord(plan, i, standing))
	}
	return out, true
}

// slotValuesOf maps each of the phase's slot types to the instance it named.
// A deferred slot contributes nothing: there is no instance to record, and an
// empty id would narrow the type to nothing at all.
func slotValuesOf(plan plangate.Plan, index int) map[string]string {
	if index < 0 || index >= len(plan.Phases) {
		return nil
	}
	out := map[string]string{}
	for _, s := range plan.Phases[index].Slots {
		if s.ID != "" {
			out[s.Type] = s.ID
		}
	}
	return out
}

// budgetReason is what the agent is told when a phase runs out of budget.
//
// The remedy differs from every other denial this gate issues, and getting it
// wrong sends the agent in circles: select_phase cannot restore a spent budget,
// and a message that mentions it invites exactly the loop the budget exists to
// break. The only move that works is a re-plan — which is the point, because
// re-planning is where a human sees that the phase went long.
func (h *PlanGate) budgetReason(activePhase int) string {
	plan := h.activePlan()
	label := "this phase"
	spent := 0
	if activePhase >= 0 && activePhase < len(plan.Phases) {
		if l := plan.Phases[activePhase].Label; l != "" {
			label = strconv.Quote(l)
		}
		spent = plan.Phases[activePhase].Budget.Calls
	}
	return fmt.Sprintf(
		"plan gate: %s has used its budget of %d tool calls. Selecting another phase "+
			"will not restore it. Call update_plan to re-plan from here — say what this "+
			"phase achieved and what remains, and give the next phase the budget it needs.",
		label, spent)
}

// ApproversForTest exposes the resolved approver description.
//
// Test-only accessor, and it exists because the failure it guards is invisible
// from either side: the renderer can be perfectly correct and the reader
// perfectly correct while nothing joins them, which is the state this field was
// in until the runner started populating it. Asserting on the built hook is the
// only place that gap is observable.
func (h *PlanGate) ApproversForTest() string { return h.deps.Approvers }

// argsMapOf decodes a call's arguments for handle resolution, unwrapping the
// {operation_id,_reason,args} envelope the same way the authz hook does. A
// malformed body yields nil, which resolvers treat as "no per-call answer" and
// fall back to the tool's base handle.
func argsMapOf(in pipeline.Input) map[string]any {
	if in.Tool == nil || len(in.Tool.Args) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(in.Tool.Args, &m); err != nil {
		return nil
	}
	return unwrapEnvelope(m)
}
