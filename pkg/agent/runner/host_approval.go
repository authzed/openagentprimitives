package runner

// host_approval.go implements pipeline.Host.PublishApproval and AwaitDecision
// for the runnerHost, so approval orchestration can be driven by the
// pipeline.Executor.
//
// PublishApproval builds the correct channel envelope by ask.Kind, registers
// the request with l.Approval, and returns the requestID. AwaitDecision blocks
// via l.Approval.Await and runs the kind-specific post-approval effect:
//   - tool_call: post-approval re-check + one-shot external grant cleanup.
//   - leakage_share: SpiceDB grant write on approve; denial callback on deny.
//   - content_inspection: none — the approved bool alone gates the tool I/O.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/authz/precondition"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	memapproval "github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiaction"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/authzed/openagentprimitives/pkg/x/besteffort"
	"github.com/authzed/openagentprimitives/pkg/x/stringsx"
)

// pendingApproval carries per-request state that the host needs to run the
// post-approval effect. Stored in the host's map between PublishApproval and
// AwaitDecision within a single executor.Run call.
type pendingApproval struct {
	kind      string
	onPublish func(ctx context.Context) error
	expiresAt *time.Time

	// tool_call fields
	toolCallPayload *pendingToolCall

	// leakage_share fields
	leakagePayload *pendingLeakageShare

	// content_inspection fields
	contentInspectionPayload *pendingContentInspection

	// plan_phase / plan_amendment fields
	planGatePayload *pendingPlanGate
}

// pendingPlanGate carries what the plan gate's decision has to be RECORDED
// against, captured at publish time.
//
// Captured then, not read back at resolution, and the difference is the point:
// the agent keeps rewriting its working plan while a human reads the card, so
// resolving the ceiling after the answer arrives would attribute the decision
// to a set the approver never saw. These fields are the reach that was actually
// put in front of them.
type pendingPlanGate struct {
	planDigest string
	phaseIndex int
	// phaseKey identifies the phase by its AUTHORITY, captured at publish for
	// the same reason ceiling is: it must describe what the approver saw, not
	// what the agent has rewritten the plan into since.
	phaseKey string
	// ceiling is the APPROVED SUBSET on a yes and the REFUSED set on a no —
	// the same field either way, because the record's meaning comes from its
	// event, and both readings need the handles to be self-contained.
	ceiling []string
	// handle is the ONE permission an amendment asks to add. Empty on a plan or
	// phase ask, which carry a whole ceiling instead. Slot binding reads it
	// because a grant is written per (instance, permission): an amendment adding
	// `read` must bind read, not whatever the slot was declared for.
	handle   string
	maxCount int
	requires []int
	// slots are the resource TYPES the phase asked to touch. Recorded as the
	// approved subset on a yes and as the refused set on a no, for the same
	// reason ceiling is.
	slots []string

	// slotValues maps a slot's resource type to the INSTANCE the card showed.
	// Types alone cannot narrow: scope.Resources keys on (type, id), and a
	// narrowing with no id would exclude everything of that type including the
	// resource just approved.
	slotValues map[string]string

	// covered is every phase this ONE answer clears, each already projected
	// into the record it will be written as.
	//
	// A yes writes one record per entry, because approval is keyed on a phase's
	// authority: a plan-scoped card that wrote a single record would clear
	// exactly one phase, and the human would be asked again at the next phase
	// boundary having already said yes to it. Per phase rather than one record
	// with a merged ceiling, because a record naming phase 0 while carrying
	// phase 2's reach reads back — at the next carry-over — as "phase 0 was
	// granted the push".
	//
	// The fields above stay the ACTIVE phase's, and a no still writes one
	// denial: refusing refuses the call in front of the human, not two phases
	// nobody has reached.
	covered  []plangateaudit.Content
	consents []json.RawMessage
}

// pendingContentInspection carries per-request state for a content_inspection
// approval. The host resolves the approver from the session reference at
// build time (host-side resolution, option a) so pkg/authz/contentguard stays
// decoupled from the approver model.
type pendingContentInspection struct {
	sessNS   string
	sessName string
}

type pendingToolCall struct {
	sessNS   string
	sessName string
	toolName string
	argsMap  map[string]any
	argsHash string
	perm     authz.Permission
	approver string
	useID    string // for memapproval audit; "" when not yet wired
	// subject + subjects are the PER-CALL principal the post-approval
	// post-approval re-verify (toolCallPostApprove) checks against: the
	// widget VIEWER on a proxy-exec (app-tool) approval (D-D1), threaded here so
	// the re-check (a) attributes to the right principal and (b) — on the
	// detached goroutine — never reads the loop-mutated l.authSubject. Both empty
	// on the LLM path, where toolCallPostApprove falls back to
	// l.authSubject/l.authSubjects (unchanged).
	subject  string
	subjects []string
}

type pendingLeakageShare struct {
	sessNS     string
	sessName   string
	leakedTo   []string
	taint      []leakageTaintRecord
	ttl        time.Duration
	onApproved func(resourceType, resourceID string) // callback to InfoLeakAudience.RecordApproved
	onDenied   func(resourceType, resourceID string) // callback to InfoLeakAudience.RecordDenied
}

// leakageTaintRecord is the subset of infoleakagetaint.TaintRecord needed by
// the host for the LeakageGrantWriter call.
type leakageTaintRecord struct {
	ResourceType string
	ResourceID   string
	Permission   string
	ToolName     string
}

// pendingApprovals is the per-request state map, guarded by a mutex.
// Since one runnerHost is constructed per dispatch site and its lifetime
// spans a single executor.Run, concurrent access is uncommon but possible
// (multiple tool goroutines share the Loop but not the host instance).
type pendingApprovals struct {
	mu sync.Mutex
	m  map[string]*pendingApproval
}

func (p *pendingApprovals) store(reqID string, pa *pendingApproval) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]*pendingApproval{}
	}
	p.m[reqID] = pa
}

func (p *pendingApprovals) take(reqID string) (*pendingApproval, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pa, ok := p.m[reqID]
	if ok {
		delete(p.m, reqID)
	}
	return pa, ok
}

// pending is the per-host pending state.  Each runnerHost carries its own.
func (h *runnerHost) pending() *pendingApprovals {
	h.pendingMu.Lock()
	defer h.pendingMu.Unlock()
	if h.pendingApprovals == nil {
		h.pendingApprovals = &pendingApprovals{}
	}
	return h.pendingApprovals
}

// PublishApproval builds the channel envelope keyed on ask.Kind, registers the
// pending state, and returns the requestID. The actual envelope publish happens
// inside AwaitDecision's OnPublish callback, so the request is registered with
// the Orchestrator before any decision can arrive.
func (h *runnerHost) PublishApproval(ctx context.Context, ask pipeline.ApprovalAsk) (string, error) {
	reqID := newRequestID()

	switch ask.Kind {
	case "tool_call":
		pa, err := h.buildToolCallPending(ctx, reqID, ask)
		if err != nil {
			return "", fmt.Errorf("host: build tool_call approval: %w", err)
		}
		h.pending().store(reqID, pa)
		h.recordDecisionAsked(ctx, reqID, ask.Kind)
		return reqID, nil

	case "leakage_share":
		pa, err := h.buildLeakagePending(ctx, reqID, ask)
		if err != nil {
			return "", fmt.Errorf("host: build leakage_share approval: %w", err)
		}
		h.pending().store(reqID, pa)
		h.recordDecisionAsked(ctx, reqID, ask.Kind)
		return reqID, nil

	case "content_inspection":
		pa, err := h.buildContentInspectionPending(ctx, reqID, ask)
		if err != nil {
			return "", fmt.Errorf("host: build content_inspection approval: %w", err)
		}
		h.pending().store(reqID, pa)
		h.recordDecisionAsked(ctx, reqID, ask.Kind)
		return reqID, nil

	case "plan_phase", "plan_amendment":
		pa, err := h.buildPlanGatePending(ctx, reqID, ask, ask.Kind)
		if err != nil {
			return "", fmt.Errorf("host: build %s approval: %w", ask.Kind, err)
		}
		h.pending().store(reqID, pa)
		h.recordDecisionAsked(ctx, reqID, ask.Kind)
		return reqID, nil

	// The Kind is the SHARED categories.PreconditionWaiver constant (RULING
	// P3-1), the same symbol the raise-time ask and the payload Category use. A
	// re-typed literal here would compile and then fall through to the default's
	// fail-closed "unknown approval kind" — a waiver that never raises.
	case categories.PreconditionWaiver:
		pa, err := h.buildPreconditionWaiverPending(ctx, reqID, ask)
		if err != nil {
			return "", fmt.Errorf("host: build precondition_waiver approval: %w", err)
		}
		h.pending().store(reqID, pa)
		h.recordDecisionAsked(ctx, reqID, ask.Kind)
		return reqID, nil

	case "preference_save":
		pa, err := h.buildPreferenceSavePending(ctx, reqID, ask)
		if err != nil {
			return "", fmt.Errorf("host: build preference_save approval: %w", err)
		}
		h.pending().store(reqID, pa)
		h.recordDecisionAsked(ctx, reqID, ask.Kind)
		return reqID, nil

	default:
		return "", fmt.Errorf("host: unknown approval kind %q", ask.Kind)
	}
}

// decisionKindFor maps a pipeline ApprovalAsk kind string to the lifecycle
// core's DecisionKind. The three host-published kinds map directly; scope_review
// and join are driven elsewhere (cold start / operator join gate).
func decisionKindFor(askKind string) lifecyclecore.DecisionKind {
	switch askKind {
	case "leakage_share":
		return lifecyclecore.DecisionLeakageShare
	case "content_inspection":
		return lifecyclecore.DecisionContentInspect
	default:
		return lifecyclecore.DecisionToolCall
	}
}

// timeoutPolicyFor maps an approval-ask kind to the pipeline TimeoutPolicy,
// sourced from the lifecycle decisionParams table via FailsClosedOnTimeout, so
// the executor's Deny-vs-Halt verdict for a timeout matches the session-phase
// projection. All executor-path kinds resolve to TimeoutDeny today; TimeoutHalt
// is the wired extension point for a future fail-closed approval kind.
func timeoutPolicyFor(askKind string) pipeline.TimeoutPolicy {
	if lifecyclecore.FailsClosedOnTimeout(decisionKindFor(askKind)) {
		return pipeline.TimeoutHalt
	}
	return pipeline.TimeoutDeny
}

// recordDecisionAsked appends a DecisionAsked transition to the signed log so
// the operator's fold + audit see the human-in-the-loop request. The live park
// (the dispatch goroutine blocking on the orchestrator) is unchanged; this is
// the durable record. No-op when no log is wired.
func (h *runnerHost) recordDecisionAsked(ctx context.Context, reqID, askKind string) {
	h.l.emitLifecycleEvent(ctx, lifecyclecore.DecisionAsked{
		RequestID: reqID,
		Kind:      decisionKindFor(askKind),
	})
}

// AwaitDecision blocks on l.Approval.Await, invoking OnPublish (the envelope
// send) via the Orchestrator's publish callback. On decision it runs the
// kind-specific post-approval effect.
func (h *runnerHost) AwaitDecision(ctx context.Context, reqID string, timeout time.Duration) (approved bool, by string, timedOut bool, err error) {
	pa, ok := h.pending().take(reqID)
	if !ok {
		return false, "", false, fmt.Errorf("host: no pending approval for request %q", reqID)
	}
	if h.l.Approval == nil {
		return false, "", false, fmt.Errorf("host: l.Approval not wired; cannot await decision for request %q", reqID)
	}
	if pa.expiresAt != nil {
		if remaining := time.Until(*pa.expiresAt); remaining < timeout {
			timeout = remaining
		}
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req := approval.Request{
		RequestID:  reqID,
		SessionRef: h.sess.Namespace + "/" + h.sess.Name,
		OnPublish:  pa.onPublish,
	}
	// Plan card shows "paused — awaiting approval" while we block, flipping back
	// to active once the decision resolves (the turn continues either way).
	// Refcounted so concurrent approval-gated tool calls in one turn keep the
	// card paused until the last resolves. No-op without a publisher.
	//
	// A proxy-exec (widget app-tool) await SKIPS the shared pause: a widget's
	// human-approval wait must not freeze the live agent turn's
	// RunClock/progress/plan-card. Read into a local so the enter and exit
	// decisions can never diverge.
	pausesTurn := !h.proxyExec
	if pausesTurn {
		h.l.enterApprovalPause(ctx, approvalPauseCause(pa.kind))
	}
	d, awaitErr := h.l.Approval.Await(waitCtx, req)
	if pausesTurn {
		h.l.exitApprovalPause(ctx)
	}
	// Record the decision resolution in the signed log (audit + the operator's
	// fold). A wait-deadline error is the timeout case (distinct from a human
	// deny, which returns awaitErr==nil with Approved=false); other await errors
	// are publish/transport failures that the executor turns into a Halt.
	timedOut = pipeline.IsTimeout(awaitErr)
	h.l.emitLifecycleEvent(ctx, lifecyclecore.DecisionResolved{
		RequestID: reqID,
		Approved:  awaitErr == nil && d.Approved,
		TimedOut:  timedOut,
	})
	// Same two facts the signed log just recorded, delivered to the per-call
	// observer as well. Placed AFTER the log emit deliberately: the durable
	// audit record must not depend on a presentation observer succeeding.
	if h.approvalEvent != nil {
		h.approvalEvent(approvalObservation{
			Resolved: true,
			Approved: awaitErr == nil && d.Approved,
			TimedOut: timedOut,
		})
	}
	if timedOut {
		// The signed log emit above returns the operator-derived phase to
		// Running, but channelsd owns the channel-facing surface (pending
		// queue + condition + rendered prompt) and reacts to NATS, not the
		// log. Tell it the request expired so it clears that surface instead
		// of stranding it. Deny-only; a lapsed deadline never approves.
		h.publishTimeoutApplied(ctx, reqID, pa.kind)
	}
	// A genuine transport/publish failure inside Await — the executor cannot turn
	// it into a verdict, so it fails closed as a Halt. A timeout is NOT an error:
	// it falls through to the kind-specific deny effects (SYSTEM_TIMEOUT framing)
	// and is returned as timedOut=true, err=nil for the executor to classify.
	if awaitErr != nil && !timedOut {
		return false, "", false, awaitErr
	}

	// Run post-approval effect.
	switch pa.kind {
	case "tool_call":
		toolName := ""
		if pa.toolCallPayload != nil {
			toolName = pa.toolCallPayload.toolName
		}
		if d.Approved && pa.toolCallPayload != nil {
			if effectErr := h.toolCallPostApprove(ctx, pa.toolCallPayload); effectErr != nil {
				// Grant check failed: surface as approved=false so the executor
				// Denies. Capture the underlying re-check message so dispatch can
				// surface it (the generic executor would otherwise show
				// "approval denied or timed out").
				slog.Default().Info("host: post-approve grant check failed",
					"session", h.sess.Namespace+"/"+h.sess.Name, "err", effectErr.Error())
				h.setApprovalDenyReason(effectErr.Error())
				// The ledger must state what HAPPENED, not what was clicked. The
				// human approved, but the tool call was denied — record "denied"
				// with the re-check's reason. Without this the append-only,
				// signed approval log ends at the request entry with no outcome,
				// while DecisionResolved above says Approved=true: an audit that
				// reads as an allowed call that was never allowed.
				denied := d
				denied.Reason = "post-approval authorization re-check denied: " + effectErr.Error()
				h.recordApprovalOutcome(ctx, pa.toolCallPayload, denied, "denied")
				return false, d.ApproverID, false, nil
			}
		}
		if !d.Approved {
			// Anti-confabulation deny framing (timeout vs explicit deny vs
			// publish error). awaitErr is nil for a delivered deny or a remote
			// timeout — a real await/publish error returned earlier as a Halt —
			// and the ctx-deadline for a local timeout that fell through above.
			// Passing it is what frames a local timeout as SYSTEM_TIMEOUT rather
			// than SYSTEM_DECISION_DENIED: nobody rejected the request.
			h.setApprovalDenyReason(approvalFailureContent(toolName, d, awaitErr))
			h.recordApprovalOutcome(ctx, pa.toolCallPayload, d, "denied")
		} else {
			h.recordApprovalOutcome(ctx, pa.toolCallPayload, d, "approved")
		}

	case "leakage_share":
		if d.Approved && pa.leakagePayload != nil {
			if effectErr := h.leakagePostApprove(ctx, pa.leakagePayload); effectErr != nil {
				slog.Default().Info("host: leakage grant write failed",
					"session", h.sess.Namespace+"/"+h.sess.Name, "err", effectErr.Error())
				// Grant write failed: treat as denial.
				return false, d.ApproverID, false, nil
			}
			// Audit the leakage-specific approve outcome; the executor's
			// generic approval_resolved record is a different kind.
			h.auditLeakageDecision(ctx, "leakage_approved", pa.leakagePayload, d.ApproverID)
			// Record approval so the respond-time (PreResponse) gate does NOT
			// re-prompt the approver for a resource the pre-feed gate cleared.
			if pa.leakagePayload.onApproved != nil {
				for _, t := range pa.leakagePayload.taint {
					pa.leakagePayload.onApproved(t.ResourceType, t.ResourceID)
				}
			}
		}
		if !d.Approved && pa.leakagePayload != nil {
			// Audit the leakage-specific denial.
			h.auditLeakageDecision(ctx, "leakage_denied", pa.leakagePayload, d.ApproverID)
			// Record denial so the audience hook knows not to re-prompt.
			if pa.leakagePayload.onDenied != nil {
				for _, t := range pa.leakagePayload.taint {
					pa.leakagePayload.onDenied(t.ResourceType, t.ResourceID)
				}
			}
		}

	case "content_inspection":
		// No grant-write effect: the executor uses `approved` to allow or
		// withhold the flagged tool I/O. Audit is handled by the contentguard
		// adapter's emit hook. The approved bool flows back to the executor
		// unchanged.

	case "plan_phase", "plan_amendment":
		// No grant write: the plan gate's authority is the folded log, not a
		// SpiceDB relationship. Recording the decision IS the effect.
		h.recordPlanGateDecision(ctx, pa.planGatePayload, d.Approved, timedOut, d.Approver)

	case "preference_save":
		// No post-approval effect runner-side, deliberately: the commit is
		// channelsd's (BindPreferenceCommitHandler / preferenceCommitHandler,
		// pkg/channels/channelsd/pipeline/preference_commit.go), keyed off the
		// Details this host published in buildPreferenceSavePending and the
		// verified decider — never anything the runner could supply
		// post-publish. approved/timedOut flow back to preferencesSaver.Save
		// unchanged, which is the whole of what this case needs to do.
	}

	return d.Approved, d.ApproverID, timedOut, nil
}

// publishTimeoutApplied notifies channelsd that a host-driven approval
// (tool_call / leakage_share / content_inspection) reached its per-kind deadline
// with no decision. It builds the matching "applied" envelope with a
// deny/timeout outcome and hands it to l.TimeoutAppliedPublish, which fans it to
// the IN subject (channelsd clears its pending queue + condition) and the OUT
// subject (the channel sender edits the prompt to "expired"). Deny-only: a
// lapsed deadline never approves. Best-effort — without the publish hook (a
// kubectl/test session) there is no channel surface to clear, and a build or
// publish failure is logged, never fatal to the turn.
func (h *runnerHost) publishTimeoutApplied(ctx context.Context, reqID, kind string) {
	if h.l.TimeoutAppliedPublish == nil {
		return
	}
	var (
		appliedKind channelevents.Kind
		payload     any
	)
	// All three host-driven approval kinds ride the unified Interaction model:
	// an expired deadline publishes interaction_applied(expired) so
	// HandleInteractionApplied clears the durable pending entry and the generic
	// surface edits the prompt to "expired".
	var category string
	switch kind {
	case "tool_call":
		category = categories.ToolApproval
	case "leakage_share":
		category = categories.InfoLeakage
	case "content_inspection":
		category = categories.ContentInspection
	case categories.PreconditionWaiver:
		// A lapsed waiver must clear the channelsd surface too, else its pending
		// queue + condition strand. Deny-only, like every other kind here: a
		// lapsed deadline never waives a fact-gated refusal.
		category = categories.PreconditionWaiver
	case "preference_save":
		// Same reason as PreconditionWaiver above: without this, a lapsed
		// preference_save confirm strands channelsd's pending queue + the
		// PreferenceConfirmPending condition forever, since nothing else ever
		// tells it the deadline passed. Deny-only: a lapsed deadline never
		// commits a preference.
		category = categories.UserPreferenceConfirm
	default:
		return
	}
	appliedKind = channelevents.KindInteractionApplied
	payload = channelevents.InteractionAppliedPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: h.sess.Namespace, Name: h.sess.Name},
		Category:        category,
		RequestRef:      reqID,
		Outcome:         channelevents.OutcomeExpired,
		Reason:          "timeout",
	}

	sessionRef := h.sess.Namespace + "/" + h.sess.Name
	env, err := channelevents.BuildEnvelope(h.sess.Namespace, h.sess.Name, appliedKind, payload)
	if err != nil {
		slog.Default().Info("host: build timeout applied envelope failed",
			"session", sessionRef, "kind", kind, "reqID", reqID, "err", err.Error())
		return
	}
	if err := h.l.TimeoutAppliedPublish(ctx, h.sess.Namespace, h.sess.Name, env); err != nil {
		slog.Default().Info("host: publish timeout applied failed",
			"session", sessionRef, "kind", kind, "reqID", reqID, "err", err.Error())
	}
}

