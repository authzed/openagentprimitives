package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	memapproval "github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// BindToolApprovalHandler binds the SIDE-EFFECTING tool_approval decision
// handler (grant-tuple write on approve) — the runner's post-approve re-check
// depends on the tuple this writes. Unlike content_inspection's pure
// ApprovalDecisionHandler, this one layers a grant write onto the approve path.
// Standing is enforced by the generic decision pipe (the DecideResourceOwners
// policy: the clicker must be an #owner of the tool's resource) BEFORE this
// runs. A grant-write failure surfaces as a returned error; the pipe then
// publishes a per-clicker render-error rejection (D4) and leaves the prompt
// parked so the approver can retry.
//
// Only DEFINED here — it is Bound in the next slice task, after the ToolApproval
// category is Registered (Bind panics on an unregistered category).
func BindToolApprovalHandler(p *Pipeline) {
	channelinteractions.Bind(categories.ToolApproval, toolApprovalHandler(p))
}

// toolApprovalHandler returns the bound handler. It matches the now-removed
// typed tool-approval decision handler's grant-write EXACTLY: on approve it
// writes the same grants.Grant tuple (SessionRef/Permission/ResourceType/
// ResourceID/ArgsHash, with a 30s TTL only for external-effect tools) via
// grants.WriteToolGrant; on deny it writes nothing. gw is the Pipeline's
// grants.Writer (the same SpiceDB writer the typed path used); mem is the
// durable memapproval reader for the cross-restart fallback.
func toolApprovalHandler(p *Pipeline) channelinteractions.DecisionHandler {
	mem := p.Mem
	return func(ctx context.Context, d channelinteractions.Decision) (channelinteractions.Outcome, error) {
		switch d.Payload.ActionID {
		case "deny":
			// Deny has no side effect — the runner denies its gate off the
			// resulting interaction_applied. No grant is written.
			return channelinteractions.Outcome{Result: channelevents.OutcomeDenied}, nil
		case "approve":
			det, err := resolveToolApprovalDetails(ctx, d, mem)
			if err != nil {
				return channelinteractions.Outcome{}, fmt.Errorf("tool approval decision: resolve grant details (requestRef %q): %w", d.Payload.RequestRef, err)
			}
			if p.Authz == nil {
				// Programming error: the pipeline was built without a slot-grant
				// writer. Fail loud (the pipe's D4 path surfaces this to the
				// clicker) rather than silently approving with no tuple written.
				return channelinteractions.Outcome{}, fmt.Errorf("tool approval decision: Authz not configured (programming error) (requestRef %q)", d.Payload.RequestRef)
			}
			if p.Mem == nil {
				// Programming error: authz.BindApproved reads/writes session_scope
				// unconditionally through p.Mem (a nil memory.Memory panics on
				// the first method call rather than erroring), so this must fail
				// loud here rather than let sessionscope.Get crash the handler.
				return channelinteractions.Outcome{}, fmt.Errorf("tool approval decision: Mem not configured (programming error) (requestRef %q)", d.Payload.RequestRef)
			}
			if det.ResourceType == "" || det.ResourceID == "" || det.Permission == "" {
				// A call that names no instance — a sidecar/workshop tool with no
				// resource handle (workshop_apply, observed live), an external
				// effect on nothing addressable — has no slot to bind: there is
				// no resource for a grant tuple to point at and no other instance
				// to exclude. The approval RECORD is the whole authorization (the
				// runner skips its post-approve re-check for exactly this shape),
				// so binding is skipped rather than attempted. GrantSlots refuses
				// an empty binding, rightly, and letting that refusal become the
				// decision's error abandoned an approval a human had already
				// given: the click was rejected and the session stayed parked.
				// Logged, never silent.
				logr.FromContextOrDiscard(ctx).Info("tool approval: the call names no instance; approval recorded with no slot grant",
					"session", d.Session.Namespace+"/"+d.Session.Name, "requestRef", d.Payload.RequestRef, "tool", det.ToolName)
				return channelinteractions.Outcome{Result: channelevents.OutcomeApproved}, nil
			}
			// Approving writes a SLOT grant — the tuple on the RESOURCE pointing
			// at the session — replacing the session-grant tuple this used to
			// write.
			//
			// The direction is the substance. The old tuple ran from the session
			// to the resource, so evaluating it meant checking the resource's
			// permission for the ORIGINAL user, who lacks it by construction —
			// that is why the approval flow ever needed a wildcard leaf
			// (`any_user`) in the resource's permission expression. Pointing the
			// tuple at the session instead resolves the SESSION's member set, so
			// the ordinary Check passes per-requester, honours `denied` for free,
			// and needs no wildcard anywhere.
			//
			// WHAT IS DELIBERATELY DROPPED: the check_hash caveat binding the
			// grant to one arguments hash. A slot grant authorizes an INSTANCE,
			// not one call shape. That is only sound because slot-grant relations
			// are PER-PERMISSION (authz.SlotGrantRelationName): a grant for
			// `read` cannot be used to exercise `write` on the same instance, so
			// approving a read of a repository cannot become a push to it. Take
			// that isolation away and the args binding would be load-bearing
			// again.
			expiry := authz.SlotGrantExpiry(p.Now(), sessionExpirationOf(ctx, p.K8s, d.Session))
			if det.StateImpact == "external" {
				// External effects keep their short leash: the approval covers
				// this moment's action, not the session.
				expiry = p.Now().Add(30 * time.Second)
			}
			// Record the scope entry AND grant. A binding a human cleared must
			// exclude the instances they did not clear; GrantSlots alone permits
			// this one and says nothing about the rest, which is how an approval
			// for one company came to be silently spendable on the next. What is
			// doing the excluding today is the grant's own instance-scoping — the
			// session_scope half is recorded and not yet read at dispatch, per
			// BindApproved's doc.
			scp := memory.Scope{Kind: "session", ID: d.Session.Namespace + "/" + d.Session.Name}
			sess := authz.SessionRef{Namespace: d.Session.Namespace, Name: d.Session.Name}
			// BindApproved reads/writes session_scope through p.Mem, which
			// (like every memory.Memory caller) must clear the capability door on
			// ctx — the inbound decision ctx carries no approval of its own, so
			// this in-process write mints one, the same way the plan-gate's
			// narrowToApproved (pkg/agent/runner/host_approval.go) does for its
			// own direct l.Mem call.
			bindCtx := memory.WithSystemApproval(ctx, "tool_approval")
			// logr.FromContextOrDiscard, not logr.Discard: bindSlots' nil-writer
			// branch returns SUCCESS having granted nothing, and says so only via
			// logger.Info (a grant failure similarly logs before returning its
			// error). Discarding both would mean an approval can report Approved
			// with no tuple actually written and no trace of why.
			if err := authz.BindApproved(bindCtx, p.Mem, scp, sess, p.Authz.Relations(),
				[]authz.SlotBinding{{
					ResourceType: det.ResourceType,
					// TrustedObjectID: det.ResourceID came from
					// channelevents.ToolApprovalDetails, populated at request time
					// from authz.ResolveResourceID, which already ran the check's
					// own transform chain (ApplyTransforms) before this was cached.
					ResourceID: authz.TrustedObjectID(det.ResourceID),
					Permission: det.Permission,
					// A type the class never declared as a slot has no
					// slot_grant relation to write. Attempting the grant fails
					// with FailedPrecondition, and that error propagating out of
					// BindApproved would abandon the approval a human just gave:
					// the tool would keep waiting for a decision that already
					// happened, then time out. The binding still narrows scope;
					// for `external` the human decision IS the authorization,
					// which is why the runner skips its post-approve re-check.
					NoGrantRelation: det.NoSlotGrant,
					// No Requires carried, so EnforcePreconditions is a no-op here.
					// It is right anyway: a Refused precondition routes to the
					// precondition_waiver card, never this tool_call one (see
					// toolcallauthz.explainPrecondition), so a tool_approval bind
					// can only ever cover a call whose gate was Satisfied or absent.
					//
					// Occupancy/Rebind were resolved from the class slot at
					// request-record time (the runner held the class; this handler
					// does not) and carried in the details, so GrantSlots pins this
					// approval's grant exactly as a non-approval bind of the same
					// slot would — a single-occupancy type binds through the pin,
					// not the unpinned plain write.
					Occupancy: det.Occupancy,
					Rebind:    det.Rebind,
				}}, authz.EnforcePreconditions, expiry, logr.FromContextOrDiscard(ctx), p.Now); err != nil {
				if errors.Is(err, authz.ErrSlotPinned) {
					// The tool_approval card is the escalation path for classes with
					// NO plan gate, so there is no re-plan route to move a filled
					// single-occupancy slot. The generic bind-error chain would hand
					// the clicker GrantSlots' "propose an updated plan" advice — a
					// dead end this class cannot follow. Return a clean refusal
					// (still wrapping ErrSlotPinned so the decision pipe routes it as
					// a pin refusal, not a render error) that names the target this
					// approval was for and the only real route: a new session.
					return channelinteractions.Outcome{}, fmt.Errorf(
						"%w: this session is already committed to a different %s, and this agent cannot move that commitment (it uses no plan) — start a new session to work on %s",
						authz.ErrSlotPinned, det.ResourceType, det.ResourceType+":"+det.ResourceID)
				}
				return channelinteractions.Outcome{}, fmt.Errorf(
					"tool approval decision: bind %s on %s/%s (requestRef %q): %w",
					det.ResourceType+":"+det.ResourceID, d.Session.Namespace, d.Session.Name,
					d.Payload.RequestRef, err)
			}
			return channelinteractions.Outcome{Result: channelevents.OutcomeApproved}, nil
		default:
			return channelinteractions.Outcome{}, fmt.Errorf("tool approval decision: unknown actionID %q (requestRef %q)", d.Payload.ActionID, d.Payload.RequestRef)
		}
	}
}

