package authz

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/authz/precondition"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// ExplainPreconditionDenial recomputes the slot-precondition verdict for the
// instance a DENIED tool call named, so the denial can tell the agent WHY the
// slot it acts on is empty rather than only that the call was refused.
//
// It returns nil for exactly two reasons: no gate is declared over the type, or
// every gate is Satisfied. nil leaves the checker's own Result exactly as it
// was, which is the unchanged behaviour every class that declares no
// precondition keeps. Anything else — including a gate it could not evaluate —
// comes back non-nil and is enforced.
//
// # It recomputes; it reads no cache, and it writes nothing
//
// A verdict is a pure function of durable, write-once facts and a declared
// expression. The bind-time filter (checkPreconditions) deliberately records
// nothing, and a cache here would be a second source of truth that can disagree
// with the first — invisibly, since the stored verdict would be what explains
// the denial while the live one is what caused it. Two fact reads on a call
// that is already being refused is not a cost worth that.
//
// # Why the denial, not the dispatch, is the trigger
//
// This adds no enforcement point. The slot is empty, so the ordinary
// authorization path has already refused the call with the ordinary audit
// record; all that is missing is the sentence saying which rule did it. Running
// only after a denial also means a satisfied gate costs nothing at all — the
// overwhelmingly common case reaches the first return below.
//
// # What it deliberately does NOT do
//
// It does not answer a VERDICT it could not compute. A nil memory, an id that
// would not resolve, an unreadable fact store and a predicate that fails only
// at eval are all logged and come back with PreconditionDenial.Unevaluatable
// set — never as Undetermined. "Could not look" is not a verdict, and reporting
// one would hand the agent an author's hint ("call get_pr_info first") for what
// is actually a broken store; that is precisely why the state is a separate
// field rather than a third Verdict value, and why each of the four branches
// writes its own sentence — a missing id is the agent's to fix, an unreadable
// store is not.
//
// What it does NOT do with that is let the call through. An unevaluatable
// result is a denial and the caller enforces it, EnforceAlways and all, because
// a gate that cannot be evaluated has to be treated as a gate that said no —
// the position pipeline.Decision.Definition already takes on this same shape.
// Returning nil here would be indistinguishable from "no gate is declared over
// this type", so under toolCalls.mode: permissive a transient fact-store
// failure on a GATED type would silently restore the bypass this function
// exists to remove.
func ExplainPreconditionDenial(
	ctx context.Context,
	mem memory.Memory,
	memScope memory.Scope,
	slots []BoundEntitySpec,
	check PermissionCheck,
	args map[string]any,
) *PreconditionDenial {
	rules := slotRules(slots, check.ResourceType)
	if len(rules) == 0 {
		// No gate over this type. The overwhelmingly common case, and the
		// regression guard for every class that predates preconditions: the
		// denial is returned untouched, with no fact read performed.
		return nil
	}
	logger := log.FromContext(ctx).WithName("authz.precondition_denial")

	if mem == nil {
		logger.Info("a denied call names a gated slot but no memory store is wired, so no precondition verdict could be recomputed; the call is refused as unevaluatable",
			"resourceType", check.ResourceType, "permission", check.Permission)
		return &PreconditionDenial{
			Unevaluatable: true,
			Message: "this call is gated on a precondition of " + check.ResourceType +
				", and no fact store is wired for this session, so the gate could not be evaluated." +
				" The call is refused rather than let past a gate nobody could read. Nothing this call does" +
				" will change that; the session is missing the store the precondition reads.",
		}
	}
	// The RAW id: facts are keyed by the provider identifier, before the
	// check's own transforms make it a legal SpiceDB object id.
	rawID, err := RawResourceID(check, args)
	if err != nil {
		logger.Info("a denied call names a gated slot but its resource id would not resolve, so no precondition verdict could be recomputed; the call is refused as unevaluatable",
			"resourceType", check.ResourceType, "permission", check.Permission, "err", err.Error())
		return &PreconditionDenial{
			Unevaluatable: true,
			// The error names the ARGUMENT that was missing or malformed, which
			// the model supplied itself — nothing here is new to it — and that
			// name is the only actionable half of this branch.
			Message: "this call is gated on a precondition of " + check.ResourceType +
				", and the call's own arguments do not say which instance it acts on (" + err.Error() +
				"), so the gate could not be evaluated. Re-issue the call with that argument supplied.",
		}
	}
	// The facade's data plane is capability-gated (memory.Local.Query checks
	// ReadMemory unconditionally), and the ctx a PreToolCall hook runs on
	// carries no approval of its own — nothing upstream of tool dispatch mints
	// one, which pkg/agent/tool/mcp's recordCtx says at length about the WRITE
	// half of this same fact plane. So this read mints its own, the way every
	// other in-process caller reaching the facade from outside a request handler
	// does (plangate/hold's tripper, the toolcall snapshotter, kg_ingestion's
	// OnSignal). WithApproval only ADDS to the set, so it can never revoke an
	// approval the caller already carried.
	//
	// The widening is bounded by what the values are used for: nothing read here
	// leaves this function. The verdict and the AUTHOR's sentence are all that
	// come back — never a fact value, which came out of a signed payload or a
	// tool result and can carry anything upstream returned.
	factCtx := memory.WithSystemApproval(ctx, "authz:precondition")
	subj, facts, err := candidateFacts(factCtx, mem, memScope, check.ResourceType, rawID)
	if err != nil {
		// LookupSubject's empty-id refusal lands here too, which is why the
		// message covers both: an empty id is a call that never said which
		// instance it meant, and the checker has already flagged that as
		// UnresolvedResource on its own route.
		logger.Info("a denied call names a gated slot but its facts could not be read, so no precondition verdict could be recomputed; the call is refused as unevaluatable",
			"resourceType", check.ResourceType, "permission", check.Permission,
			"rawResourceID", rawID, "err", err.Error())
		// The store's error stays in the log. It describes a backend, not the
		// call, so it gives the agent nothing to act on — and unlike the
		// id-resolution branch above it is not a restatement of what the model
		// already supplied.
		return &PreconditionDenial{
			Unevaluatable: true,
			Message: "this call is gated on a precondition of " + check.ResourceType + ":" + rawID +
				", and the facts recorded about that instance could not be read, so the gate could not be" +
				" evaluated. Either the call did not name a usable instance or the fact store is" +
				" unavailable; the call is refused rather than let past an unread gate.",
		}
	}
	// The SAME iteration the bind-time filter ran. Sharing it is what keeps the
	// rule that held the candidate and the rule the agent is told about the
	// same rule — see precondition.FirstUnsatisfied.
	u, held, err := precondition.FirstUnsatisfied(rules, facts, subj.ResourceType, subj.ResourceID)
	if err != nil {
		logger.Info("a denied call names a gated slot whose precondition could not be evaluated, so the call is refused as unevaluatable",
			"resourceType", check.ResourceType, "permission", check.Permission,
			"rawResourceID", subj.ResourceID, "err", err.Error())
		// The CEL error is a fault in the CLASS's declaration and is addressed
		// to whoever wrote it, so it is logged with the expression's own context
		// rather than handed to an agent that cannot act on it.
		return &PreconditionDenial{
			Unevaluatable: true,
			Message: "this call is gated on a precondition of " + check.ResourceType + ":" + subj.ResourceID +
				", and that precondition's own expression failed to evaluate, so the gate could not be" +
				" evaluated. The declaration is at fault, not this call: no action here can satisfy it," +
				" and its author has to fix the rule.",
		}
	}
	if !held {
		// Every precondition is Satisfied, so this denial is an ORDINARY
		// permission denial that a human can still act on. Claiming the gate
		// caused it would make an appealable refusal unappealable.
		return nil
	}
	// Approvers rides the denial so the waiver card (a Refused verdict only)
	// can route to the rule's declared risk-answerer. Carried from the SAME
	// rule FirstUnsatisfied stopped on, so the set the card routes to and the
	// message it shows describe one rule, never two.
	return &PreconditionDenial{
		Verdict:   u.Verdict,
		Message:   denialMessage(u, check.ResourceType, subj.ResourceID),
		Approvers: u.Rule.Approvers,
	}
}