// buildToolCallPending constructs the pending state for a tool_call approval
// and builds the generic interaction_request envelope it publishes.
//
// Standing model (invariant): the approvers resolved host-side into
// Audience.Approvers are DELIVERY ROUTING ONLY; the AUTHORITATIVE server-side
// gate is the ToolApproval category's DecideResourceOwners policy, which
// re-checks — against the request's Resources — that the clicker is an #owner of
// the gated resource before the grant handler writes the grant tuple. So this
// publisher stamps BOTH: Resources = [{type,id}] when the permission names a
// resource (else nil → the session approve-set gate), and Details = the exact
// grant-write inputs the handler reads back (Permission / ResourceType /
// ResourceID / ArgsHash / StateImpact). Details is also persisted on the durable
// memapproval record so a channelsd restart can recover the grant fields + the
// resource-owner set.
func (h *runnerHost) buildToolCallPending(ctx context.Context, reqID string, ask pipeline.ApprovalAsk) (*pendingApproval, error) {
	if h.l.InteractionRequestPublish == nil {
		return nil, fmt.Errorf("tool_call approval unavailable (interaction publish hook not configured)")
	}

	sessNS := extractString(ask.Payload, "sess_ns")
	sessName := extractString(ask.Payload, "sess_name")
	if sessNS == "" {
		sessNS = h.sess.Namespace
	}
	if sessName == "" {
		sessName = h.sess.Name
	}

	toolName := extractString(ask.Payload, "tool_name")
	permission := extractString(ask.Payload, "permission")
	resourceType := extractString(ask.Payload, "resource_type")
	resourceID := extractString(ask.Payload, "resource_id")
	stateImpact := extractString(ask.Payload, "state_impact")
	argsHash := extractString(ask.Payload, "args_hash")
	justification := extractString(ask.Payload, "justification")
	// Who may approve is DECLARED by the resource type — the publish-time half of
	// the same decision buildToolCallApprovalAsk makes at raise time, and it MUST
	// match: the two disagreeing is how an ask gets raised and then becomes
	// undeliverable.
	//
	// Resources carries the same answer onto the wire, because that is what
	// DecideResourceOwners gates on at CLICK time. Leaving it set for a
	// session-only type would route the prompt to the session's approvers and
	// then refuse their click.
	var resources []channelevents.InteractionResourceRef
	var resourceOwnerSets []string
	approverSet, declared := approverSetFor(h.l.ResourceStandings, resourceType, resourceID)
	if !declared {
		return nil, fmt.Errorf("tool_call approval: resource type %q declares no standing, so there is no way to know who may approve it", resourceType)
	}
	if approverSet != "" {
		rs := h.l.ResourceStandings[resourceType]
		resources = []channelevents.InteractionResourceRef{
			{Type: resourceType, ID: resourceID, Permission: rs.ApproverPermission},
		}
		resourceOwnerSets = []string{approverSet}
	}
	sessionApproveSet := "agentsession:" + sessNS + "/" + sessName + "#approve"
	approvers, aerr := h.resolveInteractionApprovers(ctx, sessionApproveSet, resourceOwnerSets)
	if aerr != nil {
		return nil, fmt.Errorf("resolve tool_call approvers: %w", aerr)
	}
	if len(approvers) == 0 {
		// Fail closed rather than publish an undeliverable prompt (which
		// InteractionRequestPayload.Validate also rejects for AudienceApprovers
		// scope). Parity with buildToolCallApprovalAsk's raise-time
		// "no one has standing to approve" guard.
		return nil, fmt.Errorf("tool_call approval has no resolvable approvers for %s", approverSetDescription(sessionApproveSet, resourceOwnerSets))
	}
	// An approval the clicking viewer can act on is a TRUST EVENT that must
	// auto-reveal chrome; one only somebody else can act on must not. Both sides
	// go through uiaction.RequesterKey because the approver list carries the
	// SpiceDB-prefixed identity.Subject form while the ask payload's "subject"
	// carries the bare canonical form — comparing them raw is always false, and
	// always-false here is silent (chrome simply never reveals).
	if h.approvalEvent != nil {
		viewer := uiaction.RequesterKey(extractString(ask.Payload, "subject"))
		addressed := viewer != "" && slices.ContainsFunc(approvers, func(a channelevents.ExternalIdentity) bool {
			return uiaction.RequesterKey(string(a.Subject)) == viewer
		})
		h.approvalEvent(approvalObservation{Asked: true, AddressedToViewer: addressed})
	}

	// Details: the non-display grant-write inputs the channelsd handler reads
	// back to write the grant tuple (invariant — resolveToolApprovalDetails
	// decodes these).
	occupancy, rebind := h.l.slotOccupancyRebindFor(resourceType)
	det := channelevents.ToolApprovalDetails{
		ToolName:        toolName,
		Permission:      permission,
		ResourceType:    resourceType,
		ResourceID:      resourceID,
		ArgsHash:        argsHash,
		StateImpact:     stateImpact,
		NoSlotGrant:     resourceType != "" && !slices.Contains(h.l.PlanGateSlotTypes, resourceType),
		Occupancy:       occupancy,
		Rebind:          rebind,
		ArgsJSON:        extractString(ask.Payload, "args_json"),
		ToolDescription: extractString(ask.Payload, "tool_description"),
		Justification:   justification,
	}
	detJSON, derr := json.Marshal(det)
	if derr != nil {
		return nil, fmt.Errorf("marshal tool_call details: %w", derr)
	}

	// The same published copy the plan-gate card uses, so one call cannot be
	// described two different ways depending on which surface asks. Both maps
	// are display-only and absent entries degrade to a detokenized fallback.
	card := toolApprovalCopy{
		ToolName:        toolName,
		ToolDescription: extractString(ask.Payload, "tool_description"),
		Permission:      permission,
		ResourceType:    resourceType,
		ResourceID:      resourceID,
		PermissionTitle: h.l.PlanGatePermissionTitles[resourceType+"/"+permission],
		ResourceLabel:   resourceDisplayLabel(h.l.PlanGateResourceDisplays, resourceType, resourceID),
		Justification:   justification,
	}
	card.What = whatLine(ask.Summary, card)

	pl := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sessNS, Name: sessName},
		Category:        categories.ToolApproval,
		RequestRef:      reqID,
		Lead:            toolApprovalLead(),
		Fields:          toolApprovalFields(card),
		Actions:         approveDenyActions(),
		Details:         detJSON,
		Resources:       resources,
		Audience: channelevents.InteractionAudience{
			Scope:          channelevents.AudienceApprovers,
			Approvers:      approvers,
			PublicNote:     true,
			PublicNoteBody: toolApprovalPublicNoteBody(h.l.agentClassDisplayName(), card.What),
		},
		ExpiresAt:     expiresAtPtr(hostNow().Add(h.l.resolvedApprovalTimeout())),
		Interruptible: true,
	}
	env, eerr := channelevents.BuildEnvelope(sessNS, sessName, channelevents.KindInteractionRequest, pl)
	if eerr != nil {
		return nil, fmt.Errorf("build tool_call interaction envelope: %w", eerr)
	}

	useID := extractString(ask.Payload, "use_id")
	publish := h.l.InteractionRequestPublish
	pa := &pendingApproval{
		kind: "tool_call",
		onPublish: func(pubCtx context.Context) error {
			return publish(pubCtx, sessNS, sessName, env)
		},
		toolCallPayload: &pendingToolCall{
			sessNS:   sessNS,
			sessName: sessName,
			toolName: toolName,
			argsMap:  extractAnyMap(ask.Payload, "args_map"),
			argsHash: argsHash,
			perm:     extractPermission(ask.Payload),
			// The generic InteractionRequestPayload has no ApproverSubject field,
			// so the approver comes from the stamped payload key.
			approver: extractString(ask.Payload, "approver_subject"),
			useID:    useID,
			// On the app-tool path: subject/subjects are the widget VIEWER
			// principal the post-approval re-verify checks against.
			subject:  extractString(ask.Payload, "subject"),
			subjects: extractStringSlice(ask.Payload, "subjects"),
		},
	}

	// Durable record: the audit trail + the cross-restart authority channelsd's
	// memapproval.RequestByID readers (interaction_decision.go,
	// tool_approval_interaction.go) consult once its in-process cache is gone.
	// Carries Resources + Details keyed by the request id. Scope uses the
	// ENVELOPE's session so channel-side lookups keyed by the envelope's sessRef
	// find it.
	if h.l.Mem != nil && useID != "" {
		scope := memory.Scope{Kind: "session", ID: sessNS + "/" + sessName}
		if err := memapproval.RecordRequest(context.Background(), h.l.Mem, scope, useID, memapproval.Request{
			RequestID: reqID,
			ToolName:  toolName,
			Resources: resources,
			Details:   detJSON,
		}); err != nil {
			slog.Default().Info("host: memapproval.RecordRequest failed",
				"session", scope.ID, "reqID", reqID, "err", err.Error())
		}
	}

	return pa, nil
}

