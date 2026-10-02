// Package plangateaudit is the memory Kind for the plan gate's authorization
// record: which plan was approved, which phase was active, and what the gate
// decided for each permissioned call.
//
// The records are the plan gate's ONLY durable state. Approval state, spent
// budgets, and denials are replayed by folding this log — never held in runner
// memory — so a runner restart, an idle-sleep re-hydrate, or a fork cannot
// silently re-ask a human who already decided, nor resurrect authority a human
// revoked. That is why the kind is append-only.
//
// Under planGate.mode=logging the gate runs end-to-end but publishes nothing
// and blocks nothing; the fully-built approval card is recorded here (CardJSON)
// instead of being sent to a channel. Those records are the dataset that decides
// whether phase-shaped approval is viable before enforcement is switched on.
//
// Queried via `ap memory query`.
package plangateaudit

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// Event values. The fold replays in record order and dispatches on these.
const (
	EventPlanApproved  = "plan_approved"
	EventPhaseSelected = "phase_selected"
	EventGateAllowed   = "gate_allowed"
	EventGateWouldDeny = "gate_would_deny"
	// EventGateDenied is the GATE refusing a call itself — today, requirePlan
	// with no plan declared. Deliberately NOT EventDenied: that one means a
	// HUMAN refused a phase, and the fold treats it as sticky authority that
	// outlives replanning (deniedRefs / deniedCeilings). Recording a mechanical
	// "you have not planned yet" as a human's "no" would poison the fold with a
	// refusal nobody made, and one the agent could never clear by replanning.
	EventGateDenied = "gate_denied"
	EventCardBuilt  = "card_built"
	EventDenied     = "denied"
	EventSuperseded = "superseded"
	// EventAmendmentRequested is a JUSTIFIED retry after a denial: the agent
	// asked for reach the plan lacks and said why, so a human decides.
	EventAmendmentRequested = "amendment_requested"
	// EventPhaseApproved records that a phase is CLEARED TO RUN — either
	// auto-approved at tier 0 or approved by a human.
	//
	// Distinct from EventPlanApproved, which records only that a phase was
	// frozen and recorded. Declaring a ceiling and being allowed to use it are
	// different facts, and collapsing them would make an agent's own
	// declaration self-approving.
	EventPhaseApproved = "phase_approved"
	// EventPhaseCompleted records that the agent DECLARED a phase finished.
	//
	// The agent has always declared completion — it sets item status. What this
	// event changes is WHERE the declaration lives: in the append-only log,
	// ordered and monotonic, rather than in a document update_plan resubmits
	// wholesale on every call. A field in that document can be flipped back; a
	// record cannot.
	//
	// It NARROWS only. A requires edge already needed prior ENTRY, which the
	// runtime records; completion is entry AND this declaration, so a false
	// claim lands the agent exactly where the entry rule already put it.
	EventPhaseCompleted = "phase_completed"
	// EventEnvelope is the pre-recon BASELINE: the reach the agent expected
	// before its first tool call, recorded once and never approved.
	EventEnvelope = "envelope"
	// EventApprovalsCleared drops every standing phase approval, in whatever
	// form it was recorded — the legacy (digest, index) form and the modern
	// authority-key form alike. Written ONLY by the operator, when a human
	// releases a forensic hold: reintegration must not hand a released agent
	// the ceiling it held when it was frozen.
	//
	// Distinct from EventSuperseded, which is a FLOOR and deliberately preserves
	// approvals while restoring budgets. Collapsing the two would make release a
	// no-op on reach.
	EventApprovalsCleared = "approvals_cleared"
)

// Outcome values for the per-call gate events.
const (
	OutcomeAllow     = "allow"
	OutcomeWouldDeny = "would_deny"
	// OutcomeDenied is an ENFORCED refusal — the call did not run. Distinct
	// from would_deny, which is logging mode recording a counterfactual, so a
	// dataset can never blend the two.
	OutcomeDenied = "denied"
)

