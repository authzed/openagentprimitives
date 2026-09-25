package main

// approval_applied.go owns the oap.session.<ns>.<name>.in.metaagent_approval_applied
// route: the human's approve/deny click on a metaagent scope-approval block.
//
// The click is authorized (agentsession#manage_scope = owner) and then routed to
// the in-process approval orchestrator, where the goroutine blocked in
// Orchestrator.Await resolves the request.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/coldstart"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/metaagentaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// approvalOrchestrator is the subset of *approval.Orchestrator this route needs.
// Declared as an interface so the route is unit-testable and so a typed-nil
// *Orchestrator is never assigned into an interface field (AGENTS.md
// nil-interface rule — newMetaagentApprovalRoute only assigns a non-nil one).
type approvalOrchestrator interface {
	DeliverDecision(requestID string, d approval.Decision)
	PendingRequestIDsForSession(sessionRef string) []string
}

// metaagentApprovalRoute carries everything the approval-applied subscription
// callback needs. It is built once at startup and shared by every message.
type metaagentApprovalRoute struct {
	worker  *MetaagentWorker
	orch    approvalOrchestrator
	checker authz.ManageScopeChecker
	// delivered is shared by every message (the route is built once), hence a
	// pointer on a value type.
	delivered *deliveredIDs
}

// newMetaagentApprovalRoute wires the route from the metaagent worker (which
// owns the orchestrator, the NATS conn and the Metaagent) plus the mandatory
// manage_scope checker.
func newMetaagentApprovalRoute(w *MetaagentWorker, checker authz.ManageScopeChecker) metaagentApprovalRoute {
	r := metaagentApprovalRoute{worker: w, checker: checker, delivered: newDeliveredIDs(deliveredIDsCap)}
	if w != nil && w.approvalOrch != nil {
		r.orch = w.approvalOrch
	}
	return r
}

// deliveredIDsCap bounds the recently-delivered set. A repeat click arrives
// within seconds, i.e. within a handful of intervening approvals; the cap keeps
// a long-lived authzd from accumulating one entry per approval forever.
const deliveredIDsCap = 256

// deliveredIDs remembers the request ids THIS process handed to a live waiter.
//
// It closes a window the durable already-resolved check cannot: between the
// waiter receiving the decision and the Apply stage writing the outcome record,
// a second click would find no waiter AND no outcome record, and would re-drive
// a decision that is already being applied. Safe for concurrent use.
type deliveredIDs struct {
	mu    sync.Mutex
	cap   int
	ids   map[string]struct{}
	order []string
}

func newDeliveredIDs(capacity int) *deliveredIDs {
	return &deliveredIDs{cap: capacity, ids: make(map[string]struct{}, capacity)}
}

func (d *deliveredIDs) add(id string) {
	if d == nil || id == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, dup := d.ids[id]; dup {
		return
	}
	d.ids[id] = struct{}{}
	d.order = append(d.order, id)
	if len(d.order) > d.cap {
		delete(d.ids, d.order[0])
		d.order = d.order[1:]
	}
}

func (d *deliveredIDs) has(id string) bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.ids[id]
	return ok
}