// slotRules returns the declared preconditions for one resource type.
//
// Returns the FIRST matching slot's rules. A class declaring the same resource
// type twice is refused at admission, so there is no second slot to merge with,
// and merging silently across a duplicate would be a gate nobody wrote.
func slotRules(slots []BoundEntitySpec, resourceType string) []precondition.Rule {
	if resourceType == "" {
		return nil
	}
	for _, s := range slots {
		if s.ResourceType == resourceType {
			return s.Requires
		}
	}
	return nil
}

// denialMessage is what the agent is handed for an unsatisfied precondition.
//
// The author's own message is the whole point — it is the only part of this the
// agent can act on, and neither half can be derived from CEL. The fallback
// exists because an EMPTY message would be worse than a generic one: the agent
// would receive a denial with no reason at all, which is the silent-error shape
// this repo refuses everywhere else. Both message fields are required by the
// CRD (MinLength=1) and checked again at admission, so the fallback should be
// unreachable through a class the reconciler admitted — it is here for the
// class that predates that validation or was written past it.
func denialMessage(u precondition.Unsatisfied, resourceType, resourceID string) string {
	if m := u.Rule.Message(u.Verdict); m != "" {
		return m
	}
	// The expression is quoted rather than the fact VALUES: those come out of a
	// signed payload or a tool result and can carry anything upstream returned.
	return "this call is gated on a precondition of " + resourceType + ":" + resourceID +
		" which is " + u.Verdict.String() + " (" + u.Rule.Compiled.Expression() +
		"), and its author declared no message for that verdict"
}