// buildPreconditionWaiverPending constructs the pending state + interaction
// envelope for a precondition_waiver approval — the human WAIVER card for a slot
// whose precondition (a CEL predicate over signed facts) REFUSED the call.
//
// It mirrors buildToolCallPending's SECURITY-relevant shape — Resources (the
// DecideResourceOwners standing input the category re-checks server-side),
// Details (the ToolApprovalDetails the waiver handler reads back to bind the slot
// grant), and a durable memapproval record for channelsd's cross-restart
// authority — but publishes categories.PreconditionWaiver and renders the
// gate-authored RefusalMessage as the card body. Approver resolution runs through
// the SAME preconditionWaiverApproverSets helper the raise half called, so the
// delivery audience here and the raise-time eligibility check there cannot
// disagree about who may click.
//
// There is deliberately NO AwaitDecision post-effect case for this kind: the
// slot grant is written by channelsd's waiver handler on approve (that IS the
// waiver), and the executor lets the gated call proceed on approved=true; a lapse
// leaves it denied (TimeoutDeny). A runner-side re-check would double-enforce a
// gate a human just consented past.
func (h *runnerHost) buildPreconditionWaiverPending(ctx context.Context, reqID string, ask pipeline.ApprovalAsk) (*pendingApproval, error) {
	if h.l.InteractionRequestPublish == nil {
		return nil, fmt.Errorf("precondition_waiver approval unavailable (interaction publish hook not configured)")
	}

	sessNS := extractString(ask.Payload, "sess_ns")
	sessName := extractString(ask.Payload, "sess_name")
	if sessNS == "" {
		sessNS = h.sess.Namespace
	}
	if sessName == "" {
		sessName = h.sess.Name
	}

	toolName := extractString(ask.Payload, "tool_name")
	permission := extractString(ask.Payload, "permission")
	resourceType := extractString(ask.Payload, "resource_type")
	resourceID := extractString(ask.Payload, "resource_id")
	stateImpact := extractString(ask.Payload, "state_impact")
	argsHash := extractString(ask.Payload, "args_hash")
	refusalMessage := extractString(ask.Payload, "refusal_message")
	preApprovers := extractStringSlice(ask.Payload, "precondition_approvers")

	// Same approver resolution as the raise half (preconditionWaiverApproverSets):
	// the refusing rule's declared Approvers when set, else the resource's
	// resolved standing.
	resourceOwnerSets, declared := preconditionWaiverApproverSets(preApprovers, h.l.ResourceStandings, resourceType, resourceID)
	if !declared {
		return nil, fmt.Errorf("precondition_waiver approval: resource type %q declares no standing, so there is no way to know who may waive it", resourceType)
	}
	// Resources is the DecideResourceOwners CLICK gate input, and it is stamped
	// ONLY when the pool is the resource's own #owner set — i.e. the rule declared
	// no approvers AND the type's standing yielded a non-empty owner set (required
	// standing). Then delivery and the click gate both key on #owner, consistently.
	//
	// When the rule DECLARED approvers (or the type is session-only, yielding the
	// empty owner set), Resources is left EMPTY so CheckApproverAuthorized folds to
	// the session-approve gate CheckApprove(agentsession#approve) — the set the
	// card was DELIVERED to (interaction_decision.go documents the empty-Resources
	// fold as intentional). Stamping the resource here would make the click gate
	// demand #owner of a resource nobody owns — a userless session's fork has no
	// owner — and the delivered session approver would be DENIED at click time
	// ("you are not an owner of the affected resource"), so the waiver could never
	// be granted.
	//
	// KNOWN LIMIT: a rule that declares an ARBITRARY set which is neither
	// the session-approve set nor the resource #owner set (e.g. a bespoke
	// risk-council relation) still cannot be click-gated — CheckApproverAuthorized
	// consults only CheckApprove(agentsession#approve) or CheckOwnerOnResource,
	// never a third set. Making that work needs a new Task-4 decider policy. Today
	// `approvers` supports the session-approve set (folds correctly here) and the
	// unset/owner default; demo-reviewbot's concrete case is `approvers: session`.
	var resources []channelevents.InteractionResourceRef
	if len(preApprovers) == 0 && len(resourceOwnerSets) > 0 {
		rs := h.l.ResourceStandings[resourceType]
		resources = []channelevents.InteractionResourceRef{
			{Type: resourceType, ID: resourceID, Permission: rs.ApproverPermission},
		}
	}
	sessionApproveSet := "agentsession:" + sessNS + "/" + sessName + "#approve"
	approvers, aerr := h.resolveInteractionApprovers(ctx, sessionApproveSet, resourceOwnerSets)
	if aerr != nil {
		return nil, fmt.Errorf("resolve precondition_waiver approvers: %w", aerr)
	}
	if len(approvers) == 0 {
		// Fail closed rather than publish an undeliverable prompt (Validate rejects
		// an empty approvers scope too). Parity with the raise half's guard.
		return nil, fmt.Errorf("precondition_waiver approval has no resolvable approvers for %s", approverSetDescription(sessionApproveSet, resourceOwnerSets))
	}

	// Details: the grant-write inputs the waiver handler reads back
	// (resolveToolApprovalDetails decodes these) to bind the slot grant that IS
	// the waiver. The SAME ToolApprovalDetails shape a tool_call card carries.
	occupancy, rebind := h.l.slotOccupancyRebindFor(resourceType)
	det := channelevents.ToolApprovalDetails{
		ToolName:        toolName,
		Permission:      permission,
		ResourceType:    resourceType,
		ResourceID:      resourceID,
		ArgsHash:        argsHash,
		StateImpact:     stateImpact,
		NoSlotGrant:     resourceType != "" && !slices.Contains(h.l.PlanGateSlotTypes, resourceType),
		Occupancy:       occupancy,
		Rebind:          rebind,
		ArgsJSON:        extractString(ask.Payload, "args_json"),
		ToolDescription: extractString(ask.Payload, "tool_description"),
	}
	detJSON, derr := json.Marshal(det)
	if derr != nil {
		return nil, fmt.Errorf("marshal precondition_waiver details: %w", derr)
	}

	pl := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sessNS, Name: sessName},
		Category:        categories.PreconditionWaiver,
		RequestRef:      reqID,
		Lead:            preconditionWaiverLead(),
		Fields:          preconditionWaiverFields(refusalMessage),
		Actions:         approveDenyActions(),
		Details:         detJSON,
		Resources:       resources,
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceApprovers,
			Approvers: approvers,
			// No PublicNote: waiving a fact-gated refusal is one of the strongest
			// consents in the system and the RefusalMessage may quote why the gate
			// fired — kept to the approver, not posted to the whole room.
			PublicNote: false,
		},
		ExpiresAt:     expiresAtPtr(hostNow().Add(h.l.resolvedApprovalTimeout())),
		Interruptible: true,
	}
	env, eerr := channelevents.BuildEnvelope(sessNS, sessName, channelevents.KindInteractionRequest, pl)
	if eerr != nil {
		return nil, fmt.Errorf("build precondition_waiver interaction envelope: %w", eerr)
	}

	publish := h.l.InteractionRequestPublish
	pa := &pendingApproval{
		kind: categories.PreconditionWaiver,
		onPublish: func(pubCtx context.Context) error {
			return publish(pubCtx, sessNS, sessName, env)
		},
	}

	// Durable record for channelsd's cross-restart authority: the waiver handler
	// resolves ToolApprovalDetails from memapproval.RequestByID (keyed by the
	// RequestID tag) once its in-process cache is gone. Same shape + key strategy
	// as tool_call (stored under the tool_use id, findable by reqID).
	useID := extractString(ask.Payload, "use_id")
	if h.l.Mem != nil && useID != "" {
		scope := memory.Scope{Kind: "session", ID: sessNS + "/" + sessName}
		if err := memapproval.RecordRequest(context.Background(), h.l.Mem, scope, useID, memapproval.Request{
			RequestID: reqID,
			ToolName:  toolName,
			Resources: resources,
			Details:   detJSON,
		}); err != nil {
			slog.Default().Info("host: memapproval.RecordRequest (precondition_waiver) failed",
				"session", scope.ID, "reqID", reqID, "err", err.Error())
		}
	}

	return pa, nil
}

// preconditionWaiverLead is the headline the generic renderer bolds. No glyph of
// its own — the surface draws the tone chip from the category (ToneCritical).
func preconditionWaiverLead() string { return "Precondition waiver needed" }

// preconditionWaiverFields renders the gate-authored refusal as the card's one
// field. The RefusalMessage is TRUSTED copy (the class author wrote it in the
// CRD), unlike the agent's justification or a raw fact value — so it is shown
// directly rather than through the inert-excerpt path untrusted content takes.
func preconditionWaiverFields(refusalMessage string) []channelevents.InteractionField {
	msg := strings.TrimSpace(refusalMessage)
	if msg == "" {
		msg = "_(the gate declared no refusal message)_"
	}
	return []channelevents.InteractionField{
		{Label: "Why this was refused", Value: msg},
	}
}

// buildLeakagePending constructs the pending state for a leakage_share approval
// and builds the generic interaction_request envelope it publishes.
//
// Safety invariants:
//   - NO PublicNote is ever set — the proposed share is a PRIVATE DM to the data
//     owner; posting it (or the recipient) where the whole conversation can see
//     it would itself leak.
//   - The render NAMES the recipient (infoLeakageWouldShareWithFields) so the
//     data owner knows exactly who the agent wants to expose the data to.
//   - HARD-ERROR on empty leaked_to: never prompt an owner to approve a share to
//     an unnamed recipient.
//
// Standing is data-owner-only (invariant): approvers resolve from the data
// #owner union (NO session approve-set), and Resources = the taint data refs so
// DecideResourceOwners gates on the data owner. The grant itself is written
// RUNNER-side by leakagePostApprove (via the interaction_applied bridge), which
// reads exactly the pendingLeakageShare payload built below.
func (h *runnerHost) buildLeakagePending(ctx context.Context, reqID string, ask pipeline.ApprovalAsk) (*pendingApproval, error) {
	if h.l.InteractionRequestPublish == nil {
		return nil, fmt.Errorf("leakage approval unavailable (interaction publish hook not configured)")
	}

	sessNS := extractString(ask.Payload, "sess_ns")
	sessName := extractString(ask.Payload, "sess_name")
	if sessNS == "" {
		sessNS = h.sess.Namespace
	}
	if sessName == "" {
		sessName = h.sess.Name
	}

	leakedTo := extractStringSlice(ask.Payload, "leaked_to")
	// HARD-ERROR: an info_leakage prompt with no named recipient is
	// undeliverable and meaningless — refuse to build it (never publish an
	// unnamed-recipient share). Fail closed before the envelope is built.
	if len(leakedTo) == 0 {
		return nil, fmt.Errorf("info_leakage approval has no recipient (leaked_to empty); refusing to prompt for an unnamed share")
	}

	taint := extractTaintSlice(ask.Payload, "taint")
	ttlStr := extractString(ask.Payload, "ttl")
	ttl, _ := time.ParseDuration(ttlStr)
	if ttl <= 0 {
		ttl = 10 * time.Minute // fallback default
	}
	proposedText := extractString(ask.Payload, "proposed_text")
	approverSubjects := extractStringSlice(ask.Payload, "approver_subjects")

	// Resources = the taint data refs (invariant): DecideResourceOwners gates on
	// these owners, and they are persisted durably for cross-restart recovery.
	resources := make([]channelevents.InteractionResourceRef, 0, len(taint))
	for _, t := range taint {
		resources = append(resources, channelevents.InteractionResourceRef{Type: t.ResourceType, ID: t.ResourceID})
	}

	// Approvers = the data #owner union (NO session approve-set): data-owner-only
	// standing. resolveInteractionApprovers with an empty sessionApproveSet folds
	// to the resourceOwnerSets union in authz.ResolveApprovers.
	approvers, aerr := h.resolveInteractionApprovers(ctx, "", approverSubjects)
	if aerr != nil {
		return nil, fmt.Errorf("resolve info_leakage approvers: %w", aerr)
	}
	if len(approvers) == 0 {
		return nil, fmt.Errorf("info_leakage approval has no resolvable data owners for %s", strings.Join(approverSubjects, ", "))
	}

	pl := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sessNS, Name: sessName},
		Category:        categories.InfoLeakage,
		RequestRef:      reqID,
		Lead:            infoLeakageLead(proposedText),
		Fields:          infoLeakageWouldShareWithFields(h.l.ChannelKind, leakedTo),
		Actions:         approveDenyActions(),
		Resources:       resources,
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceApprovers,
			Approvers: approvers,
			// NO PublicNote (invariant): a private DM to the data owner, never a
			// public post.
			PublicNote: false,
		},
		ExpiresAt:     expiresAtPtr(hostNow().Add(ttl)),
		Interruptible: true,
	}
	env, eerr := channelevents.BuildEnvelope(sessNS, sessName, channelevents.KindInteractionRequest, pl)
	if eerr != nil {
		return nil, fmt.Errorf("build leakage interaction envelope: %w", eerr)
	}

	publish := h.l.InteractionRequestPublish
	onDeniedCb := extractCallback(ask.Payload, "on_denied")
	onApprovedCb := extractCallback(ask.Payload, "on_approved")

	pa := &pendingApproval{
		kind: "leakage_share",
		onPublish: func(pubCtx context.Context) error {
			return publish(pubCtx, sessNS, sessName, env)
		},
		// The runner-side leakagePostApprove / auditLeakageDecision read exactly
		// these fields.
		leakagePayload: &pendingLeakageShare{
			sessNS:     sessNS,
			sessName:   sessName,
			leakedTo:   leakedTo,
			taint:      taint,
			ttl:        ttl,
			onApproved: onApprovedCb,
			onDenied:   onDeniedCb,
		},
	}

	// Durable record: persist the taint Resources so channelsd's
	// memapproval.RequestByID lookup can recover the data-owner set after a
	// restart. No Details — info_leakage's grant is runner-side, so only the
	// resource set the owner gate reads needs persisting.
	if h.l.Mem != nil {
		scope := memory.Scope{Kind: "session", ID: sessNS + "/" + sessName}
		if err := memapproval.RecordRequest(context.Background(), h.l.Mem, scope, reqID, memapproval.Request{
			RequestID: reqID,
			Resources: resources,
		}); err != nil {
			slog.Default().Info("host: memapproval.RecordRequest (leakage) failed",
				"session", scope.ID, "reqID", reqID, "err", err.Error())
		}
	}

	return pa, nil
}

// maxExcerptRunes is the maximum number of Unicode code points kept in a
// content-inspection excerpt. Longer excerpts are truncated at the host
// (rune-safe); escaping and fencing are the channel renderer's job.
const maxExcerptRunes = 500

// truncateExcerpt hard-caps s at maxRunes code points WITHOUT appending an
// ellipsis — deliberately distinct from stringsx.CapRunes. The
// content-inspection excerpt is untrusted content shown inert, so the preview is
// exactly the flagged bytes, truncated, with no characters added.
func truncateExcerpt(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes])
}

// contentInspectionTTL returns the approval TTL for content inspection
// requests. It is the SAME consolidated authz.approvalTimeout every other
// approval ask waits on (resolvedApprovalTimeout: the 4-tier resolved value
// from status.effectiveSettings, else the class, else 10m) — one window, one
// source, so a tier that tightens it tightens content inspection too.
func (h *runnerHost) contentInspectionTTL() time.Duration {
	ttl := h.l.resolvedApprovalTimeout()
	if ttl <= 0 {
		return 10 * time.Minute
	}
	return ttl
}

// buildContentInspectionPending constructs the pending state for a
// content_inspection approval and builds the generic interaction_request
// envelope it publishes.
//
// Approver resolution is host-side: the eligible approvers are the session
// approve-set derived directly from h.sess — NO resource-owner intersection
// (unlike tool_call / leakage_share), because a suspected-injection flag has no
// external resource counterparty to route consent to. Resolving here keeps
// pkg/authz/contentguard free of the approver model: the adapter emits a generic
// ApprovalAsk with no approver field. Fail-closed on an empty approve-set —
// surfacing "no one has standing" beats prompting into the void, and
// InteractionRequestPayload.Validate rejects an empty approvers scope anyway.
func (h *runnerHost) buildContentInspectionPending(ctx context.Context, reqID string, ask pipeline.ApprovalAsk) (*pendingApproval, error) {
	if h.l.InteractionRequestPublish == nil {
		return nil, fmt.Errorf("content_inspection approval unavailable (interaction publish hook not configured)")
	}
	sessNS := h.sess.Namespace
	sessName := h.sess.Name

	inspector := extractString(ask.Payload, "inspector")
	tool := extractString(ask.Payload, "tool")
	details, _ := ask.Payload["details"].(map[string]any)
	score, _ := details["score"].(float64)
	threshold, _ := details["threshold"].(float64)
	point := extractString(details, "point")
	// RAW excerpt: truncated (rune-safe) but NOT inerted — the generic renderer
	// escapes + code-fences it per the InteractionExcerpt inertness contract.
	excerpt := truncateExcerpt(extractString(details, "excerpt"), maxExcerptRunes)

	// Host-side approver resolution: session approve-set, no resource-owner
	// intersection for content inspection. Resolve the subject-set → canonical
	// approver ids and stamp them as Audience.Approvers (Subject passthrough, the
	// same shape resolveSlackUserIDFromCanonical expects — mirrors leakedToExt).
	approveSet := "agentsession:" + sessNS + "/" + sessName + "#approve"
	var approvers []channelevents.ExternalIdentity
	if h.l.SpiceDBLookupSubjects != nil {
		canons, _, rerr := authz.ResolveApprovers(ctx, loopLookuper{h.l.SpiceDBLookupSubjects}, approveSet, nil)
		if rerr != nil {
			return nil, fmt.Errorf("resolve content_inspection approvers: %w", rerr)
		}
		for _, c := range canons {
			approvers = append(approvers, channelevents.ExternalIdentity{
				Kind:    identity.Kind(h.l.AddressableChannelKind()),
				Subject: identity.Subject(c),
			})
		}
	}
	if len(approvers) == 0 {
		// No reachable approver means the gate cannot be answered — fail closed
		// rather than publish an undeliverable prompt (Validate rejects an empty
		// approvers scope too).
		return nil, fmt.Errorf("content_inspection approval has no resolvable approvers for %s", approveSet)
	}

	// AgentDisplayName intentionally NOT stamped via a rendered Field — that
	// would add a visible card line. Carrying it for CLI/admind display over a
	// non-rendered mechanism remains unimplemented.

	pl := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sessNS, Name: sessName},
		Category:        categories.ContentInspection,
		RequestRef:      reqID,
		Lead:            contentInspectionLead(tool, point, score, threshold, inspector),
		Excerpt:         excerptOrNil(excerpt),
		Actions:         approveDenyActions(),
		Audience:        channelevents.InteractionAudience{Scope: channelevents.AudienceApprovers, Approvers: approvers},
		ExpiresAt:       expiresAtPtr(hostNow().Add(h.contentInspectionTTL())),
		Interruptible:   false, // C1: resurface-interrupt is C2
	}
	env, eerr := channelevents.BuildEnvelope(sessNS, sessName, channelevents.KindInteractionRequest, pl)
	if eerr != nil {
		return nil, fmt.Errorf("build content_inspection interaction envelope: %w", eerr)
	}

	publish := h.l.InteractionRequestPublish
	return &pendingApproval{
		kind: "content_inspection",
		onPublish: func(c context.Context) error {
			return publish(c, sessNS, sessName, env)
		},
		contentInspectionPayload: &pendingContentInspection{
			sessNS:   sessNS,
			sessName: sessName,
		},
	}, nil
}

