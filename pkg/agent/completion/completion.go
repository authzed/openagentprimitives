// Package completion is the registry of COMPLETION REQUIREMENTS: conditions a
// session must satisfy before its agent may declare a round of work finished.
//
// # Not plans.Phase.Requires
//
// A completion requirement is the OTHER axis from
// pkg/agent/session/state/plans.Phase.Requires, and the two must not be read
// for each other. Requires gates phase ENTRY ordering, against the runtime's
// record of which phases were entered, and its own doc says it is "never on an
// agent's assertion that it finished something". A completion requirement is
// read at exactly that assertion: when agent_work_complete arrives, the gate
// asks the RUNTIME whether what the operator said a session of this class must
// produce actually happened. One orders work; the other refuses to call it
// done.
//
// # Registered, never branched on
//
// A new requirement is a package with a Register in its init(), not a case in
// the checker. Evaluate resolves every declared key through Get, so every kind
// shares one lookup, one error path and one refusal message.
//
// # The bypass is part of the design
//
// A requirement with no escape turns a degraded round into a wedged session —
// a failed artifact render is a degraded review, not a failed one. So the agent
// may proceed anyway, but only by stating a reason, and the reason is recorded
// and shown to a human. That is what keeps the bypass from being an off switch.
package completion

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// Requirement is one registered kind of completion requirement.
//
// Implementations are stateless: everything a Check reads comes from Input, so
// one registered value serves every session in the process.
type Requirement interface {
	// Key is the registry key an AgentClass declares in
	// spec.completionRequirements. Stable; lowercase-dash.
	Key() string

	// Title is the short human phrase naming what the requirement demands
	// ("every artifact you produced reaches the user"). It is USER copy — it
	// reaches a person on the bypass notice — so it must name the obligation in
	// the reader's terms and never an internal object.
	Title() string

	// Check reports whether this session satisfies the requirement right now.
	//
	// An error means the requirement could not be EVALUATED (a missing client,
	// state the session does not carry). It is not "unsatisfied": Evaluate
	// fails closed on it, so a requirement that cannot answer refuses
	// completion rather than waving it through.
	Check(ctx context.Context, in Input) (Finding, error)
}

// Input is what a Check reads. It carries the runner-side session handle rather
// than per-kind wiring so the registry stays the only thing a new kind has to
// touch: a kind reaches its own observation source through SessionContext
// (K8sClient, State) exactly as the meta tools do.
type Input struct {
	// Session is the per-session runner handle. Never nil in production; a
	// Check must return an error rather than panic when it is.
	Session *tool.SessionContext
}

// Finding is one requirement's verdict for one session.
type Finding struct {
	// Met reports satisfaction. When false, Missing must be non-empty.
	Met bool

	// Missing is MODEL-facing: one or two sentences naming exactly what is
	// absent and the call that would fix it. It is fed back as a tool_result,
	// so it may name concrete handles the model needs to act on — unlike Title,
	// which a person reads.
	Missing string
}

// Unmet is one declared requirement that a session did not satisfy.
type Unmet struct {
	// Key and Title identify the requirement; Missing is its model-facing
	// detail. Carried together so both the refusal (model) and the bypass
	// record (human) render from one value.
	Key     string
	Title   string
	Missing string
}

// Bypass is an agent's recorded decision to finish anyway.
type Bypass struct {
	// Reason is the agent's stated justification. AGENT-AUTHORED and
	// UNTRUSTED: a surface showing it to a person must render it inert.
	Reason string
	// Unmet is what was outstanding at the moment of the bypass.
	Unmet []Unmet
}