// resolveToolApprovalDetails reads the grant fields from the cached request's
// Details (fast path), falling back to the durable memapproval record on a
// channelsd restart (d.Request == nil) — the same authority the Show-Details
// modal and the resource-owner standing check read, preserving the
// restart-resilience the typed pending-tool-grant list used to provide.
func resolveToolApprovalDetails(ctx context.Context, d channelinteractions.Decision, mem memory.Memory) (channelevents.ToolApprovalDetails, error) {
	if d.Request != nil && len(d.Request.Details) > 0 {
		var det channelevents.ToolApprovalDetails
		if err := json.Unmarshal(d.Request.Details, &det); err != nil {
			return channelevents.ToolApprovalDetails{}, fmt.Errorf("decode cached tool-approval details: %w", err)
		}
		return det, nil
	}
	if mem == nil {
		return channelevents.ToolApprovalDetails{}, fmt.Errorf("no cached request and no durable memory reader")
	}
	rec, err := memapproval.RequestByID(ctx, mem, memory.Scope{Kind: "session", ID: d.Session.Namespace + "/" + d.Session.Name}, d.Payload.RequestRef)
	if err != nil {
		return channelevents.ToolApprovalDetails{}, fmt.Errorf("durable memapproval lookup: %w", err)
	}
	if rec == nil || len(rec.Details) == 0 {
		return channelevents.ToolApprovalDetails{}, fmt.Errorf("durable memapproval record missing details for requestRef %q", d.Payload.RequestRef)
	}
	var det channelevents.ToolApprovalDetails
	if err := json.Unmarshal(rec.Details, &det); err != nil {
		return channelevents.ToolApprovalDetails{}, fmt.Errorf("decode durable tool-approval details: %w", err)
	}
	return det, nil
}

// sessionExpirationOf reads the session's wall-clock lifetime cap, which bounds
// how long an approved slot grant may live.
//
// Best-effort: a session that cannot be read yields zero, and
// authz.SlotGrantExpiry turns zero into its own bounded default rather than
// "no expiry". Failing the approval over a status read would be the wrong
// trade — the human already decided, and a conservatively-short grant costs at
// most a re-approval.
func sessionExpirationOf(ctx context.Context, k8s client.Client, ref channelevents.SessionRef) time.Duration {
	if k8s == nil {
		return 0
	}
	var sess spiceboxv1alpha1.AgentSession
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &sess); err != nil {
		return 0
	}
	if sess.Status.EffectiveSettings == nil {
		return 0
	}
	return sess.Status.EffectiveSettings.Budget.SessionExpiration.Duration
}