// handle processes one in.metaagent_approval_applied message.
func (r metaagentApprovalRoute) handle(ctx context.Context, subject string, data []byte) {
	// The session comes from the SUBJECT and nothing else. It is what the
	// manage_scope gate below is keyed on, so a subject accepted loosely would
	// authorize the clicker against a DIFFERENT session's owner tuple; the
	// decode enforces the full grammar and refuses an envelope-shaped body whose
	// session claim disagrees.
	msg, err := channelevents.DecodeMetaagentIn(subject, data, channelevents.KindMetaagentApprovalApplied)
	if err != nil {
		slog.Warn("metaagent approval: message refused before the gate; dropping",
			"subject", subject, "err", err.Error())
		return
	}
	ns, name := msg.Namespace, msg.Name

	var payload channelevents.MetaagentApprovalAppliedPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		slog.Warn("metaagent: malformed approval payload", "session", ns+"/"+name, "err", err.Error())
		return
	}
	// Authorize the clicker before delivering the decision. The approve/deny
	// block is non-ephemeral and visible to the whole thread; without this gate
	// ANY thread viewer could resolve a scope change matched by RequestID alone.
	// manage_scope == owner == approve (all `= owner` in the schema), so
	// CheckManageScope is the correct owner-only gate. channelsd resolved +
	// published the email-canonical subject (approverCanonical) so this matches
	// the operator-written owner tuple.
	if !authorizeMetaagentClicker(ctx, r.checker, ns, name, payload.ApproverCanonical) {
		slog.Warn("metaagent approval: clicker is not a session owner; dropping",
			"session", ns+"/"+name, "clicker", payload.ApproverCanonical)
		// Best-effort deny notice to the clicker (same sink as NoticePublish).
		r.notice(ctx, ns, name, payload.ApproverID,
			"You are not authorized to approve this scope change.")
		return
	}

	dec := approval.Decision{
		Approved:   payload.Approved,
		ApproverID: payload.ApproverID,
		Action:     payload.Action,
	}
	if r.orch == nil {
		slog.Error("metaagent approval: no approval orchestrator wired; decision dropped",
			"session", ns+"/"+name, "requestId", payload.RequestID)
		r.notice(ctx, ns, name, payload.ApproverID,
			"Your scope decision could not be recorded. Please send the request again.")
		return
	}
	if r.hasWaiter(ns, name, payload.RequestID) {
		r.delivered.add(payload.RequestID)
		r.orch.DeliverDecision(payload.RequestID, dec)
		return
	}
	if r.delivered.has(payload.RequestID) {
		slog.Info("metaagent approval: this request was already resolved in-process; ignoring the repeat click",
			"session", ns+"/"+name, "requestId", payload.RequestID)
		r.notice(ctx, ns, name, payload.ApproverID, "This scope decision has already been recorded.")
		return
	}

	// No waiter. The request was published by an authzd process that is now
	// gone (its Await goroutine, and the 24h deadline it owned, died with it),
	// or it has already been resolved. Both are re-drive cases: the durable
	// metaagent_audit request record is the source of truth, and the
	// already-resolved case is caught by the outcome record written against the
	// same request id.
	slog.Info("metaagent approval: no in-process waiter; resolving from the durable request record",
		"session", ns+"/"+name, "requestId", payload.RequestID, "approved", payload.Approved)
	res, err := r.worker.redriveApproval(ctx, ns, name, payload.RequestID, dec)
	switch {
	case err != nil:
		slog.Error("metaagent approval: could not resolve the decision from the durable record",
			"session", ns+"/"+name, "requestId", payload.RequestID, "err", err.Error())
		msg := redriveUserMessage(err)
		r.notice(ctx, ns, name, payload.ApproverID, msg)
		r.noticeRequester(ctx, ns, name, res.requester, payload.ApproverID, msg)
	case res.outcome == redriveAlreadyResolved:
		slog.Info("metaagent approval: decision already recorded for this request; not applying again",
			"session", ns+"/"+name, "requestId", payload.RequestID)
		r.notice(ctx, ns, name, payload.ApproverID, "This scope decision has already been recorded.")
	default:
		slog.Info("metaagent approval: decision resolved from the durable request record",
			"session", ns+"/"+name, "requestId", payload.RequestID, "approved", payload.Approved)
	}
}

// hasWaiter reports whether some goroutine in THIS process is blocked in
// Orchestrator.Await for requestID.
//
// The check is not racy in the direction that matters: Await registers the
// request BEFORE its OnPublish sends the envelope the human clicks, so a
// registration can never still be in flight when the click for it arrives.
// "Absent" therefore means either never-registered-here (a restart) or
// already-resolved — never not-yet-registered.
func (r metaagentApprovalRoute) hasWaiter(ns, name, requestID string) bool {
	return slices.Contains(r.orch.PendingRequestIDsForSession(ns+"/"+name), requestID)
}

// noticeRequester delivers a notice to the original requester through the
// metaagent's own notice path (which also records it in the metaagent thread).
// Skipped when the requester is unknown or is the clicker, who was told already.
func (r metaagentApprovalRoute) noticeRequester(ctx context.Context, ns, name, requester, clicker, body string) {
	if requester == "" || requester == clicker || r.worker == nil || r.worker.mg == nil {
		return
	}
	r.worker.mg.notify(ctx, memory.Scope{Kind: "session", ID: ns + "/" + name}, requester, body)
}