// buildPreferenceSavePending constructs the pending state + interaction
// envelope for a preference_save confirm.
//
// Audience is unlike every other kind in this file: neither the session
// approve-set nor a resource #owner may decide it — only the CURRENT TURN's
// own author may, because a preference is personal. In a shared thread the
// person whose language/timezone/whatever is being changed decides, not
// whoever owns or started the session. categories.UserPreferenceConfirm's
// Deciders (DecideRequester) enforces exactly this server-side
// (interaction_decision.go), matched against the Audience.Requester built
// here — so this function's ONE job on the audience side is making sure that
// match can succeed.
//
// Requester is built from l.lastInboundAuthor — the canonical subject of the
// most recently drained human turn, kept current by
// drainInbox/advanceRequester — via currentTurnAuthorIdentity, which reuses
// speakerEmail's decode (the same one the annotation echo and the speaker
// profile both rely on). A synthetic (email-less) turn author cannot be
// addressed this way, and the ask is refused before publish: that mirrors
// interaction_decision.go's own DecideRequester refusal of a synthetic
// decider (categories.go's doc: "DecideRequester also refuses synthetic
// subjects, fail-closed") — an unaddressable requester fails at the same
// conceptual point, just earlier, with a clearer error than an ask published
// into the void.
//
// Details carries the (key, value, display) triple EXACTLY as
// pkg/channels/channelsd/pipeline/preference_commit.go's
// preferenceConfirmDetails decodes it — value key omitted entirely (never
// JSON null) to mean "clear". This is channelsd's own copy of what the card
// promised, read back at commit time; the runner never re-derives it.
func (h *runnerHost) buildPreferenceSavePending(ctx context.Context, reqID string, ask pipeline.ApprovalAsk) (*pendingApproval, error) {
	if h.l.InteractionRequestPublish == nil {
		return nil, fmt.Errorf("preference_save approval unavailable (interaction publish hook not configured)")
	}
	sessNS, sessName := h.sess.Namespace, h.sess.Name

	key := extractString(ask.Payload, "key")
	display := extractString(ask.Payload, "display")
	if key == "" {
		return nil, fmt.Errorf("preference_save approval: key must not be empty")
	}

	requester := currentTurnAuthorIdentity(h.l.lastInboundAuthor)
	if requester == nil {
		return nil, fmt.Errorf("preference_save approval has no addressable current-turn author to confirm with")
	}

	det := map[string]any{"key": key, "display": display}
	if raw, ok := extractRawMessage(ask.Payload, "value"); ok {
		det["value"] = raw
	}
	detJSON, derr := json.Marshal(det)
	if derr != nil {
		return nil, fmt.Errorf("marshal preference_save details: %w", derr)
	}

	pl := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sessNS, Name: sessName},
		Category:        categories.UserPreferenceConfirm,
		RequestRef:      reqID,
		Lead:            preferenceSaveLead(display),
		Actions:         approveDenyActions(),
		Details:         detJSON,
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceRequester,
			Requester: requester,
		},
		ExpiresAt:     expiresAtPtr(hostNow().Add(h.l.resolvedApprovalTimeout())),
		Interruptible: true,
	}
	env, eerr := channelevents.BuildEnvelope(sessNS, sessName, channelevents.KindInteractionRequest, pl)
	if eerr != nil {
		return nil, fmt.Errorf("build preference_save interaction envelope: %w", eerr)
	}

	publish := h.l.InteractionRequestPublish
	pa := &pendingApproval{
		kind: "preference_save",
		onPublish: func(pubCtx context.Context) error {
			return publish(pubCtx, sessNS, sessName, env)
		},
	}

	// Durable record for channelsd's cross-restart authority:
	// resolvePreferenceConfirmDetails falls back to memapproval.RequestByID
	// once its in-process cache is gone — same shape + key strategy as every
	// other kind here.
	if h.l.Mem != nil {
		scope := memory.Scope{Kind: "session", ID: sessNS + "/" + sessName}
		if err := memapproval.RecordRequest(context.Background(), h.l.Mem, scope, reqID, memapproval.Request{
			RequestID: reqID,
			Details:   detJSON,
		}); err != nil {
			slog.Default().Info("host: memapproval.RecordRequest (preference_save) failed",
				"session", scope.ID, "reqID", reqID, "err", err.Error())
		}
	}

	return pa, nil
}

// preferenceSaveLead renders the confirm card's headline. display is
// publisher-authored (the tool composed it, e.g. `Save "language: de" as
// your default for this agent?`) but MAY carry model-chosen (untrusted) text
// — the value the agent asked to save is embedded in it. That text is
// already sanitized and length-capped by renderHumanValue at the tool layer
// (pkg/agent/tool/meta/set_preference.go) before display reaches here, so
// this function uses it directly as the Lead rather than routing it through
// Excerpt the way a tool-call card's Fields would be.
func preferenceSaveLead(display string) string {
	display = strings.TrimSpace(display)
	if display == "" {
		return "Save this as your preference?"
	}
	return display
}

// currentTurnAuthorIdentity builds the channelevents.ExternalIdentity for
// Audience.Requester from a turn author's canonical subject, reusing
// speakerEmail's decode (speakerprofile.go) rather than re-deriving it: that
// helper already rejects the RawSubject-passthrough trap (a subject that
// LOOKS like an email because it was never base64-encoded in the first
// place) and a synthetic (email-less) canonical, both of which would
// otherwise slip an unverifiable value into a security-sensitive Requester.
//
// Kind="idp" + Email + ExternalID=email is deliberately the SAME shape
// maybeEchoAnnotationTurn (loop_inbox.go) builds for its echo Author:
// identity.Principal.Canonical() derives its canonical from Email alone once
// Email is non-empty (Kind/TeamScope/ExternalID are not consulted), so this
// reconciles with whatever channel actually delivers the decider's click at
// decision time (interaction_decision.go:126), regardless of its Kind.
//
// Returns nil when the author has no recoverable email — refusing to
// address a Requester nobody could ever be verified to decide.
//
// ASSUMPTION carried from speakerEmail's "@" test: no channel kind's
// externalID contains "@". A synthetic canonical decodes to
// "kind:teamScope:externalID"; if some kind's externalID were itself
// email-shaped, that decoded payload would contain "@" and pass here as an
// "email" like "slack:T1:foo@bar". The consequence is a wrong Email on the
// Requester — which FAILS CLOSED at decision time: DecideRequester
// (interaction_decision.go) canonicalizes that garbage and compares it
// against the real decider's canonical, which can never match, so nobody
// can approve the card; it expires. Wrong-but-safe, never an authorization
// widening.
func currentTurnAuthorIdentity(author identity.Subject) *channelevents.ExternalIdentity {
	email := speakerEmail(author)
	if email == "" {
		return nil
	}
	return &channelevents.ExternalIdentity{Kind: "idp", Email: identity.Email(email), ExternalID: identity.RawExternalID(email)}
}

// contentInspectionLead authors the headline the generic renderer shows as the
// prompt Lead. Its wording is pinned by
// pkg/channels/channelkinds/slack/content_inspection_characterization_test.go;
// the generic renderer additionally bolds it, the sole accepted render delta.
func contentInspectionLead(tool, point string, score, threshold float64, inspector string) string {
	return fmt.Sprintf("Possible prompt injection — `%s` %s scored %.2f (≥ %.2f) via `%s`. Allow it to proceed?",
		tool, ioWord(point), score, threshold, inspector)
}

// ioWord maps a content-guard inspection Point to input/output. The Point
// arrives over the wire as pipeline.Point's string form ("pre_tool_call"), NOT
// the Go identifier — compare against the constant.
// pkg/authz/contentguard/kinds/promptinjection holds the same two-branch mapping
// over the typed Point; a third Point needs an arm in both, and a miss silently
// labels it "output".
func ioWord(point string) string {
	if point == string(pipeline.PreToolCall) {
		return "input"
	}
	return "output"
}

// excerptOrNil wraps a non-empty raw excerpt in an InteractionExcerpt (the
// untrusted content the renderer inerts), or returns nil so no excerpt block is
// rendered. The renderer appends the trailing ":" to the Label.
func excerptOrNil(s string) *channelevents.InteractionExcerpt {
	if s == "" {
		return nil
	}
	return &channelevents.InteractionExcerpt{
		Label:   "Flagged content (untrusted — shown as inert text, not interpreted)",
		Content: s,
	}
}

