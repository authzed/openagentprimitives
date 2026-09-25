package plangate

import "github.com/authzed/openagentprimitives/pkg/channels/channelevents"

// Severity is how dangerous an approval decision is. A card that reads
// differently must also LOOK different: identical chrome on a categorically
// different decision is how an injected agent gets a click.
type Severity string

const (
	Routine  Severity = "routine"
	Elevated Severity = "elevated"
	Severe   Severity = "severe"
)

func (s Severity) rank() int {
	switch s {
	case Elevated:
		return 1
	case Severe:
		return 2
	default:
		return 0
	}
}

// ApproveStyle is the rendering hint for the Approve action. It is always
// primary, at every severity.
//
// This reverses an earlier, deliberate decision, and the old reasoning is
// worth restating rather than deleting: Elevated and Severe used to override
// Approve to danger, on the argument that "the destructive act is granting,
// so Approve should be the red button" — and that a red Approve, unlike every
// routine card in the same thread, was exactly the discontinuity habituation
// erodes.
//
// The discontinuity is preserved. It moved. Elevation now rides the card's
// own rail, marker and external lines — where the reader is actually
// deciding what they are granting — instead of the confirm button. Colouring
// Approve red conflated "consequential" with "error": Approve answers a
// question, and a question is not a failure. It also collided with Deny,
// which is never danger-styled (see DenyStyle) — a red Approve beside a red
// Deny made the card shout in two directions and distinguished neither.
func (s Severity) ApproveStyle() channelevents.ActionStyle {
	return channelevents.ActionStylePrimary
}

// DenyStyle is the rendering hint for the Deny action. Deny is NEVER
// danger-styled on a plan card: a red Deny trains the reflex that refusing is
// the dangerous act, when the opposite is true.
func (s Severity) DenyStyle() channelevents.ActionStyle {
	return channelevents.ActionStyleDefault
}

// Marker is the Lead prefix for this severity. Empty for Routine.
//
// It exists so a channel kind with no styling affordance still carries the
// signal: colour degrades to text rather than disappearing.
func (s Severity) Marker() string {
	switch s {
	case Elevated:
		return "⚠️"
	case Severe:
		return "🛑"
	default:
		return ""
	}
}

// RequestKind is what the human is being asked to decide.
type RequestKind string

const (
	KindPlanApproval RequestKind = "plan_approval"
	KindSupersede    RequestKind = "supersede"
	KindExtraEntry   RequestKind = "extra_entry"
	KindAmendment    RequestKind = "amendment"
)

// Request is the STRUCTURAL description of an approval, and the only input to
// Classify.
//
// Every field is a fact the runtime computed — from the frozen plan, the
// recorded fold, and the surface. There is deliberately no field an agent
// authors: if a reviewer can point at one the agent controls, the classifier is
// wrong, because severity would then be forgeable by the party it warns about.
type Request struct {
	Kind RequestKind

	// HasExternalHandle: the request covers at least one `external` handle —
	// an irreversible side effect, re-approved per call.
	HasExternalHandle bool

	// BudgetSpent: the phase's max count is already exhausted.
	BudgetSpent bool

	// CeilingChanged: this supersede alters at least one phase ceiling.
	CeilingChanged bool

	// RestoresBudget: this supersede would return a spent budget to unspent.
	RestoresBudget bool

	// RewindsPhase: this supersede would move the active phase backwards.
	RewindsPhase bool

	// LeavesEnvelope: the proposal reaches beyond the pre-recon envelope.
	LeavesEnvelope bool

	// EnvelopeRecorded: the session recorded a pre-exposure envelope at all.
	EnvelopeRecorded bool

	// AfterFirstToolResult: the session has already consumed external text, so
	// anything proposed now was authored by an agent that has read untrusted
	// input.
	AfterFirstToolResult bool

	// AddsExternalReach: the proposal adds `external` handles it did not have.
	AddsExternalReach bool
}

// Classify computes a request's severity from structural facts alone.
//
// It evaluates every rule and returns the MAXIMUM, never the first match: a
// supersede that both changes a ceiling and leaves the envelope is Severe, not
// Elevated. Short-circuiting on the first hit would systematically under-price
// the compound cases, which are the dangerous ones.
func Classify(req Request) Severity {
	out := Routine
	raise := func(s Severity) {
		if s.rank() > out.rank() {
			out = s
		}
	}

	if req.HasExternalHandle {
		raise(Elevated)
	}
	if req.Kind == KindExtraEntry && req.BudgetSpent {
		raise(Elevated)
	}
	// An amendment is a WIDENING by definition: the agent is asking for reach
	// its approved plan does not hold. That is never a routine decision, however
	// mild the handle — a human is being asked to hand over something the plan
	// they already approved deliberately excluded.
	if req.Kind == KindAmendment {
		raise(Elevated)
	}
	if req.Kind == KindSupersede {
		if req.CeilingChanged || req.RestoresBudget || req.RewindsPhase {
			raise(Elevated)
		}
		// No envelope ⇒ no pre-exposure baseline to compare against, so every
		// supersede is treated as leaving it. Scoped to KindSupersede on
		// purpose: a FIRST plan has no pre-commitment it could have left, and
		// sweeping it up here would make every session's opening card Severe
		// and kill the signal on its first use.
		if req.LeavesEnvelope || !req.EnvelopeRecorded {
			raise(Severe)
		}
	}
	if req.AfterFirstToolResult && req.AddsExternalReach {
		raise(Severe)
	}

	return out
}