// notice publishes a best-effort user-facing message on out.metaagent_notice.
// recipient is a channel-native user id (the clicker's), which channelsd passes
// straight to the channel; an empty recipient is skipped (nothing to address).
func (r metaagentApprovalRoute) notice(ctx context.Context, ns, name, recipient, body string) {
	if recipient == "" {
		slog.Info("metaagent approval: no recipient for notice; not published",
			"session", ns+"/"+name, "body", body)
		return
	}
	var nc *nats.Conn
	if r.worker != nil {
		nc = r.worker.natsConn
	}
	if err := publishMetaagentNotice(nc, ns+"/"+name, recipient, body); err != nil {
		slog.Info("metaagent approval: notice publish failed",
			"session", ns+"/"+name, "recipient", recipient, "err", err.Error())
	}
}

// Sentinel reasons a re-drive cannot restore a decision. Each maps to a
// user-facing message: the click must never end in silence.
var (
	errRedriveNoRecord = errors.New("no durable request record for this approval")
	errRedriveExpired  = errors.New("approval window elapsed before the decision arrived")
	errRedriveNoDelta  = errors.New("request record carries no classified delta to apply")
)

// coldStartRedriveWindow bounds how late a cold-start click may still be
// re-driven. The class's configured authz.approvalTimeout is not part of the
// durable record, so this mirrors approvalTimeoutFor's fallback: conservative on
// purpose, because past it the runner's own WaitForColdStartTask deadline has
// halted the session and applying scope would change a session nobody is
// waiting on. A click refused here still gets told to ask again.
const coldStartRedriveWindow = 10 * time.Minute

func redriveWindowFor(coldStart bool) time.Duration {
	if coldStart {
		return coldStartRedriveWindow
	}
	return hooks.MidSessionApprovalTimeout
}

type redriveOutcome int

const (
	redriveFailed redriveOutcome = iota
	redriveApplied
	redriveAlreadyResolved
)

// redriveResult carries what the caller needs to address the humans involved.
type redriveResult struct {
	outcome redriveOutcome
	// requester is the original requester from the durable record, "" when the
	// record could not be read.
	requester string
}

// redriveApproval resolves a metaagent scope decision whose in-process await no
// longer exists, from the durable metaagent_audit request record.
//
// It runs the SAME hooks.MetaagentApply stage the live lifecycle runs (via
// metaagentApplyDeps), seeded with the classified delta and cleaned task the
// record persisted — so a re-driven approve, deny, or run-without-scope does
// exactly what it would have done had the process never restarted. It does NOT
// re-run the extractor: the human approved a specific delta, and re-deriving it
// from the request text could apply something they never saw.
func (w *MetaagentWorker) redriveApproval(ctx context.Context, ns, name, requestID string, dec approval.Decision) (redriveResult, error) {
	if w == nil || w.mg == nil || w.mg.Memory == nil {
		return redriveResult{}, fmt.Errorf("metaagent re-drive: memory not wired")
	}
	if requestID == "" {
		return redriveResult{}, fmt.Errorf("metaagent re-drive: empty request id")
	}
	scopeRef := memory.Scope{Kind: "session", ID: ns + "/" + name}

	rec, err := metaagentaudit.RequestByID(ctx, w.mg.Memory, scopeRef, requestID)
	if err != nil {
		return redriveResult{}, fmt.Errorf("metaagent re-drive: read request record: %w", err)
	}
	if rec == nil {
		return redriveResult{}, errRedriveNoRecord
	}
	res := redriveResult{requester: rec.Requester}

	decided, err := decisionRecorded(ctx, w.mg.Memory, scopeRef, requestID)
	if err != nil {
		return res, fmt.Errorf("metaagent re-drive: read outcome records: %w", err)
	}
	if decided {
		res.outcome = redriveAlreadyResolved
		return res, nil
	}
	if age := time.Since(rec.Ts); !rec.Ts.IsZero() && age > redriveWindowFor(rec.ColdStart) {
		return res, fmt.Errorf("%w (requested %s ago)", errRedriveExpired, age.Round(time.Second))
	}

	// cold_start carries the five-way action; mid_session is a plain two-way bool.
	approved, action := dec.Approved, ""
	if rec.ColdStart {
		action = coldstart.ActionForDecision(dec)
		approved = action == coldstart.ActionApproveCleaned || action == coldstart.ActionApproveOriginal
	}
	if approved && rec.Classification.Applied.IsEmpty() {
		// Written before the classification was persisted, so there is nothing to
		// apply. Refuse loudly rather than report an approve that changed nothing.
		return res, errRedriveNoDelta
	}

	req := metaagentRequest{
		scopeRef:  scopeRef,
		requester: rec.Requester,
		text:      rec.RequestText,
		coldStart: rec.ColdStart,
	}
	kind, trigger := "mid_session", "mention"
	if rec.ColdStart {
		kind, trigger = "cold_start", "session_start"
	}
	// Seed the Host scratch the Apply stage reads, standing in for the Extract
	// and Decide stages the previous process already ran.
	host := w.newAuthzdHost(req)
	host.SetKind(kind)
	host.SetExtract(rec.Classification.Applied, "", rec.CleanedTask)
	host.SetApproved(approved, action)

	in := pipeline.Input{
		Session: pipeline.SessionRef{Namespace: ns, Name: name},
		// Input.Requester is deliberately left unset: it exists for the Received
		// stage's manage_scope gate, which ran (and passed) before the request was
		// published. The clicker was just re-authorized on the same permission.
		Metaagent: &pipeline.MetaagentInfo{
			Kind:      kind,
			Trigger:   trigger,
			Requester: rec.Requester,
			Text:      rec.RequestText,
			InboxIdx:  0,
		},
	}
	reg := pipeline.NewRegistry()
	reg.Register(hooks.NewMetaagentApply(
		w.metaagentApplyDeps(req, host, func() string { return requestID })), hooks.OrderMetaagentApply)
	if out, _ := pipeline.NewExecutor(reg).Run(ctx, pipeline.MetaagentApply, in, host); out.Verdict != pipeline.Allow {
		slog.Info("metaagent re-drive: apply stage non-Allow",
			"session", scopeRef.ID, "requestId", requestID,
			"verdict", out.Verdict, "reason", out.Reason, "hook", out.FiredHook)
	}
	res.outcome = redriveApplied
	return res, nil
}

