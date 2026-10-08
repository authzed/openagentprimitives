package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// BindPreconditionWaiverHandler binds the SIDE-EFFECTING precondition_waiver
// decision handler — the human WAIVER card for a slot whose precondition (a CEL
// predicate over signed facts) Refused. Approving binds the slot grant anyway,
// which IS the waiver: authz.BindApproved deliberately skips its precondition
// filter, because the design's whole answer to a Refused verdict is that a
// person may consent to it from a card that explains what they consent to.
//
// It is a DISTINCT category from tool_approval, never a reused one: both write a
// slot grant on approve, so reusing tool_approval would conflate two different
// consents. Standing is enforced by the generic decision pipe FIRST (the
// DecideResourceOwners policy: the clicker must be an #owner of the refused
// resource) BEFORE this runs, which is what makes the unconditional grant write
// here safe.
//
// Only DEFINED here — it is Bound in internal/cmd/channelsd (Bind panics on an
// unregistered category, so the PreconditionWaiver row must be Registered
// first, which importing pkg/channels/channelinteractions/categories does).
func BindPreconditionWaiverHandler(p *Pipeline) {
	channelinteractions.Bind(categories.PreconditionWaiver, preconditionWaiverHandler(p))
}

// preconditionWaiverHandler returns the bound handler. On approve it binds a
// SLOT grant scoped to the approved instance (the same shape tool_approval
// writes) via authz.BindApproved; on deny it writes nothing and the runner
// denies its gate off the resulting interaction_applied.
func preconditionWaiverHandler(p *Pipeline) channelinteractions.DecisionHandler {
	mem := p.Mem
	return func(ctx context.Context, d channelinteractions.Decision) (channelinteractions.Outcome, error) {
		switch d.Payload.ActionID {
		case "deny":
			// Deny has no side effect — the runner denies the refused slot off the
			// resulting interaction_applied. No grant is written.
			return channelinteractions.Outcome{Result: channelevents.OutcomeDenied}, nil
		case "approve":
			// The waiver card carries the same grant fields as a tool_approval
			// card (ToolApprovalDetails), so the same resolver — cached request
			// fast path, durable memapproval fallback on a channelsd restart —
			// recovers them.
			det, err := resolveToolApprovalDetails(ctx, d, mem)
			if err != nil {
				return channelinteractions.Outcome{}, fmt.Errorf("precondition waiver decision: resolve grant details (requestRef %q): %w", d.Payload.RequestRef, err)
			}
			if p.Authz == nil {
				// Programming error: the pipeline was built without a slot-grant
				// writer. Fail loud (the pipe's D4 path surfaces this to the
				// clicker) rather than silently approving with no tuple written.
				return channelinteractions.Outcome{}, fmt.Errorf("precondition waiver decision: Authz not configured (programming error) (requestRef %q)", d.Payload.RequestRef)
			}
			if p.Mem == nil {
				// Programming error: authz.BindApproved reads/writes session_scope
				// unconditionally through p.Mem (a nil memory.Memory panics on the
				// first method call rather than erroring), so this must fail loud
				// here rather than let sessionscope.Get crash the handler.
				return channelinteractions.Outcome{}, fmt.Errorf("precondition waiver decision: Mem not configured (programming error) (requestRef %q)", d.Payload.RequestRef)
			}
			// Same expiry policy as tool_approval: bounded by the session horizon,
			// with external effects kept on the short 30s leash (re-approve per
			// call). A slot grant with no expiry is the leak the expiry prevents.
			expiry := authz.SlotGrantExpiry(p.Now(), sessionExpirationOf(ctx, p.K8s, d.Session))
			if det.StateImpact == "external" {
				expiry = p.Now().Add(30 * time.Second)
			}
			scp := memory.Scope{Kind: "session", ID: d.Session.Namespace + "/" + d.Session.Name}
			sess := authz.SessionRef{Namespace: d.Session.Namespace, Name: d.Session.Name}
			// BindApproved reads/writes session_scope through p.Mem, which (like
			// every memory.Memory caller) must clear the capability door on ctx —
			// the inbound decision ctx carries no approval of its own, so this
			// in-process write mints one, distinct from tool_approval's.
			bindCtx := memory.WithSystemApproval(ctx, "precondition_waiver")
			// PreconditionsWaived: approving THIS card is the consent to the
			// Refused verdict, so BindApproved must NOT re-check the gate — doing
			// so would refuse the very thing the human just cleared. This is the
			// one call site allowed to waive; every other passes
			// EnforcePreconditions.
			if err := authz.BindApproved(bindCtx, p.Mem, scp, sess, p.Authz.Relations(),
				[]authz.SlotBinding{{
					ResourceType: det.ResourceType,
					// TrustedObjectID: det.ResourceID was populated at raise time
					// from authz.ResolveResourceID, which already ran the check's
					// own transform chain before this was cached.
					ResourceID:      authz.TrustedObjectID(det.ResourceID),
					Permission:      det.Permission,
					NoGrantRelation: det.NoSlotGrant,
					// Resolved from the class slot at request-record time and
					// carried in the details, so the waiver's grant is pinned
					// exactly as any other bind of the same single-occupancy slot.
					Occupancy: det.Occupancy,
					Rebind:    det.Rebind,
				}}, authz.PreconditionsWaived, expiry, logr.FromContextOrDiscard(ctx), p.Now); err != nil {
				return channelinteractions.Outcome{}, fmt.Errorf(
					"precondition waiver decision: bind %s on %s/%s (requestRef %q): %w",
					det.ResourceType+":"+det.ResourceID, d.Session.Namespace, d.Session.Name,
					d.Payload.RequestRef, err)
			}
			return channelinteractions.Outcome{Result: channelevents.OutcomeApproved}, nil
		default:
			return channelinteractions.Outcome{}, fmt.Errorf("precondition waiver decision: unknown actionID %q (requestRef %q)", d.Payload.ActionID, d.Payload.RequestRef)
		}
	}
}