// SlotRef is one slot request: the resource type, and the instance the phase
// named if it could name one. An empty ID is a real value — the phase declared
// the type and deferred the target, which the card renders as unnamed and the
// approval declines to pre-clear.
type SlotRef struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`

	// Standing records HOW this slot's approval got its authority:
	// v1alpha1.StandingSessionOnly (a human vouched) or StandingRequired (a
	// human delegated something they already held). Without it the two are
	// indistinguishable in the log, and `ap audit verify` cannot tell them
	// apart afterwards.
	//
	// Additive: empty on records written before this field existed, which the
	// append-only contract requires — see TestContent_StandingIsAdditive. Not
	// pkg/apis/v1alpha1-typed here because this package must stay decoupled
	// from the CRD shape (see pkg/authz/hooks/activation.go); callers compare
	// against the same string constants.
	Standing string `json:"standing,omitempty"`

	// MovedFrom, when non-empty, is the instance this slot was CURRENTLY pinned
	// to when the approval card was built — recorded ONLY when the card named a
	// DIFFERENT instance of a single-occupancy slot, i.e. when approving this
	// card MOVES a filled slot rather than filling an empty one. It is the
	// already-canonical object id read back from the pin (SlotPinner.ReadPin),
	// not the raw value the phase declared, so the decision path can derive the
	// move's PriorID from it directly.
	//
	// It is written at CARD-BUILD time, off an advisory read, so the human
	// approves exactly the move they were shown: a card parked for days executes
	// the move it displayed, never one the pin silently drifted into. Empty is
	// the overwhelmingly common first-fill case — an empty slot, a
	// multi-occupancy slot (which holds no pin), or a card built with no pinner
	// wired. Additive, like Standing: a record written before this field existed
	// unmarshals with it empty, which reads as "no move" everywhere downstream.
	MovedFrom string `json:"movedFrom,omitempty"`
}

// Content is one plan-gate record.
type Content struct {
	Event string `json:"event"`

	// PlanDigest identifies the frozen approved plan this record belongs to.
	// It covers the ordered phase list and each phase's authority (permissions,
	// max count, requires edges) — never the agent's `why` narration, so
	// re-wording a justification does not read as a new plan.
	PlanDigest string `json:"planDigest,omitempty"`

	// PhaseIndex is the ACTIVE phase. A pointer, because phase 0 is the
	// overwhelmingly common case and must stay distinguishable from "no phase
	// recorded" — with a plain int32 the two are the same serialized bytes.
	PhaseIndex *int32 `json:"phaseIndex,omitempty"`

	// PhaseKey identifies the approved phase by its AUTHORITY — the permissions
	// it holds and the instances it names — rather than by its position in a
	// particular plan. See plangate.Phase.AuthorityKey.
	//
	// It is on the RECORD rather than derived at fold time because a fold only
	// holds the CURRENT plan: a record written against an earlier digest refers
	// to a phase that may no longer exist, so its key cannot be recomputed.
	//
	// Empty on records written before this field existed. The fold falls back to
	// (PlanDigest, PhaseIndex) for those — this log is append-only, so a session
	// upgraded mid-run must not lose approvals it already holds.
	PhaseKey string `json:"phaseKey,omitempty"`

	Handle string `json:"handle,omitempty"` // permsurface handle, on per-call events
	Tool   string `json:"tool,omitempty"`
	UseID  string `json:"useID,omitempty"`

	// DeniedCeiling and Envelope travel only on a FORK ROOT: the derived record
	// a child session starts from. Denials must survive a fork or forking
	// becomes a laundering step; the envelope must survive it because the task
	// continues and re-deriving a baseline post-exposure would defeat its point.
	// MaxCount and Requires complete the phase-level record so the frozen plan
	// can be reconstructed EXACTLY from the log. Without them a rebuilt plan
	// digests differently from the recorded PlanDigest, and a fold against it
	// discards every record as belonging to another plan.
	MaxCount int   `json:"maxCount,omitempty"`
	Requires []int `json:"requires,omitempty"`

	// BudgetCalls is the phase's governed-call budget.
	//
	// Recorded for the same reason MaxCount, Requires and Slots are: the plan
	// digest covers it, so a plan rebuilt without it digests differently from
	// the digest these very records name, and the fold discards all of them as
	// another plan's. That coupling has now been broken twice — once for slots,
	// once here — because adding a dimension to the digest is silent until
	// something tries to reconstruct.
	BudgetCalls int `json:"budgetCalls,omitempty"`

	// Slots are the resource TYPES the phase asked to touch — the instance
	// axis's half of its authority.
	//
	// Recorded for the same reason MaxCount and Requires are: the plan digest
	// covers slot requests (approving one writes a SpiceDB grant), so a plan
	// rebuilt without them digests differently from the digest these very
	// records name, and the fold then discards all of them as another plan's.
	Slots []string `json:"slots,omitempty"`

	// SlotRefs are those same slot requests WITH the instance each one names.
	//
	// Separate from Slots rather than replacing it: this log is append-only, so
	// every record already written speaks the type-only spelling and has to keep
	// folding as it always did. A reader that finds SlotRefs prefers it and
	// falls back to Slots, which is exactly what an older record meant — that
	// type, no instance, because none could be named yet.
	//
	// It exists because a slot's id is authority and enters the plan digest. A
	// plan rebuilt from a type-only log therefore digested differently from the
	// digest its own records named, every fold discarded the lot, and a restart
	// silently lost the session's whole gate state. Third time this coupling has
	// broken; see plangate.PhaseAuthorityRecord, which is now the one projection.
	SlotRefs []SlotRef `json:"slotRefs,omitempty"`

	DeniedCeiling []string `json:"deniedCeiling,omitempty"`
	Envelope      []string `json:"envelope,omitempty"`

	// DelegatedFrom names the PARENT session a derived root was projected from,
	// on that root and nowhere else. Empty on every record a session wrote for
	// itself.
	//
	// It is a field rather than something read out of Provenance, which already
	// says "delegated from <parent> phase(s) …" in prose, because a real
	// decision hangs off it: a child holding an inherited ceiling must run its
	// plan gate even when its own class resolved the gate to disabled, or the
	// ceiling is inert data and the delegation hands the child MORE reach than
	// the parent had. A control that turns on by parsing a sentence is one
	// re-wording away from turning off.
	DelegatedFrom string `json:"delegatedFrom,omitempty"`

	// Ceiling is the phase's full handle set, recorded on PLAN-level events
	// (approval, denial, supersede).
	//
	// A denial has to carry its own ceiling rather than point at one. Denials
	// must outlive the plan they were made against — a human's "no" is not
	// undone by the agent reshaping its plan — so the fold has to know WHAT was
	// refused without being able to look up a plan it no longer holds. Storing
	// the handles makes a denial self-contained and lets it be matched by
	// intersection against any later plan.
	Ceiling []string `json:"ceiling,omitempty"`

	Outcome  string `json:"outcome,omitempty"`
	Severity string `json:"severity,omitempty"` // routine|elevated|severe
	Tier     string `json:"tier,omitempty"`

	// PhaseCount and HandleCount are recorded on PLAN-level events so breadth
	// is measurable from the log without re-deriving it from a plan the reader
	// may no longer have. They answer "how wide was what we approved?" — the
	// question the tier gradient exists to price.
	PhaseCount  int `json:"phaseCount,omitempty"`
	HandleCount int `json:"handleCount,omitempty"`

	// PhaseOutcome is what the agent says a phase achieved, carried on a
	// phase_completed record. Agent-authored and UNTRUSTED — it is audit and
	// display material, never an input to any decision. It exists because a
	// completion nobody can read is a worse trail than no completion at all.
	PhaseOutcome string `json:"phaseOutcome,omitempty"`

	// CardJSON is the fully-rendered approval card. Under logging it is
	// recorded INSTEAD of being published; under enforcing it is recorded
	// alongside the publish so the record shows exactly what a human saw.
	CardJSON string `json:"cardJSON,omitempty"`

	// Mode is what actually ran. Deliberately NOT omitempty: "would_deny" means
	// something entirely different under logging (nothing happened) than under
	// enforcing (the call was refused), so a record that omits its mode cannot
	// be interpreted.
	Mode string `json:"mode"`

	Provenance string    `json:"provenance"`
	At         time.Time `json:"at"`
}

type Kind struct{}

func (Kind) Name() string     { return "plan_gate_audit" }
func (Kind) IDPrefix() string { return "pgaud-" }

// WriteAuthority: the runner records its own plan-gate decisions
// (gate/approve/deny/carryover) as it makes them.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }

func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 90 * 24 * time.Hour,
		AppendOnly:      true, // the gate's durable authorization state — write-once.
		// NEVER copied on fork. These records are a chain: their meaning comes
		// from order and from the per-(scope, publisher) provenance sequence, and
		// every rule built on them — "a denial survives a later supersede", "the
		// last write wins for this ceiling" — is order-dependent. Copying them
		// into a child scope would produce a history the child does not have, and
		// a mis-ordered replay could resurrect authority a human revoked. The fork
		// path folds the parent to its terminal state and writes ONE derived
		// record instead; see plangate.DeriveForFork.
		NeverForkCopy: true,
	}
}

func (Kind) ContentSchema() reflect.Type { return reflect.TypeOf(Content{}) }

// IndexedFields covers what the two readers actually query: the fold dispatches
// on Event, and the logging-mode dataset aggregates by Outcome and Tool.
func (Kind) IndexedFields() []string { return []string{"at", "tool", "event", "outcome"} }

func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