// decisionRecorded reports whether an OUTCOME record already exists for
// requestID. Outcome records carry a non-empty ApproverDecision; the
// request-time record carries the id with no decision. This is what stops a
// second click — or a click that raced a live resolve — from applying the same
// delta twice.
func decisionRecorded(ctx context.Context, mem memory.Memory, scopeRef memory.Scope, requestID string) (bool, error) {
	recs, err := metaagentaudit.List(ctx, mem, scopeRef)
	if err != nil {
		return false, err
	}
	for _, r := range recs {
		if r.RequestID == requestID && r.ApproverDecision != "" {
			return true, nil
		}
	}
	return false, nil
}

// redriveUserMessage maps a re-drive failure to what the humans are told. The
// wording stays at the level of the thing they asked for — no component names,
// no internals.
func redriveUserMessage(err error) string {
	switch {
	case errors.Is(err, errRedriveNoRecord):
		return "This scope request is no longer available. Please send it again."
	case errors.Is(err, errRedriveExpired):
		return "This scope request expired before it was decided. Please send it again."
	case errors.Is(err, errRedriveNoDelta):
		return "This scope request could not be restored. Please send it again."
	}
	return "Your scope decision could not be applied. Please send the request again."
}

// publishMetaagentNotice sends one out.metaagent_notice payload for the session
// whose memory.Scope.ID is scopeID ("ns/name").
//
// recipient is a CHANNEL-NATIVE user id (the clicker's), which is why it lands
// in the payload's `requester` slot rather than `requesterCanonical`: the
// channel kind passes this one straight to its addressing API. The operator's
// fork-deny notice, which holds only a canonical subject, uses the other slot.
func publishMetaagentNotice(nc *nats.Conn, scopeID, recipient, body string) error {
	if nc == nil {
		return fmt.Errorf("metaagent notice: nats not wired")
	}
	ns, name, err := metaagentScopeSession(scopeID)
	if err != nil {
		return fmt.Errorf("metaagent notice: %w", err)
	}
	return channelevents.PublishMetaagentOut(nc.Publish, ns, name,
		channelevents.KindMetaagentNotice,
		channelkinds.MetaagentNoticePayload{Requester: recipient, Body: body})
}