// approveDenyActions returns the two decision buttons every approve/deny
// interaction carries. The IDs ("approve"/"deny") are what
// pipeline.ApprovalDecisionHandler switches on.
func approveDenyActions() []channelevents.InteractionAction {
	return []channelevents.InteractionAction{
		{ID: "approve", Label: "Approve", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
		{ID: "deny", Label: "Deny", Style: channelevents.ActionStyleDanger, Kind: channelevents.ActionKindDecision},
	}
}

func expiresAtPtr(t time.Time) *time.Time { return &t }

// resolveInteractionApprovers resolves a session approve-set + resource-owner
// sets into Audience.Approvers (canonical-Subject passthrough). Shared by the
// tool_call and info_leakage publishers. Returns nil with no error when
// SpiceDBLookupSubjects is unwired — callers fail closed on the empty set.
// authz.ResolveApprovers folds the two operands: non-empty resourceOwnerSets
// means the union of resource owners (the session set is NOT an operand); empty
// means the session approve-set.
func (h *runnerHost) resolveInteractionApprovers(ctx context.Context, sessionApproveSet string, resourceOwnerSets []string) ([]channelevents.ExternalIdentity, error) {
	if h.l.SpiceDBLookupSubjects == nil {
		return nil, nil
	}
	canons, _, err := authz.ResolveApprovers(ctx, loopLookuper{h.l.SpiceDBLookupSubjects}, sessionApproveSet, resourceOwnerSets)
	if err != nil {
		return nil, err
	}
	out := make([]channelevents.ExternalIdentity, 0, len(canons))
	for _, c := range canons {
		// Subject passthrough (invariant): c is a resolved canonical id, carried
		// via the Subject field — NEVER the raw-only ExternalID (stuffing a
		// canonical into ExternalID is the known silent-delivery bug). Kind stays
		// the session channel kind so the surface's kind gate matches.
		out = append(out, channelevents.ExternalIdentity{
			Kind:    identity.Kind(h.l.AddressableChannelKind()),
			Subject: identity.Subject(c),
		})
	}
	return out, nil
}

// approverSetDescription renders the approver pool as a human-readable string
// for the fail-closed "no resolvable approvers" error.
func approverSetDescription(sessionApproveSet string, resourceOwnerSets []string) string {
	if len(resourceOwnerSets) > 0 {
		return strings.Join(resourceOwnerSets, ", ")
	}
	return sessionApproveSet
}

// --- tool_approval render copy. This file owns the wording; the generic
// renderer supplies the block structure (bold Lead, "• *Label*: Value" Fields).
// ---

// toolApprovalLead is the headline the generic renderer bolds. No glyph of its
// own — the surface draws the tone chip from the category.
func toolApprovalLead() string { return "Approval needed" }

// toolApprovalCopy is everything the tool_approval card needs to speak
// English. Gathered into one struct because the alternative — five positional
// strings — is how the raw tool id and permission name ended up rendered
// verbatim: at four arguments nobody notices that two of them are identifiers.
type toolApprovalCopy struct {
	ToolName        string
	ToolDescription string
	Permission      string
	ResourceType    string
	ResourceID      string

	// PermissionTitle is the phrase the declaring SpiceDBPermission published
	// (AgentClass status.resolvedPermissionTitles), the SAME string the
	// plan-gate card shows. Empty when the type declared none, and the fallback
	// detokenizes rather than printing the wire name.
	PermissionTitle string

	// ResourceLabel is the instance as its type's Display derives it — a repo
	// URL shortened to its path, say. Empty when undeclared or underivable.
	ResourceLabel string

	Justification string
	What          string
}

// toolApprovalFields authors the Why / What / Tool / Permission rows of the
// approval card.
//
// NO WIRE IDENTIFIERS. This card used to print "Tool: `gitlike_git`" and
// "Permission: `push`" — an internal tool id and a SpiceDB permission name  14
// beside a plan-gate card that resolved both to declared human phrases. Two
// approval surfaces for the same call, one speaking English and one speaking
// schema.
func toolApprovalFields(c toolApprovalCopy) []channelevents.InteractionField {
	why := c.Justification
	if strings.TrimSpace(why) == "" {
		why = "_(no justification provided)_"
	}
	return []channelevents.InteractionField{
		{Label: "Why", Value: why},
		{Label: "What", Value: c.What},
		{Label: "Tool", Value: humanToolName(c.ToolName, c.ToolDescription)},
		{Label: "Permission", Value: humanPermission(c.Permission, c.ResourceType, c.PermissionTitle)},
	}
}

// humanToolName prefers the tool's own description, else a humanized form of
// its name. An underscored identifier is still an identifier.
func humanToolName(name, description string) string {
	if d := strings.TrimSpace(description); d != "" {
		return d
	}
	return detokenize(name)
}

// humanPermission prefers the declared title, else detokenizes the pair so an
// undeclared title degrades to plain English rather than to schema.
func humanPermission(permission, resourceType, title string) string {
	if t := strings.TrimSpace(title); t != "" {
		return t
	}
	if resourceType == "" {
		return detokenize(permission)
	}
	return detokenize(permission) + " on " + detokenize(resourceType)
}

// detokenize turns a wire identifier into words: underscores and hyphens become
// spaces. Deliberately not title-cased — these read inside a sentence.
func detokenize(s string) string {
	return strings.TrimSpace(strings.NewReplacer("_", " ", "-", " ").Replace(s))
}

// toolApprovalPublicNoteBody is the public "approval pending" note posted to the
// whole conversation. It says WHAT is being asked; the channel kind appends WHO,
// because resolving an approver to a mention needs a client this package does
// not have, and the ticker appends the elapsed suffix. No glyph here — the
// surface draws the tone from the category, so one glued on here would compete.
func toolApprovalPublicNoteBody(agentDisplayName, what string) string {
	agent := agentDisplayName
	if agent == "" {
		agent = "agent"
	}
	return fmt.Sprintf("*%s* wants to %s", agent, what)
}

// whatLine is the summarizer-LLM one-liner when present, else a deterministic
// fallback built from the same human copy the fields use.
//
// The old fallback read "Grant `push` on `git_repo:https=3A//github=2Ecom/o/r`."
//
//	14 a permission name, a wire type name, and a SpiceDB-escaped object id, all
//
// on the line a human decides from. Every one of those has a published human
// form; this uses them and keeps the escaped id out entirely.
func whatLine(summary string, c toolApprovalCopy) string {
	if s := strings.TrimSpace(summary); s != "" {
		return s
	}
	perm := humanPermission(c.Permission, c.ResourceType, c.PermissionTitle)
	target := strings.TrimSpace(c.ResourceLabel)
	if target == "" {
		// No derived label, so there is no instance to name. Never fall back to
		// the object id: it is SpiceDB-escaped and reads as corruption.
		//
		// Naming the type here only helps when a TITLE is carrying the phrase —
		// an untitled permission already reads "update on crm company", and
		// appending the type again says it twice.
		if c.PermissionTitle != "" && c.ResourceType != "" {
			return perm + " (the " + detokenize(c.ResourceType) + " this call names)."
		}
		return perm + "."
	}
	return perm + " — " + target + "."
}

// --- info_leakage render copy: the "Share request" framing + the "Would share
// with" recipient list. ---

// infoLeakageLead is the headline. Uses the summarizer's proposed-share text
// when present, else the "Share request" framing.
func infoLeakageLead(proposedText string) string {
	lead := strings.TrimSpace(proposedText)
	if lead == "" {
		lead = "Share request — a data owner's approval is needed before this data is shared."
	}
	return lead
}

// infoLeakageWouldShareWithFields NAMES the recipient(s) the proposed share
// would expose the data to (invariant: the data owner must see exactly who). The
// recipient list is rune-capped via the shared stringsx.CapRunes.
//
// Mentions carries the FULL structured identity alongside the flattened Value
// (standing rule: never flatten to a display string early). Every leakedTo entry
// is a resolved canonical subject ("user:<canonical>", the same
// Subject-passthrough shape resolveInteractionApprovers stamps), so it is
// carried verbatim via Subject, NEVER via ExternalID — stuffing a canonical into
// ExternalID is the known silent-delivery bug.
func infoLeakageWouldShareWithFields(channelKind string, leakedTo []string) []channelevents.InteractionField {
	names := make([]string, 0, len(leakedTo))
	mentions := make([]channelevents.ExternalIdentity, 0, len(leakedTo))
	for _, s := range leakedTo {
		names = append(names, strings.TrimPrefix(s, "user:"))
		mentions = append(mentions, channelevents.ExternalIdentity{
			Kind:    identity.Kind(channelKind),
			Subject: identity.Subject(s),
		})
	}
	return []channelevents.InteractionField{
		{
			Label:    "Would share with",
			Value:    stringsx.CapRunes(strings.Join(names, ", "), 2800),
			Mentions: mentions,
		},
	}
}

// toolCallPostApprove runs the post-approval re-check and
// schedules one-shot external grant cleanup.
func (h *runnerHost) toolCallPostApprove(ctx context.Context, tc *pendingToolCall) error {
	sessionRef := tc.sessNS + "/" + tc.sessName

	// Reaching this post-approve path IS the authorization for an External tool
	// — that is what StateImpact:External means. Re-running CheckToolCall here
	// would wrongly reject a call the human just approved, in the two shapes
	// that get here: an undeclared slot type has no grant to find (nothing was
	// written, because no slot_grant relation exists for it), and a declared one
	// whose grant carries External's 30s TTL can have expired between the card
	// and the decision. Skip the re-check; the one-shot cleanup below still runs.
	if tc.perm.StateImpact != authz.External {
		// Re-check the PER-CALL principal: the widget VIEWER on a proxy-exec
		// (app-tool) approval (tc.subject/tc.subjects, threaded at raise time),
		// else the loop's session subject on the LLM path. The proxy branch is
		// taken FIRST and reads NEITHER l.authSubject NOR l.authSubjects, so a
		// detached post-approve never races advanceRequester's between-turn
		// writes; the LLM path runs on the loop goroutine, where reading is safe.
		var checkSubject string
		var checkSubjects []string
		if tc.subject != "" {
			checkSubject = tc.subject
			checkSubjects = tc.subjects
		} else {
			checkSubject = h.l.authSubject.String()
			checkSubjects = h.l.authSubjects
		}
		retryIn := authz.Inputs{
			Args:              tc.argsMap,
			Subject:           checkSubject,
			Subjects:          checkSubjects,
			AgentSessionRef:   tc.sessNS + "/" + tc.sessName,
			SlotResourceTypes: h.l.PlanGateSlotTypes,
			// The slot grant was written by channelsd moments ago, in another
			// process. Without this the Check can run against a snapshot that
			// predates it and refuse the call the human just approved.
			RequireFreshest: true,
		}
		var retry authz.Result
		if h.l.Engine != nil {
			retry = h.l.Engine.CheckToolCall(ctx, tc.perm, retryIn)
		} else {
			retry = toolcheck.Checker{Cli: h.l.AuthzCli, Cache: h.l.AuthzCache}.CheckToolCall(ctx, tc.perm, retryIn)
		}
		if retry.IsError() {
			// NOT a read-after-write race: checkSessionGrant reads
			// FullyConsistent (head revision), so the grant tuple channelsd
			// wrote before it published the decision is always visible here.
			// A deny at this point means the grant does not SATISFY the check —
			// name that, and log the exact coordinates, so the next operator
			// greps the grant/permission shape instead of chasing consistency.
			resType, resID, permName := "", "", ""
			if tc.perm.Check != nil {
				resType = tc.perm.Check.ResourceType
				permName = tc.perm.Check.Permission
				if resolved, rerr := authz.ResolveResourceID(*tc.perm.Check, tc.argsMap); rerr == nil {
					resID = resolved
				}
			}
			slog.Default().Info("host: post-approve re-check denied despite an approved grant",
				"session", sessionRef, "tool", tc.toolName, "subject", checkSubject,
				"permission", permName, "resource", resType+":"+resID,
				"argsHash", tc.argsHash, "reason", retry.Message)
			return fmt.Errorf("approval recorded but the post-approval authorization re-check still denies this call: %s", retry.Message)
		}
	}

	// External: schedule one-shot grant cleanup.
	if tc.perm.StateImpact == authz.External && h.l.GuardianGrantWriter != nil {
		if tc.perm.Check != nil {
			resourceType := tc.perm.Check.ResourceType
			permission := tc.perm.Check.Permission
			resourceID := ""
			if resolved, err := authz.ResolveResourceID(*tc.perm.Check, tc.argsMap); err == nil {
				resourceID = resolved
			}
			if resourceType != "" && resourceID != "" && h.l.SlotRevoker != nil {
				// Revoke the SLOT grant the approval wrote. This used to delete
				// the session-grant tuple, which the approval flow no longer
				// writes — so it was deleting nothing, and an external effect's
				// grant lived until its 30s expiry rather than being withdrawn
				// the moment the one call it authorized had run.
				//
				// The expiry is still the backstop; this is the prompt path.
				binding := authz.SlotBinding{
					ResourceType: resourceType,
					// TrustedObjectID: resourceID came from authz.ResolveResourceID
					// above, which already ran the check's own transform chain
					// (ApplyTransforms) — already canonical.
					ResourceID: authz.TrustedObjectID(resourceID),
					Permission: permission,
				}
				revoker, ref := h.l.SlotRevoker, authz.SessionRef{Namespace: tc.sessNS, Name: tc.sessName}
				go func() {
					delCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					err := authz.RevokeSlots(delCtx, revoker, ref, []authz.SlotBinding{binding})
					besteffort.Log(slog.Default().Info, "revoke one-shot slot grant", err,
						"session", sessionRef, "permission", permission, "resource", resourceType+":"+resourceID)
				}()
			}
		}
	}
	return nil
}

// leakagePostApprove writes SpiceDB leakage grants.
func (h *runnerHost) leakagePostApprove(ctx context.Context, lp *pendingLeakageShare) error {
	if h.l.LeakageGrantWriter == nil {
		return fmt.Errorf("info-leakage: grant writer not wired; cannot persist approval")
	}
	grantResources := make([]approval.LeakageGrantResource, 0, len(lp.taint))
	for _, t := range lp.taint {
		grantResources = append(grantResources, approval.LeakageGrantResource{Type: t.ResourceType, ID: t.ResourceID})
	}
	if _, err := h.l.LeakageGrantWriter(ctx, lp.sessNS, lp.sessName, lp.leakedTo, grantResources, lp.ttl); err != nil {
		return fmt.Errorf("info-leakage: write grants: %w", err)
	}
	return nil
}

// auditLeakageDecision writes the leakage approval decision (leakage_approved /
// leakage_denied) to the info-leakage audit memory. The InfoLeakAudience hook
// emits the leakage_detected record at request time via Decision.Audit; this
// records the post-decision outcome. Best-effort with logging.
func (h *runnerHost) auditLeakageDecision(ctx context.Context, kind string, lp *pendingLeakageShare, approverID string) {
	if h.l.AuditMemoryAppend == nil || lp == nil {
		return
	}
	resources := make([]infoleakageaudit.ResourceRef, 0, len(lp.taint))
	for _, t := range lp.taint {
		resources = append(resources, infoleakageaudit.ResourceRef{Type: t.ResourceType, ID: t.ResourceID})
	}
	rec := infoleakageaudit.AuditRecord{
		At:        hostNow(),
		Kind:      kind,
		Session:   h.sess.Namespace + "/" + h.sess.Name,
		LeakedTo:  lp.leakedTo,
		Resources: resources,
		Details:   map[string]string{"approverID": approverID},
	}
	if err := h.l.AuditMemoryAppend(ctx, rec); err != nil {
		slog.Default().Info("host: leakage decision audit failed",
			"kind", kind, "session", rec.Session, "err", err.Error())
	}
}

// recordApprovalOutcome audits the tool_call approval outcome to memory.
func (h *runnerHost) recordApprovalOutcome(_ context.Context, tc *pendingToolCall, d approval.Decision, decision string) {
	if h.l.Mem == nil || tc == nil || tc.useID == "" {
		return
	}
	// Scope on the ENVELOPE's session (tc.sessNS/sessName), matching the
	// request-write path (buildToolCallPending), so a request and its
	// outcome land in the same scope even when ask.Payload overrode the
	// session — otherwise a future ByToolCall reader would never pair them.
	scope := memory.Scope{Kind: "session", ID: tc.sessNS + "/" + tc.sessName}
	if err := memapproval.RecordOutcome(context.Background(), h.l.Mem, scope, tc.useID, memapproval.Outcome{
		Decision: decision,
		Approver: d.ApproverID,
		Reason:   d.Reason,
	}); err != nil {
		slog.Default().Info("host: memapproval.RecordOutcome failed",
			"session", scope.ID, "err", err.Error())
	}
}

// approvalPauseCause maps a pending approval kind to the plan PauseCause shown
// on the plan card while the agent blocks awaiting the decision.
func approvalPauseCause(kind string) string {
	if kind == "leakage_share" {
		return channelevents.PauseCauseLeakageApproval
	}
	return channelevents.PauseCauseApproval
}

// --- Payload codec helpers ---

// extractCard pulls the rendered card out of the ask.
//
// Typed, not a map read: the gate puts a plangate.Card there, and accepting a
// loosely-shaped map as well would let a fixture assert against a shape the
// production path never produces.
func extractCard(m map[string]any) plangate.Card {
	if m == nil {
		return plangate.Card{}
	}
	c, _ := m["card"].(plangate.Card)
	return c
}

// planGateFields renders the card for a channel, preserving the split the card
// exists to maintain.
//
// The labels are load-bearing. "What" is computed from the frozen plan and the
// permission surface; the agent cannot author a byte of it. The justification is
// the agent's own text, so its label SAYS so — rendered unlabelled beside the
// computed description, a prompt-injected agent's prose would be
// indistinguishable from the system's own account of what it is asking for.
func planGateFields(c plangate.Card) []channelevents.InteractionField {
	var out []channelevents.InteractionField
	if strings.TrimSpace(c.What) != "" {
		// Value AND Items, saying the same thing. Value is what a plain-text
		// surface prints; Items is what a surface with a layout should render
		// instead of printing a paragraph of newline-joined prose.
		out = append(out, channelevents.InteractionField{
			Label: "What", Value: c.What, Items: planGateItems(c),
		})
	}
	if strings.TrimSpace(c.Why) != "" {
		out = append(out, channelevents.InteractionField{Label: "Why (the agent's words)", Value: c.Why})
	}
	if strings.TrimSpace(c.When) != "" {
		out = append(out, channelevents.InteractionField{Label: "When", Value: c.When})
	}
	if strings.TrimSpace(c.Approvers) != "" {
		out = append(out, channelevents.InteractionField{Label: "Who can approve", Value: c.Approvers})
	}
	return out
}

func extractString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func extractStringSlice(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	switch v := m[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// extractStringMap reads a map[string]string the payload may carry as either
// the typed map (in-process, which is how the gate publishes it) or the
// map[string]any shape a JSON round trip produces.
func extractStringMap(m map[string]any, key string) map[string]string {
	if m == nil {
		return nil
	}
	switch v := m[key].(type) {
	case map[string]string:
		return v
	case map[string]any:
		out := make(map[string]string, len(v))
		for k, raw := range v {
			if s, ok := raw.(string); ok {
				out[k] = s
			}
		}
		return out
	}
	return nil
}

// extractInt reads an int the payload may carry as any of the numeric shapes a
// map[string]any picks up in-process or after a JSON round trip.
func extractInt(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func extractIntSlice(m map[string]any, key string) []int {
	if m == nil {
		return nil
	}
	switch v := m[key].(type) {
	case []int:
		return v
	case []any:
		out := make([]int, 0, len(v))
		for _, item := range v {
			switch n := item.(type) {
			case int:
				out = append(out, n)
			case float64:
				out = append(out, int(n))
			}
		}
		return out
	}
	return nil
}

func extractAnyMap(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

// extractRawMessage reads a json.RawMessage the payload carries at key, or
// (nil, false) when absent or a different type. Used for preference_save's
// "value" entry, which preferencesSaver.Save stores as this exact type
// (json.RawMessage(value.Raw)) precisely so its ABSENCE (ok==false) is
// distinguishable from an explicit JSON null — a clear omits the key
// entirely rather than setting it to a null RawMessage.
func extractRawMessage(m map[string]any, key string) (json.RawMessage, bool) {
	if m == nil {
		return nil, false
	}
	v, ok := m[key].(json.RawMessage)
	if !ok {
		return nil, false
	}
	return v, true
}

func extractPermission(m map[string]any) authz.Permission {
	if m == nil {
		return authz.Permission{}
	}
	v, ok := m["perm"].(authz.Permission)
	if ok {
		return v
	}
	return authz.Permission{}
}

func extractTaintSlice(m map[string]any, key string) []leakageTaintRecord {
	if m == nil {
		return nil
	}
	if v, ok := m[key].([]leakageTaintRecord); ok {
		return v
	}
	return nil
}

// extractCallback returns a func(resourceType, resourceID) stored at key, or
// nil. Used for the leakage approval on_approved / on_denied callbacks that the
// host invokes after AwaitDecision to update the InfoLeakAudience hook's
// session-scoped approved/denied sets.
func extractCallback(m map[string]any, key string) func(string, string) {
	if m == nil {
		return nil
	}
	if v, ok := m[key].(func(string, string)); ok {
		return v
	}
	return nil
}

// ToolCallApprovalPayload is the strongly-typed payload for "tool_call" ApprovalAsk.
// Built by pipeline_wiring.go and decoded by the host.
type ToolCallApprovalPayload struct {
	SessNS           string
	SessName         string
	ToolName         string
	ToolDescription  string
	Permission       string
	ResourceType     string
	ResourceID       string
	StateImpact      string
	ArgsMap          map[string]any
	ArgsJSON         string
	ArgsHash         string
	Perm             authz.Permission
	ApproverSubject  string
	Requester        channelevents.ExternalIdentity
	Justification    string
	AgentDisplayName string
	Labels           map[string]string
	UseID            string
	// Subject + Subjects carry the proxy-exec VIEWER principal (D-D1) onto the
	// pending tool call so the post-approval re-verify checks
	// the viewer, not the loop's session subject. Empty on the LLM path.
	Subject  string
	Subjects []string
}

// ToMap converts a ToolCallApprovalPayload to map[string]any for embedding in
// pipeline.ApprovalAsk.Payload. Used by pipeline_wiring.go.
//
// "request_id" is intentionally omitted: the single source of truth is the reqID
// PublishApproval mints and registers with the approval orchestrator, which
// buildToolCallPending stamps as the envelope's RequestRef — so the published
// request id and the orchestrator's await key are always identical.
func (p ToolCallApprovalPayload) ToMap() map[string]any {
	return map[string]any{
		"sess_ns":            p.SessNS,
		"sess_name":          p.SessName,
		"tool_name":          p.ToolName,
		"tool_description":   p.ToolDescription,
		"permission":         p.Permission,
		"resource_type":      p.ResourceType,
		"resource_id":        p.ResourceID,
		"state_impact":       p.StateImpact,
		"args_map":           p.ArgsMap,
		"args_json":          p.ArgsJSON,
		"args_hash":          p.ArgsHash,
		"perm":               p.Perm,
		"approver_subject":   p.ApproverSubject,
		"requester":          p.Requester,
		"justification":      p.Justification,
		"agent_display_name": p.AgentDisplayName,
		"labels":             p.Labels,
		"use_id":             p.UseID,
		"subject":            p.Subject,
		"subjects":           p.Subjects,
	}
}

// buildPlanGatePending builds the pending approval for the plan gate's two
// categories: plan_phase (clear a declared phase to run) and plan_amendment
// (grant reach the approved plan does not hold).
//
// One builder for both because the shape is identical — a computed Lead, a
// severity-styled action pair, and the session approve-set as audience. Only
// the category and the styling input differ, so splitting them would duplicate
// the approver resolution and the envelope build for nothing.
func (h *runnerHost) buildPlanGatePending(
	ctx context.Context, reqID string, ask pipeline.ApprovalAsk, category string,
) (*pendingApproval, error) {
	if h.l.InteractionRequestPublish == nil {
		return nil, fmt.Errorf("%s approval unavailable (interaction publish hook not configured)", category)
	}
	sessNS, sessName := h.sess.Namespace, h.sess.Name

	// Session approve-set, with no resource-owner intersection: a plan ceiling
	// names KINDS of action, not one external resource, so there is no owner
	// counterparty to route consent to. Same reasoning as content_inspection.
	approveSet := "agentsession:" + sessNS + "/" + sessName + "#approve"
	var approvers []channelevents.ExternalIdentity
	if h.l.SpiceDBLookupSubjects != nil {
		canons, _, rerr := authz.ResolveApprovers(ctx, loopLookuper{h.l.SpiceDBLookupSubjects}, approveSet, nil)
		if rerr != nil {
			return nil, fmt.Errorf("resolve %s approvers: %w", category, rerr)
		}
		for _, c := range canons {
			approvers = append(approvers, channelevents.ExternalIdentity{
				Kind:    identity.Kind(h.l.ChannelKind),
				Subject: identity.Subject(c),
			})
		}
	}
	if len(approvers) == 0 {
		// Fail closed rather than publish a prompt nobody can answer — an
		// unanswerable gate is a hang, and Validate rejects an empty scope anyway.
		return nil, fmt.Errorf("%s approval has no resolvable approvers for %s", category, approveSet)
	}

	// Severity drives the styling, but never by recolouring the confirm button
	// red: an approval is a question, not a failure, at every level — that is
	// what sev.ApproveStyle() now always returns. The elevation signal instead
	// rides the card itself (rail, marker, external lines) where the reader is
	// actually deciding. sev.DenyStyle() is wired here too, computed from the
	// same recorded severity, so the styling signal survives all the way to the
	// channel instead of stopping at the audit log — and so neither button
	// trains the reflex that answering, rather than the thing being approved,
	// is the dangerous act.
	sev := plangate.Severity(extractString(ask.Payload, "severity"))
	actions := approveDenyActions()
	for i := range actions {
		switch actions[i].ID {
		case "approve":
			actions[i].Style = sev.ApproveStyle()
		case "deny":
			actions[i].Style = sev.DenyStyle()
		}
	}

	pl := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sessNS, Name: sessName},
		Category:        category,
		RequestRef:      reqID,
		// The COMPUTED summary, never the agent's words. The agent's own
		// justification rides in the card payload as its claim.
		Lead: ask.Summary,
		// The card itself. Without this the whole card reached the audit record
		// and stopped: the human decided on the lead sentence alone, which names
		// the request and its strongest impact tier but cannot say WHICH
		// resource, why the agent wants it, or whether saying yes finishes the
		// request.
		Fields:        planGateFields(extractCard(ask.Payload)),
		Actions:       actions,
		Audience:      channelevents.InteractionAudience{Scope: channelevents.AudienceApprovers, Approvers: approvers},
		ExpiresAt:     expiresAtPtr(hostNow().Add(h.l.resolvedApprovalTimeout())),
		Interruptible: false,
	}
	covered := extractCoveredPhases(ask.Payload)
	var consentRaw []json.RawMessage
	var approvedPhases map[string]bool
	if plan, declared := h.l.ActiveFrozenPlan(ctx); declared {
		state, err := plangate.Fold(plan, h.l.PlanGateRecords())
		if err != nil {
			return nil, fmt.Errorf("read existing plan consent authority: %w", err)
		}
		approvedPhases = make(map[string]bool)
		for i := range plan.Phases {
			approvedPhases[plan.Phases[i].AuthorityKey()] = state.PhaseApproved(i)
		}
	}
	for _, phase := range covered {
		// A new phase must not re-request an already approved phase's expired
		// consent. Keep its full authority in the audit coverage, while only
		// applying child decisions for phases that need this approval.
		if approvedPhases[phase.PhaseKey] {
			continue
		}
		consentRaw = append(consentRaw, phase.Consents...)
	}
	for _, raw := range consentRaw {
		var child channelevents.InteractionRequestPayload
		if err := json.Unmarshal(raw, &child); err != nil {
			return nil, fmt.Errorf("decode plan consent: %w", err)
		}
		if child.AgentSessionRef != pl.AgentSessionRef {
			return nil, fmt.Errorf("plan consent belongs to another session")
		}
		pl.Consents = append(pl.Consents, child)
		if child.ExpiresAt == nil {
			return nil, fmt.Errorf("plan consent has no expiry")
		}
		if child.ExpiresAt.Before(*pl.ExpiresAt) {
			pl.ExpiresAt = child.ExpiresAt
		}
	}
	if len(pl.Consents) > 0 {
		noun := "private reminder"
		if len(pl.Consents) != 1 {
			noun += "s"
		}
		pl.Lead = fmt.Sprintf("Approve this plan and %d %s?", len(pl.Consents), noun)
		pl.Body = "Authorizes the exact requests below. Each future session will ask for a fresh action plan before sending its reminder."
		owners := make(map[string]bool)
		for _, child := range pl.Consents {
			if child.Audience.Requester == nil {
				return nil, fmt.Errorf("plan consent has no private owner")
			}
			owner, err := child.Audience.Requester.Principal().Canonical()
			if err != nil {
				return nil, err
			}
			owners[owner.String()] = true
		}
		if len(owners) != 1 {
			return nil, fmt.Errorf("plan consents must address the same private owner")
		}
		var privateApprovers []channelevents.ExternalIdentity
		for _, approver := range pl.Audience.Approvers {
			canonical, err := approver.Principal().Canonical()
			if err != nil {
				return nil, err
			}
			if owners[canonical.String()] {
				privateApprovers = append(privateApprovers, approver)
			}
		}
		if len(privateApprovers) == 0 {
			return nil, fmt.Errorf("private reminder owner cannot approve this plan")
		}
		pl.Audience.Approvers = privateApprovers
		var err error
		pl.Details, err = json.Marshal(pl.Consents)
		if err != nil {
			return nil, err
		}
		pl.Fields = append(pl.Fields, channelevents.PlanConsentFields(pl.Consents)...)
		pl.Excerpt = channelevents.PlanConsentExcerpt(pl.Consents)
		if h.l.Mem == nil {
			return nil, fmt.Errorf("plan consent durable details unavailable")
		}
		scope := memory.Scope{Kind: "session", ID: sessNS + "/" + sessName}
		if err := memapproval.RecordRequest(ctx, h.l.Mem, scope, reqID, memapproval.Request{RequestID: reqID, Details: pl.Details}); err != nil {
			return nil, fmt.Errorf("record plan consent details: %w", err)
		}
	}
	env, eerr := channelevents.BuildEnvelope(sessNS, sessName, channelevents.KindInteractionRequest, pl)
	if eerr != nil {
		return nil, fmt.Errorf("build %s interaction envelope: %w", category, eerr)
	}

	publish := h.l.InteractionRequestPublish
	return &pendingApproval{
		kind:      category,
		expiresAt: pl.ExpiresAt,
		onPublish: func(c context.Context) error {
			return publish(c, sessNS, sessName, env)
		},
		planGatePayload: &pendingPlanGate{
			planDigest: extractString(ask.Payload, "planDigest"),
			phaseIndex: extractInt(ask.Payload, "phase"),
			phaseKey:   extractString(ask.Payload, "phaseKey"),
			ceiling:    extractStringSlice(ask.Payload, "ceiling"),
			handle:     extractString(ask.Payload, "handle"),
			slots:      extractStringSlice(ask.Payload, "slots"),
			slotValues: extractStringMap(ask.Payload, "slotValues"),
			maxCount:   extractInt(ask.Payload, "maxCount"),
			requires:   extractIntSlice(ask.Payload, "requires"),
			covered:    covered,
			consents:   consentRaw,
		},
	}, nil
}

// approverCanDelegateSlots filters the requested slot requests down to those
// this approver personally holds standing on.
//
// Asked per INSTANCE whenever the phase named one, because that is the axis the
// approval writes on: narrowToApproved binds a grant to the exact
// (resourceType, resourceID, permission) triple the phase named, so answering
// "do they hold this permission on ANY resource of this type" and then granting
// a SPECIFIC one turns standing on the company an approver owns into live
// access to the company they do not. Only a slot the phase DEFERRED — no id yet
// — falls back to the type-level question, and that case grants nothing
// instance-scoped because there is no id to bind to.
//
// Standing is asked per (resource, permission) PAIR, never per resource alone:
// a slot's grant confers the permission the class declared for it, and holding
// `read` on a repository says nothing about a slot whose grant confers `write`.
// A type whose permission the class does not declare cannot be checked, so it
// is held back rather than waved through — as is a request whose question this
// runner has no hook wired to ask.
//
// With NEITHER lookup wired, everything is delegable — the pre-existing
// behaviour, safe for the reason given on Loop.SpiceDBHasOnResource.
//
// The approver arrives as a PRINCIPAL, never as a display string. This used to
// take Decision.ApproverID — a raw external id — and wrap it in
// identity.CanonicalUserID, which is a TYPE, so the "conversion" encoded
// nothing: SpiceDB was asked about `user:admin@ap.local`, refused the object id
// as illegal, and the error discarded an approval a human had just given.
// The value is the set of PERMISSIONS the approver may delegate for that ref —
// not a bare "delegable" bool. A StandingRequired slot can declare a primary
// permission plus additional ones (its EffectivePermissions set), and the bind
// path writes a grant for every one it is handed; checking only the primary let
// an approver who holds `read` on a repo authorize the agent to `push` to it.
// So each permission the slot may confer is checked independently and only the
// held ones are returned; the caller binds exactly this set.
func (h *runnerHost) approverCanDelegateSlots(
	ctx context.Context, requested []plangateaudit.SlotRef, approver identity.Principal,
) (map[plangateaudit.SlotRef]map[string]struct{}, error) {
	out := make(map[plangateaudit.SlotRef]map[string]struct{}, len(requested))
	if len(requested) == 0 {
		return out, nil
	}
	if h.l.SpiceDBHasOnResource == nil && h.l.SpiceDBHasAnyOfType == nil {
		// The pre-existing "everything delegable" degrade: with no lookup wired,
		// every declared permission of each ref is delegable on the approver's
		// say-so.
		for _, ref := range requested {
			out[ref] = permSet(slotPermissionCeiling(h.l, ref.Type))
		}
		return out, nil
	}
	canonical, err := approver.Canonical()
	if err != nil {
		return nil, fmt.Errorf("canonical approver: %w", err)
	}
	if canonical.IsZero() {
		// No identifiable approver, so there is nobody to ask SpiceDB about.
		// Fail closed rather than treating an anonymous yes as authority to
		// delegate somebody else's resource.
		return nil, fmt.Errorf("the decision carried no identifiable approver, so no slot can be delegated")
	}

	session := h.sess.Namespace + "/" + h.sess.Name
	for _, ref := range requested {
		if _, done := out[ref]; done {
			// Deduped: a plan-scoped card covers several phases and they commonly
			// name the same instance, so asking once per DISTINCT request keeps a
			// human's click from waiting on repeated identical lookups.
			continue
		}
		perms := slotPermissionCeiling(h.l, ref.Type)
		if len(perms) == 0 {
			// The class declares no permission for this type, so there is no
			// question to ask SpiceDB and no basis to call it theirs.
			slog.Default().Info("plan_gate: slot type has no declared permission; "+
				"holding it back rather than treating it as delegable",
				"session", session, "slotType", ref.Type)
			continue
		}
		if h.l.PlanGateSlotStanding[ref.Type] != spiceboxv1alpha1.StandingRequired {
			// Session-only (the default for anything not explicitly `required`,
			// including a type absent from the map entirely — status may not have
			// published yet, and treating that window as strict would make every
			// slot unbindable until the controller catches up). SpiceDB holds no
			// upstream truth for this type — a git repository's permissions live at
			// the forge, never in a SpiceDB tuple — so there is no standing to check
			// and the approver's decision on the card is the authority.
			//
			// Ahead of the id-derivation step deliberately: a session-only type
			// needs no object id to ask SpiceDB about, so deriving first would let
			// an underivable value block a slot that never required a lookup.
			//
			// This does NOT weaken enforcement downstream: the grant this produces
			// is still written, still carries a mandatory expiry, is still scoped to
			// the session, is still revocable, and the tool-call Check still
			// resolves slot_grant_<perm>->interact + owner against it. The only
			// dropped step is "did the approver already hold it".
			out[ref] = permSet(perms)
			continue
		}
		var derived authz.ObjectID
		if ref.ID != "" {
			var derr error
			derived, derr = authz.NewObjectID(ref.ID, h.l.PlanGateSlotTransforms[ref.Type])
			if derr != nil {
				// The value the phase named cannot become an object id, so there
				// is no question to ask. Holding back is the only answer that is
				// not a guess in the permissive direction — and asking SpiceDB
				// about the raw value instead is exactly the bug this replaces:
				// it returns an InvalidArgument that discards a human's approval.
				slog.Default().Info("plan_gate: a named slot value could not be derived into an object id; "+
					"holding it back rather than asking about the raw value",
					"session", session, "slotType", ref.Type, "err", derr.Error())
				continue
			}
		}
		// The lookup MODE is a property of the ref, decided once; the permission
		// is what varies. A ref with no wired lookup is held back whole.
		useOnResource := ref.ID != "" && h.l.SpiceDBHasOnResource != nil
		switch {
		case useOnResource:
		case ref.ID != "":
			// The phase named an instance and this runner cannot ask about it.
			slog.Default().Info("plan_gate: no per-instance standing lookup is wired; "+
				"holding the named slot back rather than delegating it on type-level standing",
				"session", session, "slotType", ref.Type, "slotID", ref.ID)
			continue
		case h.l.SpiceDBHasAnyOfType != nil:
		default:
			slog.Default().Info("plan_gate: no type-level standing lookup is wired; "+
				"holding the deferred slot back rather than treating it as delegable",
				"session", session, "slotType", ref.Type)
			continue
		}
		// Ask about EACH permission the slot may confer, and keep only the held
		// ones — the fix for the over-grant: binding a permission the approver
		// does not hold hands the agent reach the approving human lacks.
		held := make(map[string]struct{}, len(perms))
		for _, perm := range perms {
			var ok bool
			var lookupErr error
			if useOnResource {
				ok, lookupErr = h.l.SpiceDBHasOnResource(ctx, ref.Type, derived.String(), perm, canonical)
			} else {
				ok, lookupErr = h.l.SpiceDBHasAnyOfType(ctx, ref.Type, perm, canonical)
			}
			if lookupErr != nil {
				return nil, fmt.Errorf("standing for slot %s#%s: %w", slotRefLabel(ref), perm, lookupErr)
			}
			if ok {
				held[perm] = struct{}{}
			}
		}
		if len(held) > 0 {
			out[ref] = held
		}
	}
	return out, nil
}

// slotRefLabel renders a slot request for a log or an error — `crm_company:4210`
// for a named instance, the bare type for one the phase deferred.
func slotRefLabel(ref plangateaudit.SlotRef) string {
	if ref.ID == "" {
		return ref.Type
	}
	return ref.Type + ":" + ref.ID
}

// slotRefLabels renders a whole request set, for the one-line log that says what
// an approval asked for against what it granted.
func slotRefLabels(refs []plangateaudit.SlotRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, slotRefLabel(r))
	}
	return out
}

// recordPlanGateDecision writes the human's answer into the plan gate's
// append-only log.
//
// This is what makes approval state durable. The gate holds nothing in memory
// by design — it FOLDS this log on every permissioned call — so a decision that
// is not written here does not exist: a five-call phase would publish five
// identical cards, the approval would not survive a runner restart or an
// idle-sleep re-hydrate, and IsDenied would never fire, leaving an agent free to
// retry a refusal until the human relented.
//
// Best-effort by the same rule as every other gate write: losing the record must
// not take down the turn, but it must never be silent, because a gate that
// cannot persist its own decisions is operating blind.
func (h *runnerHost) recordPlanGateDecision(
	ctx context.Context, pg *pendingPlanGate, approved, timedOut bool, approver identity.Principal,
) {
	if pg == nil || h.l == nil || h.l.Mem == nil {
		return
	}

	// A NO is one denial whatever the card covered. Approving is what clears
	// several phases; refusing refuses the call in front of the human, and the
	// route out is a re-plan — recording a denial per covered phase would key a
	// refusal to two phases nobody has reached yet.
	if !approved {
		rec := plangateaudit.Content{
			Event:      plangateaudit.EventDenied,
			Outcome:    plangateaudit.OutcomeDenied,
			PlanDigest: pg.planDigest,
			PhaseIndex: new(int32(pg.phaseIndex)),
			PhaseKey:   pg.phaseKey,
			Ceiling:    pg.ceiling,
			Slots:      pg.slots,
			MaxCount:   pg.maxCount,
			Requires:   pg.requires,
			Mode:       h.l.PlanGateMode,
			At:         time.Now().UTC(),
		}
		// A lapsed deadline is a verdict, not a decision, and the two must stay
		// distinguishable in the trail: nobody refused this, the clock did.
		rec.Provenance = "plan_gate:denied"
		if timedOut {
			rec.Provenance = "plan_gate:timeout"
		}
		h.writePlanGateDecision(ctx, rec)
		return
	}

	// Delegation, not elevation — the slot half. A session owner may approve a
	// plan, but only the portion they themselves hold, so what gets recorded as
	// granted is what the person who clicked can speak for. The rest stays held
	// for its resource owner and escalates when the agent actually reaches it.
	//
	// Bounded here rather than on the card, which is what makes partial approval
	// work on every transport: there is no subset for the human to compose and
	// no multi-select to render, so a channel that can only show approve/deny
	// still yields a correctly-bounded grant.
	//
	// Asked ONCE for the union and intersected per phase. A plan-scoped card
	// covers several phases that often request the same instance, and asking
	// SpiceDB per phase would repeat identical lookups while a human waits.
	delegable, err := h.approverCanDelegateSlots(ctx, unionSlotRefs(pg, h.l.PlanGateSlotStanding, ConstantInstancesOf(h.l.PlanGateSurface)), approver)
	if err != nil {
		// Fail closed on the GRANT. Reading an undeterminable subset as "they
		// hold everything" would turn a lookup blip into a grant nobody
		// authorized.
		slog.Default().Info("plan_gate: could not establish which slots the approver may delegate; "+
			"recording no approval and telling the approver their answer did not take",
			"session", h.sess.Namespace+"/"+h.sess.Name,
			"approver", approverIDOf(approver), "phase", pg.phaseIndex, "err", err.Error())
		// AND TELL THEM. Silently recording nothing leaves the person who just
		// clicked Approve looking at the same card again with no idea why —
		// they approve, it fails identically, and they approve again. A silent
		// loop is worse than a silent error: the system looks like it is working
		// and the human is the one being made to repeat themselves.
		//
		// A standing-lookup blip is a recoverable FAULT, not a pin refusal, so the
		// "fault on our side, try once more" copy is the honest one here.
		h.publishPlanGateApprovalFailed(ctx, err, false)
		return
	}

	for _, rec := range approvedPhaseRecords(pg) {
		rec.Event = plangateaudit.EventPhaseApproved
		rec.PlanDigest = pg.planDigest
		rec.Mode = h.l.PlanGateMode
		rec.Provenance = "plan_gate:approved"
		rec.At = time.Now().UTC()

		// BOTH spellings narrow together, from ONE filtered list, so they cannot
		// disagree: Slots is the type spelling every existing reader speaks and
		// SlotRefs is the instance spelling carry-over reads, and a record that
		// kept a ref the approver could not speak for would read as granting an
		// instance nobody delegated.
		requested := requestedSlotRefs(rec, pg, h.l.PlanGateSlotStanding, ConstantInstancesOf(h.l.PlanGateSurface))
		granted := make([]plangateaudit.SlotRef, 0, len(requested))
		for _, r := range requested {
			if _, ok := delegable[r]; ok {
				granted = append(granted, r)
			}
		}
		types := make([]string, 0, len(granted))
		for _, r := range granted {
			types = append(types, r.Type)
		}
		rec.Slots = types
		if len(rec.SlotRefs) > 0 {
			// Only a record that ALREADY carried refs gets them rewritten.
			// Synthesizing them onto a record frozen without any would change
			// what slotsFromRecord rebuilds, and the fold digests that.
			rec.SlotRefs = granted
		}

		// Writes the instance-scoped SpiceDB grant that makes the approved
		// instance the permitted one (a call against a DIFFERENT instance of
		// the same type has no grant to succeed on) and records the matching
		// session_scope entry alongside it — see narrowToApproved's doc comment
		// for what each half currently does and does not enforce. Written per
		// record, from THIS record's own refs, so it reflects both the
		// DELEGABLE subset (a request the approver could not speak for is not
		// bound to, because binding an instance nobody granted would deny the
		// type outright) and THIS PHASE'S instance — a plan-scoped card can
		// cover two phases that bind the same resource type to two different
		// instances (clone from upstream/repo, push to fork/repo), and each
		// must get its own grant, not just whichever phase raised the card.
		h.narrowToApproved(ctx, rec, pg, requested, granted, delegable)
		h.writePlanGateDecision(ctx, rec)
	}
}

// narrowToApproved writes an INSTANCE-SCOPED SpiceDB slot grant for the
// approved instances (via authz.BindApproved), and records their
// scope.Resources entries in session_scope (Layer 2) alongside it.
//
// Before this existed, a plan-gate approval wrote only an audit record — no
// SpiceDB grant at all — so an approved phase was authorization-inert: a
// permissioned call after approval still depended on some OTHER path (an
// owner tuple, a class default) to succeed. What this closes is that gap:
// the grant is scoped to the specific (resourceType, resourceID, permission)
// triple a human was shown, so a call against a DIFFERENT instance of the
// same type is refused for want of a grant on that instance.
//
// The session_scope write this also performs is NOT an independent
// enforcement layer today. scope.CheckScopeWithRefs — the function that
// would check a resolved resource ref against scope.Resources — has no
// caller in either live dispatch path (pkg/authz/check_tool_call.go's
// `check`, pkg/authz/hooks/scope.go's `Scope.Eval`); both call the ref-less
// scope.CheckScope instead. So this function's name should not be read as
// "the session can now only reach these instances" — that would require
// CheckScopeWithRefs to be wired in, which it is not. What IS true today is
// narrower and still real: the instance-scoped grant is the thing a
// different instance has none of.
//
// granted is THIS RECORD's own delegable slot requests — the instance each
// covered phase itself named, already filtered by what the approver may
// speak for. Per record rather than per card because a plan-scoped card
// covers several phases, and two of them can legitimately bind the SAME
// resource type to two DIFFERENT instances: reading the instance from one
// shared map keyed only on type would collapse every phase to whichever
// phase raised the card and silently deny the others' already-approved
// calls.
//
// requested is the same record's unfiltered set, carried only so the log
// below can say what the human's yes was asked to cover against what it
// actually bought.
//
// Best-effort by the same rule as the audit write — losing it must not take
// down the turn — but never silent: an approval that grants nothing at all
// is exactly the gap this change exists to close, so both a failure here and
// a yes that bound nothing have to be findable in the log.
func (h *runnerHost) narrowToApproved(
	ctx context.Context, rec plangateaudit.Content, pg *pendingPlanGate, requested, granted []plangateaudit.SlotRef,
	delegable map[plangateaudit.SlotRef]map[string]struct{},
) {
	if h.l.Mem == nil {
		return
	}
	// The permissions the approver actually holds, keyed by type+id — what
	// planGateBindings filters each binding against, so a permission the
	// approver was not verified for is never written even though it sits in the
	// phase ceiling. Keyed by (type,id) rather than the full SlotRef because
	// planGateBindings only has those two at the emit point (Standing is a
	// property of the type, so it cannot disambiguate).
	heldByTypeID := make(map[string]map[string]struct{}, len(delegable))
	for ref, perms := range delegable {
		heldByTypeID[ref.Type+"\x00"+ref.ID] = perms
	}
	// A ref with an empty ID is a target the phase deferred: there is no
	// instance to narrow to, the approval still stands, and the type stays
	// unnarrowed — the honest reading of "approved, target not yet named".
	// planGateBindings drops those and pairs every remaining permission with
	// the instance it actually resolves to, constant or declared.
	declaredPermissions := make(map[string][]string, len(granted))
	for _, r := range granted {
		declaredPermissions[r.Type] = slotPermissionCeiling(h.l, r.Type)
	}
	for _, raw := range rec.Ceiling {
		if hh, err := permsurface.ParseHandle(raw); err == nil {
			declaredPermissions[hh.ResourceType()] = slotPermissionCeiling(h.l, hh.ResourceType())
		}
	}
	bindings := planGateBindings(
		h.l.PlanGateSurface, pg, rec, granted, declaredPermissions, heldByTypeID, h.l.PlanGateSlotStanding, h.l.PlanGateSlotTransforms,
		h.planGateSlotPreconditions(), h.planGateSlotOccupancy())
	if len(bindings) == 0 {
		if len(requested) > 0 {
			// A human clicked Approve and the session gained no instance. That is
			// a legitimate outcome — the approver may hold none of what the phase
			// named, or the phase may have deferred every target — but it is also
			// what a mis-wired standing lookup looks like, and the two are
			// indistinguishable from silence. The phase itself is still cleared to
			// run; each named instance simply has to escalate on its own.
			slog.Default().Info("plan_gate: the approval was recorded but bound no instance; "+
				"the phase may run and every named resource still escalates per instance",
				"session", h.l.SessionKey.Namespace+"/"+h.l.SessionKey.Name,
				"phase", phaseIndexOf(rec), "phaseKey", rec.PhaseKey,
				"requested", slotRefLabels(requested), "granted", slotRefLabels(granted))
		}
		return
	}
	scp := memory.Scope{Kind: "session", ID: h.l.SessionKey.Namespace + "/" + h.l.SessionKey.Name}
	sess := authz.SessionRef{Namespace: h.l.SessionKey.Namespace, Name: h.l.SessionKey.Name}
	// Loop carries no bare SessionExpiration field; the same wall-clock cap the
	// JIT tool-approval path re-reads off the AgentSession status
	// (pkg/channelsd/pipeline/tool_approval_interaction.go's sessionExpirationOf)
	// is already resident here on Budget, loaded once at loop start
	// (cmd/runner/main.go), so read it from there instead of a second fetch.
	var sessionExpiration time.Duration
	if h.l.Budget != nil {
		sessionExpiration = h.l.Budget.cfg.SessionExpiration.Duration
	}
	// bindSlots reads session_scope through l.Mem directly (not over the
	// bearer-token HTTP path), so it needs the same system approval every
	// other direct l.Mem call in this file mints — see PlanGateRecords
	// (pipeline_wiring.go) for the sibling read on the same "plan_gate"
	// component.
	writeCtx := memory.WithSystemApproval(ctx, "plan_gate")
	// logr.FromContextOrDiscard, not logr.Discard: bindSlots' nil-writer branch
	// returns SUCCESS having granted nothing, and says so only via
	// logger.Info — discarding it would mean an approval can narrow scope with
	// no grant written and no trace of why, on top of the error path already
	// logged below.
	// EnforcePreconditions, never waived: a plan-phase approval consents to the
	// phase, not to the risk a slot precondition guards. Each binding carries its
	// type's compiled Requires (via planGateBindings), so an instance the phase
	// named whose gate is Refused is dropped here rather than bound — it stays
	// unbound and escalates to the human waiver card at its next tool call, which
	// is the one card that asks for risk consent. An instance with no precondition
	// (the overwhelmingly common phase) binds exactly as it did before.
	if err := authz.BindApproved(writeCtx, h.l.Mem, scp, sess, h.l.SlotBinder, bindings,
		authz.EnforcePreconditions,
		authz.SlotGrantExpiry(time.Now(), sessionExpiration), logr.FromContextOrDiscard(ctx), time.Now); err != nil {
		if errors.Is(err, authz.ErrSlotPinned) {
			// A pin refusal reached here one of two ways, and they are DIFFERENT
			// facts the approver needs told apart:
			//
			//   - a MOVE the card promised could not execute (its MUST_MATCH
			//     failed: the pin drifted since the card was shown). Nothing bound,
			//     and re-approving rebuilds the card from the current pin. This is
			//     a fault, not a refusal.
			//   - a plain pinned REFUSAL: a binding named a different instance of a
			//     slot already pinned to another, and no move was approved for it.
			//     GrantSlots is per-type partitioned, so OTHER types in the same
			//     approval may well have bound — "nothing was granted" would be a
			//     lie — and re-approving changes nothing.
			//
			// A move was attempted iff some binding carried a PriorID (set from the
			// record's MovedFrom at card-build).
			wasMove := anyBindingMovesAPin(bindings)
			slog.Default().Info("plan_gate: an approved slot bind was refused by the single-occupancy pin; "+
				"telling the approver",
				"session", scp.ID, "bindings", len(bindings), "wasMove", wasMove, "err", err.Error())
			// A drifted move is a recoverable fault (retry); a plain pinned refusal
			// (no move attempted) is an answer re-approving cannot change.
			h.publishPlanGateApprovalFailed(ctx, err, !wasMove)
			return
		}
		slog.Default().Info("plan_gate: the approval was recorded but did not narrow the session; "+
			"the approved instance is permitted and others are not excluded",
			"session", scp.ID, "bindings", len(bindings), "err", err.Error())
		return
	}
	// A successful approved bind — possibly a MOVE — supersedes any recorded
	// promotion refusal for these types: a refusal that said "pinned to A" must
	// not be appended to a later denial after an approved A→B move, where it
	// would name a pin that no longer exists.
	h.l.clearSlotPinRefusals(bindingResourceTypes(bindings))
	// Mirror what SpiceDB now holds onto status.slotPins — display-only,
	// best-effort, derived from a ReadPin per involved type rather than from
	// `bindings` (which BindApproved may have partially dropped and still
	// returned nil); see mirrorApprovedSlotPins' doc comment.
	mirrorApprovedSlotPins(ctx, h.l.Status, h.l.SlotBinder, sess, rec, bindings, time.Now())
}

// planGateSlotPreconditions returns the compiled precondition set per resource
// type the class declares, for the plan-gate bind to carry into BindApproved.
//
// Same source as dispatch's autofill (boundEntitySpecsForAutofill →
// slotspec.FromClass), so the rule that holds a candidate out of its slot at
// bind time and the one dispatch recomputes to explain the resulting refusal
// are the SAME compiled program — one derivation, two consumers.
//
// A class whose slot preconditions do not compile should already have been
// refused at admission, so an error here is defensive. It is logged loudly
// rather than swallowed, and the map is left empty: enforcement then finds no
// Requires and binds as it did before preconditions existed, which is
// fail-OPEN for a gated type — but it is the only answer that does not also
// wedge every ORDINARY phase (whose types declare no precondition), and the
// case is unreachable for a class that passed admission.
func (h *runnerHost) planGateSlotPreconditions() map[string][]precondition.Rule {
	if h.l == nil || h.l.AgentClass == nil {
		return nil
	}
	specs, err := boundEntitySpecsForAutofill(h.l.AgentClass)
	if err != nil {
		slog.Default().Info("plan_gate: the class's slot preconditions did not compile; "+
			"a plan-gate approval will bind without re-checking them",
			"session", h.l.SessionKey.Namespace+"/"+h.l.SessionKey.Name, "err", err.Error())
		return nil
	}
	out := make(map[string][]precondition.Rule, len(specs))
	for _, s := range specs {
		if len(s.Requires) > 0 {
			out[s.ResourceType] = s.Requires
		}
	}
	return out
}

// slotOccupancy is a slot's single-vs-multi commitment paired with its rebind
// policy — the two GrantSlots' pinning gate reads together. Both empty is the
// common (single, default-rebind) case and is left out of the by-type map.
type slotOccupancy struct {
	Occupancy string
	Rebind    string
}

// planGateSlotOccupancy returns the occupancy/rebind pair per resource type the
// class declares, for planGateBindings to stamp onto every binding it emits so
// the plan-gate approval's grant is pinned exactly as a non-approval bind of
// the same slot would be.
//
// Derived from the SAME boundEntitySpecsForAutofill → slotspec.FromClass source
// as planGateSlotPreconditions, so occupancy reaches the bind through the one
// converter that owns the AuthzSlot→BoundEntitySpec mapping — never a second
// hand-rolled read of the spec. A compile failure in the class's preconditions
// leaves the map empty (single/default for every type), matching
// planGateSlotPreconditions' defensive handling; the case is unreachable for a
// class that passed admission.
func (h *runnerHost) planGateSlotOccupancy() map[string]slotOccupancy {
	if h.l == nil || h.l.AgentClass == nil {
		return nil
	}
	specs, err := boundEntitySpecsForAutofill(h.l.AgentClass)
	if err != nil {
		return nil
	}
	out := make(map[string]slotOccupancy, len(specs))
	for _, s := range specs {
		if s.Occupancy != "" || s.Rebind != "" {
			out[s.ResourceType] = slotOccupancy{Occupancy: s.Occupancy, Rebind: s.Rebind}
		}
	}
	return out
}

// approvedPhaseRecords is what a yes writes: one record per covered phase, or
// the active phase alone when the ask carried no coverage (an older gate, or a
// card that only ever spoke for one phase).
func approvedPhaseRecords(pg *pendingPlanGate) []plangateaudit.Content {
	if len(pg.covered) > 0 {
		return pg.covered
	}
	return []plangateaudit.Content{{
		PhaseIndex: new(int32(pg.phaseIndex)),
		PhaseKey:   pg.phaseKey,
		Ceiling:    pg.ceiling,
		// The ONE handle an amendment added, empty on a plain phase ask. The
		// fold reads it to widen this phase for the rest of its life; without it
		// the approval clears the call in front of it and nothing more.
		Handle:   pg.handle,
		Slots:    pg.slots,
		Consents: pg.consents,
		MaxCount: pg.maxCount,
		Requires: pg.requires,
	}}
}

// requestedSlotRefs is one record's slot requests WITH the instance each names.
//
// SlotRefs when the record carries them — plangate.PhaseAuthorityRecord writes
// Slots and SlotRefs as index-aligned pairs, one entry per phase slot, so a
// phase naming two instances of the same type is representable and must not be
// collapsed to one.
//
// pg.slotValues is the fallback for the one shape with no per-record refs:
// approvedPhaseRecords' non-covered branch (a plain plan_phase ask, or a
// plan_amendment) synthesizes a record from pg's own fields and never sets
// SlotRefs, so the single active phase's instance is all there is. A type with
// no entry there yields an empty ID, which reads as "deferred" everywhere
// downstream — the honest reading of a phase that named no target.
//
// standing is the SAME per-type map the live decision reads
// (Loop.PlanGateSlotStanding), threaded in so a fallback ref carries how its
// approval got its authority — v1alpha1.StandingSessionOnly (a human vouched)
// or StandingRequired (a human delegated something they already held) — the
// same way plangate.PhaseAuthorityRecord already stamps it onto a covered
// record at freeze time. Without it, a fallback ref's Standing stayed at its
// zero value, indistinguishable in the append-only log from a record written
// before the field existed.
func requestedSlotRefs(rec plangateaudit.Content, pg *pendingPlanGate, standing, constants map[string]string) []plangateaudit.SlotRef {
	if len(rec.SlotRefs) > 0 {
		return rec.SlotRefs
	}
	out := make([]plangateaudit.SlotRef, 0, len(rec.Slots))
	for _, t := range rec.Slots {
		id := pg.slotValues[t]
		if id == "" {
			// The plan named no instance for this type — which for an amendment
			// is the NORMAL case, not a deferral: an amendment adds a permission,
			// and the instance that permission reaches may be one the agent is
			// structurally unable to name. A constant id is fixed by the toolkit
			// (git_repo read/write key on the literal "workspace"), so it appears
			// in no plan and cannot be deferred to a later prompt either.
			//
			// Without this the ref carried a bare type, narrowToApproved bound
			// nothing, and every subsequent call escalated per instance — the
			// human clicking Approve on the same amendment forever.
			//
			// Still empty when the type has no constant, which keeps the honest
			// "approved, target not yet named" reading for a genuinely deferred
			// target.
			id = constants[t]
		}
		out = append(out, plangateaudit.SlotRef{Type: t, ID: id, Standing: standing[t]})
	}
	return out
}

// unionSlotRefs is every slot request any covered phase asked to touch — the one
// question put to SpiceDB about this approver.
//
// Refs, not bare types: standing is asked per instance now, so collapsing two
// phases' different instances of one type into a single type-level question is
// exactly the widening the per-instance check exists to prevent.
//
// standing is passed straight through to requestedSlotRefs — see its doc
// comment. It must be the SAME map the caller also passes to
// requestedSlotRefs for each individual record afterward: both derive a
// SlotRef's Standing from it, and the two outputs are compared as map keys
// (the delegable set built from this function's result, looked up by the
// refs requestedSlotRefs returns per phase) — a mismatched map would make
// refs that name the same request compare unequal.
func unionSlotRefs(pg *pendingPlanGate, standing, constants map[string]string) []plangateaudit.SlotRef {
	seen := map[plangateaudit.SlotRef]struct{}{}
	var out []plangateaudit.SlotRef
	for _, rec := range approvedPhaseRecords(pg) {
		for _, r := range requestedSlotRefs(rec, pg, standing, constants) {
			if _, dup := seen[r]; dup {
				continue
			}
			seen[r] = struct{}{}
			out = append(out, r)
		}
	}
	return out
}

// phaseIndexOf reads a record's phase for a log line, -1 when it carries none.
func phaseIndexOf(rec plangateaudit.Content) int {
	if rec.PhaseIndex == nil {
		return -1
	}
	return int(*rec.PhaseIndex)
}

// writePlanGateDecision persists one decision record, loudly on failure: a gate
// that cannot persist its own decisions is operating blind, and the phase will
// simply be re-asked.
func (h *runnerHost) writePlanGateDecision(ctx context.Context, rec plangateaudit.Content) {
	if err := (planGateRecorder{l: h.l}).Record(ctx, rec); err != nil {
		phase := phaseIndexOf(rec)
		slog.Default().Info("plan_gate: recording the human decision failed; "+
			"the approval will not survive a fold and the phase will be re-asked",
			"session", h.sess.Namespace+"/"+h.sess.Name,
			"event", rec.Event,
			"phase", phase,
			"digest", rec.PlanDigest,
			"err", err.Error())
	}
}

// extractCoveredPhases reads the phases one answer clears off the ask.
//
// The payload never leaves the process — the gate builds the Decision and the
// host consumes it — so the records arrive as themselves. A caller that puts
// anything else under this key is a wiring bug, and reading nothing leaves the
// active-phase fallback, which is the pre-plan-scoped behaviour.
func extractCoveredPhases(m map[string]any) []plangateaudit.Content {
	if m == nil {
		return nil
	}
	c, _ := m["covered"].([]plangateaudit.Content)
	return c
}

// boundPermissionsFor is the set of permissions an approval actually named for
// one resource type — what a slot grant must be written for.
//
// A slot grant is keyed (instance, permission), so binding the wrong permission
// writes a grant the approved call can never spend and the call escalates again
// on its next attempt. Two shapes:
//
//   - An AMENDMENT names exactly one handle (pg.handle). That handle's
//     permission is the answer, whatever the slot was declared for: the card
//     said "add Read the checked-out files", so read is what the human granted.
//   - A PLAN or PHASE ask carries a ceiling. Every permission in it that belongs
//     to this type was on the card the human approved, so all of them bind.
//
// Falls back to the slot's declared permission when neither yields anything,
// which keeps a phase that named a type but no matching handle behaving exactly
// as it did before.
//
// This is what "adding read adds it to the slot" means mechanically: the slot
// is no longer a single (type, permission) pair frozen at declaration time, it
// accrues the permissions successive approvals put on it.
// rec is the record being bound, and its ceiling is what decides — NOT the
// pending ask's. One click on a plan-scoped card covers every phase, and this
// runs once per covered record; reading pg.ceiling here bound every phase with
// the permissions of whichever phase happened to raise the card. A 2-phase
// plan then granted phase 1's fetch and read on the repository and nothing for
// phase 2, so its push and write escalated per call and the human approved a
// plan they had already approved. The INSTANCES were per-record all along —
// only the permissions were not.
func boundPermissionsFor(pg *pendingPlanGate, rec plangateaudit.Content, resourceType string, declared []string) []string {
	allowed := map[string]bool{}
	for _, p := range declared {
		allowed[p] = true
	}
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		// Filtered through the slot's DECLARED set. A permission the slot never
		// declared has no slot_grant_<permission> relation in the composed
		// schema, and including it fails the whole grant write at SpiceDB —
		// costing the approval even the permissions that WERE declared.
		if p == "" || seen[p] || !allowed[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	// An amendment adds ONE permission, to ONE phase. Applying it to every
	// record a plan-scoped approval covers would grant the other phases a
	// permission nobody asked for them to have.
	if pg != nil && pg.handle != "" && amendmentTouches(pg, rec) {
		if h, err := permsurface.ParseHandle(pg.handle); err == nil && h.ResourceType() == resourceType {
			add(h.Permission())
		}
	}
	if len(out) == 0 {
		// THIS record's ceiling first. pg.ceiling is the fallback for a record
		// frozen without one (approvedPhaseRecords synthesizes such a record
		// from the pending ask, where the two are the same list anyway).
		ceiling := rec.Ceiling
		if len(ceiling) == 0 && pg != nil {
			ceiling = pg.ceiling
		}
		for _, raw := range ceiling {
			h, err := permsurface.ParseHandle(raw)
			if err != nil || h.ResourceType() != resourceType {
				continue
			}
			add(h.Permission())
		}
	}
	if len(out) == 0 && len(declared) > 0 {
		// Nothing named this type — a phase that declared a slot but whose
		// ceiling holds no handle for it. Bind the slot's primary permission,
		// which is exactly what this did before an approval could name any, so a
		// shape that worked keeps working.
		//
		// EffectivePermissions puts the singular Permission field first, so for
		// every class written before slots could carry a set this IS that field.
		add(declared[0])
	}
	return out
}

// permSet turns a permission list into a set, for the delegable map's value.
func permSet(perms []string) map[string]struct{} {
	out := make(map[string]struct{}, len(perms))
	for _, p := range perms {
		out[p] = struct{}{}
	}
	return out
}

// slotPermissionCeiling is every permission the class declared for this slot
// type, falling back to the singular field for a class written before slots
// could name more than one.
func slotPermissionCeiling(l *Loop, resourceType string) []string {
	if set := l.PlanGateSlotPermissionSets[resourceType]; len(set) > 0 {
		return set
	}
	if p := l.PlanGateSlotPermissions[resourceType]; p != "" {
		return []string{p}
	}
	return nil
}

// amendmentTouches reports whether rec is the phase the pending amendment
// amends. When either side cannot identify its phase the answer is yes, which
// preserves the single-phase behavior that predates plan-scoped coverage —
// there, the only record IS the amended phase.
func amendmentTouches(pg *pendingPlanGate, rec plangateaudit.Content) bool {
	if pg == nil {
		return false
	}
	if pg.phaseKey == "" || rec.PhaseKey == "" {
		return true
	}
	return pg.phaseKey == rec.PhaseKey
}

// anyBindingMovesAPin reports whether any binding in the set is a pin MOVE —
// carrying a PriorID set at card-build from the approval record's MovedFrom.
// The plan-gate approval-failed notice branches its copy on this: a move that
// could not execute (the pin drifted) is a fault that re-approving can fix,
// while a plain pinned refusal is a refusal that re-approving cannot.
func anyBindingMovesAPin(bindings []authz.SlotBinding) bool {
	for _, b := range bindings {
		if !b.PriorID.IsZero() {
			return true
		}
	}
	return false
}

// planGateBindings decides the exact (type, instance, permission) triples an
// approved record grants.
//
// The rule is one line and both halves matter: EACH PERMISSION BINDS THE
// INSTANCE IT RESOLVES TO. A permission whose check is answerable with no tool
// args names the same object on every call (git.yaml keys git_repo read/write
// on "workspace", the session's checked-out copy); every other permission takes
// its instance from the slot the agent declared.
//
// Pairing every permission with the declared slot — what this did before — got
// both directions wrong at once. The constant instance received no grant, so
// the first call needing it raised an amendment for a resource the card had
// already listed and promised to cover; and the declared instance received
// grants for permissions that never resolve to it, which no check can spend.
//
// Pure and separated from narrowToApproved so the decision is testable without
// a memory store, a SpiceDB writer, or a session.
func planGateBindings(
	surface []permsurface.Descriptor,
	pg *pendingPlanGate,
	rec plangateaudit.Content,
	granted []plangateaudit.SlotRef,
	declaredPermissions map[string][]string,
	// heldByTypeID is the permissions the approver was verified to hold, keyed
	// by type+"\x00"+rawID. A binding is written only for a permission present
	// here for its exact instance — the fix for the over-grant where a
	// read-holding approver's click bound push.
	heldByTypeID map[string]map[string]struct{},
	// standing is the per-type standing map. A constant instance skips the
	// held-set filter (session-only, the approver's decision is authority) ONLY
	// when its type is not StandingRequired — nothing structurally forbids a
	// StandingRequired type from carrying a constant id, and skipping the filter
	// for one would let an approver's click grant reach on a shared resource
	// they hold nothing on.
	standing map[string]string,
	transforms map[string][]string,
	requires map[string][]precondition.Rule,
	// occupancy is the per-type single-vs-multi commitment and rebind policy,
	// stamped onto every emitted binding so the plan-gate grant is pinned the
	// same way a non-approval bind of the slot would be. A type absent from the
	// map is single with the default rebind — the fail-closed reading.
	occupancy map[string]slotOccupancy,
) []authz.SlotBinding {
	// The object id each (type, permission) pair fixes in advance, if any.
	constants := map[string]string{}
	for _, d := range surface {
		if d.ConstantResourceID != "" {
			constants[d.ResourceType+"/"+d.Permission] = d.ConstantResourceID
		}
	}

	// Declared instances per type, in the order the approval granted them.
	declaredIDs := map[string][]string{}
	for _, r := range granted {
		if r.ID != "" {
			declaredIDs[r.Type] = append(declaredIDs[r.Type], r.ID)
		}
	}

	// The prior instance a MOVE displaces, keyed by (type, raw moved-to id) —
	// taken ONLY from the record's MovedFrom, which is what the card showed the
	// approver, NEVER a live pin read here. A card can sit parked for days; the
	// human must execute exactly the move they saw, and a pin that drifted
	// meanwhile makes MovePin's MUST_MATCH fail loudly rather than moving
	// something unseen. Keyed by (type,id) rather than type alone so a move is
	// stamped only onto the binding whose instance IS the moved-to one — never a
	// constant instance of the same type, which this approval did not move.
	// MovedFrom is already a canonical object id (read from the pin), so
	// TrustedObjectID is correct: there is no transform to re-apply.
	priorByTypeID := map[string]authz.ObjectID{}
	for _, r := range granted {
		if r.MovedFrom != "" && r.ID != "" {
			priorByTypeID[r.Type+"\x00"+r.ID] = authz.TrustedObjectID(r.MovedFrom)
		}
	}

	// Every type this record touches: one the approval granted an instance for,
	// or one reached only through a constant.
	types := map[string]bool{}
	for _, r := range granted {
		types[r.Type] = true
	}
	for _, raw := range rec.Ceiling {
		if h, err := permsurface.ParseHandle(raw); err == nil {
			types[h.ResourceType()] = true
		}
	}

	seen := map[string]bool{}
	var out []authz.SlotBinding
	for resourceType := range types {
		for _, perm := range boundPermissionsFor(pg, rec, resourceType, declaredPermissions[resourceType]) {
			ids := declaredIDs[resourceType]
			// A CONSTANT instance (a check resolvable with no tool args, e.g.
			// git_repo:workspace) is session-only: it carries no upstream
			// standing, so the approver's click is the whole authority and the
			// held-set filter below must NOT apply to it. It is also absent from
			// the delegable map (the declared ref is keyed by the phase's named
			// instance, not the constant), so filtering it would wrongly drop
			// it and force a second per-call approval.
			isConstant := false
			if c := constants[resourceType+"/"+perm]; c != "" {
				// Fixed in advance, so it does not vary with the declared slots
				// and must be bound once rather than once per slot.
				ids = []string{c}
				isConstant = true
			}
			// The held-filter is skipped ONLY for a session-only constant — the
			// approver's decision is the whole authority there, and the constant
			// is often absent from the held map (the declared ref is keyed by the
			// phase's named instance). A StandingRequired type that happens to
			// carry a constant id is still filtered: skipping it would grant reach
			// on a shared resource the approver holds nothing on.
			skipFilter := isConstant && standing[resourceType] != spiceboxv1alpha1.StandingRequired
			for _, raw := range ids {
				// The approver-standing filter, per (instance, permission). A
				// permission the approver was not verified to hold on THIS
				// instance is not bound, even though it sits in the phase
				// ceiling — the over-grant fix. Fail-closed: a (type,id) absent
				// from the held map (never delegable) binds nothing.
				if !skipFilter {
					if _, ok := heldByTypeID[resourceType+"\x00"+raw][perm]; !ok {
						continue
					}
				}
				id, err := authz.NewObjectID(raw, transforms[resourceType])
				if err != nil {
					// Never silent: an approved instance that cannot become an
					// id binds nothing, and the human is owed a reason it did
					// not take effect.
					slog.Default().Info("plan_gate: an approved instance could not be derived into an object id; "+
						"it binds nothing and will escalate per call",
						"slotType", resourceType, "permission", perm, "err", err.Error())
					continue
				}
				key := resourceType + "\x00" + id.String() + "\x00" + perm
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, authz.SlotBinding{
					ResourceType: resourceType, ResourceID: id, Permission: perm,
					// The RAW value (before transforms) and the type's compiled
					// preconditions travel with the binding so BindApproved can
					// re-evaluate the gate: it reads facts keyed by RawID and drops
					// a Refused/Undetermined instance rather than binding it. Empty
					// requires (the common case) makes that a no-op.
					RawID: raw, Requires: requires[resourceType],
					// Occupancy/Rebind travel too, so GrantSlots pins a
					// single-occupancy type's grant rather than writing it unpinned.
					Occupancy: occupancy[resourceType].Occupancy,
					Rebind:    occupancy[resourceType].Rebind,
					// PriorID, when the card recorded a move for THIS instance,
					// makes BindApproved repoint the pin off the displaced instance
					// and revoke its grants. Zero for a first-fill — a record with
					// no MovedFrom never yields a move, whatever the live pin is.
					PriorID: priorByTypeID[resourceType+"\x00"+raw],
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ResourceType != out[j].ResourceType {
			return out[i].ResourceType < out[j].ResourceType
		}
		if out[i].ResourceID.String() != out[j].ResourceID.String() {
			return out[i].ResourceID.String() < out[j].ResourceID.String()
		}
		return out[i].Permission < out[j].Permission
	})
	return out
}

// resourceDisplayLabel derives an instance's human label from its type's
// declared Display, the same derivation the plan-gate card's resource lines
// use. Empty when the type declared no display, or when the deriver cannot
// shorten this value — the caller then names the type in words instead of
// falling back to the escaped object id.
func resourceDisplayLabel(displays map[string]plangate.ResourceDisplay, resourceType, resourceID string) string {
	d, ok := displays[resourceType]
	if !ok || resourceID == "" {
		return ""
	}
	return plangate.DeriveLabel(d.Label, resourceID)
}
