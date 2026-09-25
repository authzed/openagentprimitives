// Package pipelinehost is authzd's implementation of pipeline.Host. It owns the
// approval round-trip for the two ApprovalAsk kinds authzd raises:
//
//   - cold_start — publish the metaagent_scope_approval envelope, await, map the
//     decision through coldstart.ActionForDecision (five actions), then apply the
//     scope and write the cold_start_task.
//   - metaagent_scope — the mid-session @metaagent gate. Same envelope minus the
//     coldStart key (channelsd branches on that key to pick the 3-button render
//     over the 5-button one) and a plain two-way approve/deny. It owns no effect;
//     the MetaagentApply hook mutates scope and converts a deny to a sticky
//     HardDeny.
//
// Both paths treat an await error (ctx timeout, orchestrator failure) as a
// sticky deny returned with err=nil, so the executor Denies rather than Halts.
// The envelope's field names are load-bearing: channelsd renders the approval
// buttons off them.
//
// The apply / write / notice closures come from internal/cmd/authzd/main.go, so this
// package imports no cmd/* and forms no cycle with the hooks package. The
// authzd/ path level scopes it to one daemon — every other package under
// pkg/authz is shared by the runner, channelsd and the operator — and mirrors
// the sibling pkg/channels/channelsd/pipelinehost.
package pipelinehost

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz/coldstart"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// SessionRef identifies the session for envelopes and log scope.
type SessionRef struct {
	Namespace string
	Name      string
}

func (s SessionRef) String() string { return s.Namespace + "/" + s.Name }

// orchestrator is the approval-await surface the host needs. *approval.Orchestrator
// satisfies it; tests use a fake.
type orchestrator interface {
	Await(ctx context.Context, r approval.Request) (approval.Decision, error)
}

// Deps configures the authzd Host. The apply / write / notice closures are
// supplied by internal/cmd/authzd/main.go so this package never imports package main.
type Deps struct {
	Session SessionRef

	// Orchestrator is the approval orchestrator the host's AwaitDecision blocks
	// on (the SAME one internal/cmd/authzd's metaagent_approval_applied subscriber feeds
	// via DeliverDecision). Declared as an interface so a typed-nil pointer is
	// never assigned (AGENTS.md nil-interface rule).
	Orchestrator orchestrator

	// Publish sends the metaagent_scope_approval envelope to channelsd. Wraps the
	// NATS publish on the out.metaagent_scope_approval subject. Invoked inside the
	// orchestrator's OnPublish (after the request is registered).
	Publish func(ctx context.Context, payload []byte) error

	// ApplyScope applies the (already-classified) delta to the session scope.
	// Wraps Metaagent.ApplyScopeChange. Called inside AwaitDecision on an approve.
	ApplyScope func(ctx context.Context, d scope.ScopeDelta) error

	// WriteTask persists the cold_start_task outcome. Wraps coldstarttask.Put.
	WriteTask func(ctx context.Context, c coldstarttask.Content) error

	// NotifyRequester delivers a requester-facing confirmation/deny notice.
	// Wraps Metaagent.notify. Used by AwaitDecision per-outcome.
	NotifyRequester func(ctx context.Context, body string)

	// Audit writes one metaagent_audit record for the resolved approval outcome
	// (wraps auditColdStart). appliedDelta is non-nil only on an approve that
	// applied scope. Best-effort; nil disables.
	Audit func(ctx context.Context, action string, appliedDelta *scope.ScopeDelta)

	// NoticePublish backs the pipeline.Host.Notify method (the executor's
	// Decision.Notices). Wraps the out.metaagent_notice publish.
	NoticePublish func(ctx context.Context, requester, body string) error

	Logger *slog.Logger
	Now    func() time.Time
}

// approval kinds the Host serves. Both are internal to authzd, never on the
// wire: cold_start drives the five-way action mapping plus the apply + task
// write effect; metaagent_scope (mid-session @metaagent) only resolves the
// decision, leaving apply and deny-conversion to MetaagentApply.
const (
	kindColdStart      = "cold_start"
	kindMetaagentScope = "metaagent_scope"
)

// pendingApproval carries the per-request state AwaitDecision needs to publish
// the envelope and (for cold-start) run the post-approval effect. kind tags
// which dispatch AwaitDecision takes; the cold-start-only fields (cleanedTask,
// applied, inboxIdx) are unused for metaagent_scope.
type pendingApproval struct {
	kind        string
	requester   string
	verbatim    string
	cleanedTask string
	applied     scope.ScopeDelta
	inboxIdx    int
	// payload is the pre-built metaagent_scope_approval payload (with requestId
	// stamped) the OnPublish callback sends.
	payload []byte
}

// metaagentScratch carries the accumulating control-plane state across the four
// staged metaagent hooks, threaded per-request through one Host. It mirrors
// runnerHost.coldStartPlacement* (pkg/agent/runner/host.go): a mutex-guarded
// side-channel the generic Decision must NOT carry (it holds domain types
// pkg/platform/pipeline can't import). MetaagentReceived sets kind; MetaagentExtract sets
// delta/shape/cleaned; MetaagentDecide sets output + approved; MetaagentApply
// reads them. Set*/Peek* are the only accessors (Peek, not Take: a stage may
// re-read prior state without clearing it — the Host is single-use per request).
type metaagentScratch struct {
	mu       sync.Mutex
	kind     string                // Received: cold_start | mid_session
	delta    scope.ScopeDelta      // Extract: classified-applied delta carried to Decide/Apply
	shape    string                // Extract: widen | narrow | mixed | ""
	cleaned  string                // Extract: cleaned task (cold_start only)
	output   scope.MetaagentOutput // Decide: composed approver output
	approved bool                  // Decide/Apply: resolved approval bool
	action   string                // Decide/Apply: resolved cold-start 5-way action (coldstart.Action*)
}

// Host implements pipeline.Host for authzd's metaagent control-plane points and
// resolves the cold_start / metaagent_scope approvals.
type Host struct {
	d Deps

	mu      sync.Mutex
	pending map[string]*pendingApproval
	// reqID is the id minted for this Host's approval, kept after the pending
	// entry is consumed so the later stages can stamp it onto the durable
	// outcome record. That stamp is what links an outcome back to its
	// request-time metaagent_audit entry — and therefore what lets a
	// post-restart re-drive tell "not yet decided" from "already decided".
	// The Host is single-use per request, so there is only ever one.
	reqID string

	scratch metaagentScratch
}

// New constructs an authzd pipeline Host.
func New(d Deps) *Host {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Host{d: d, pending: map[string]*pendingApproval{}}
}

// SetKind records the classified metaagent kind (Received → scratch).
func (h *Host) SetKind(kind string) {
	h.scratch.mu.Lock()
	defer h.scratch.mu.Unlock()
	h.scratch.kind = kind
}

// PeekKind returns the classified kind ("" before Received ran).
func (h *Host) PeekKind() string {
	h.scratch.mu.Lock()
	defer h.scratch.mu.Unlock()
	return h.scratch.kind
}

// SetExtract records the classified delta, derived shape, and cleaned task
// (Extract → scratch).
func (h *Host) SetExtract(delta scope.ScopeDelta, shape, cleaned string) {
	h.scratch.mu.Lock()
	defer h.scratch.mu.Unlock()
	h.scratch.delta = delta
	h.scratch.shape = shape
	h.scratch.cleaned = cleaned
}

// PeekExtract returns the delta/shape/cleaned written by Extract.
func (h *Host) PeekExtract() (scope.ScopeDelta, string, string) {
	h.scratch.mu.Lock()
	defer h.scratch.mu.Unlock()
	return h.scratch.delta, h.scratch.shape, h.scratch.cleaned
}

// SetDecide records the composed approver output (Decide → scratch).
func (h *Host) SetDecide(out scope.MetaagentOutput) {
	h.scratch.mu.Lock()
	defer h.scratch.mu.Unlock()
	h.scratch.output = out
}

// PeekDecide returns the composed output written by Decide.
func (h *Host) PeekDecide() scope.MetaagentOutput {
	h.scratch.mu.Lock()
	defer h.scratch.mu.Unlock()
	return h.scratch.output
}

// SetApproved records the resolved approval bool + the cold-start 5-way action
// (Decide/Apply → scratch). action is "" for mid_session (a two-way approve/deny).
func (h *Host) SetApproved(approved bool, action string) {
	h.scratch.mu.Lock()
	defer h.scratch.mu.Unlock()
	h.scratch.approved = approved
	h.scratch.action = action
}

// PeekApproved returns the resolved approval bool + the cold-start action
// (false/"" before Decide ran).
func (h *Host) PeekApproved() (approved bool, action string) {
	h.scratch.mu.Lock()
	defer h.scratch.mu.Unlock()
	return h.scratch.approved, h.scratch.action
}

// PublishApproval mints a reqID, builds the metaagent_scope_approval payload
// (with requestId stamped so the envelope and the orchestrator await key match),
// and stores pending state. The envelope is not sent here: it goes out inside
// AwaitDecision's OnPublish, so the request is always registered with the
// orchestrator BEFORE a decision for it can arrive.
func (h *Host) PublishApproval(_ context.Context, ask pipeline.ApprovalAsk) (string, error) {
	switch ask.Kind {
	case kindColdStart:
		return h.publishColdStart(ask)
	case kindMetaagentScope:
		return h.publishMetaagentScope(ask)
	default:
		return "", fmt.Errorf("authzd host: unsupported approval kind %q", ask.Kind)
	}
}

// publishColdStart mints a cold-start reqID and builds the 5-button
// metaagent_scope_approval payload (coldStart:true + cleanedTask).
func (h *Host) publishColdStart(ask pipeline.ApprovalAsk) (string, error) {
	reqID := fmt.Sprintf("metaagent-coldstart-%s-%d", h.d.Session.String(), h.d.Now().UnixNano())

	pl := scope.MetaagentApprovalPayloadFromAsk(ask.Payload)
	pl.RequestID = reqID
	// The approval KIND decides the render, not the ask's payload: this method
	// only ever publishes the five-button cold-start prompt.
	pl.ColdStart = true

	pc := &pendingApproval{
		kind:        kindColdStart,
		requester:   pl.Requester,
		verbatim:    pl.Verbatim,
		cleanedTask: pl.CleanedTask,
		applied:     pl.Applied,
		inboxIdx:    pl.InboxIdx,
	}

	payload, err := json.Marshal(pl)
	if err != nil {
		return "", fmt.Errorf("authzd host: marshal cold-start approval payload: %w", err)
	}
	pc.payload = payload

	h.mu.Lock()
	h.pending[reqID] = pc
	h.reqID = reqID
	h.mu.Unlock()
	return reqID, nil
}

// publishMetaagentScope mints a mid-session reqID (metaagent-scope- prefix) and
// builds the 3-button metaagent_scope_approval payload: the 9-field set with NO
// coldStart / cleanedTask key, so channelsd's `if pl.ColdStart` stays false and
// it renders the three mid-session actions. The Host only resolves the decision
// on this path — MetaagentApply owns the apply and the deny→sticky-HardDeny
// conversion — so no applied/inboxIdx effect state is kept.
func (h *Host) publishMetaagentScope(ask pipeline.ApprovalAsk) (string, error) {
	reqID := fmt.Sprintf("metaagent-scope-%s-%d", h.d.Session.String(), h.d.Now().UnixNano())

	pl := scope.MetaagentApprovalPayloadFromAsk(ask.Payload)
	pl.RequestID = reqID
	// The approval KIND decides the render, not the ask's payload: clearing the
	// cold-start fields keeps them off the wire, which is how channelsd knows to
	// draw the three-button mid-session block.
	pl.ColdStart = false
	pl.CleanedTask = ""

	pm := &pendingApproval{
		kind:      kindMetaagentScope,
		requester: pl.Requester,
		verbatim:  pl.Verbatim,
	}

	payload, err := json.Marshal(pl)
	if err != nil {
		return "", fmt.Errorf("authzd host: marshal metaagent-scope approval payload: %w", err)
	}
	pm.payload = payload

	h.mu.Lock()
	h.pending[reqID] = pm
	h.reqID = reqID
	h.mu.Unlock()
	return reqID, nil
}

// RequestID returns the approval request id this Host minted, or "" when no
// approval has been published (auto-apply, or a stage that failed before
// Decide). Stable for the life of the Host.
func (h *Host) RequestID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reqID
}

// AwaitDecision blocks on the orchestrator, then OWNS the five-way action
// mapping + the apply + cold_start_task write effect (mirroring
// runnerHost.toolCallPostApprove). Returns approved=true on approve_*, false on
// run_without_scope / deny / await-error (sticky deny). A nil error is returned
// even on await failure: an await error maps to deny, NOT a host-primitive
// failure (which the executor would turn into Halt).
func (h *Host) AwaitDecision(ctx context.Context, reqID string, timeout time.Duration) (bool, string, bool, error) {
	h.mu.Lock()
	pa, ok := h.pending[reqID]
	if ok {
		delete(h.pending, reqID)
	}
	h.mu.Unlock()
	if !ok {
		return false, "", false, fmt.Errorf("authzd host: no pending approval for request %q", reqID)
	}
	if h.d.Orchestrator == nil {
		return false, "", false, fmt.Errorf("authzd host: orchestrator not wired; cannot await decision %q", reqID)
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	publishFn := h.d.Publish
	dec, awaitErr := h.d.Orchestrator.Await(waitCtx, approval.Request{
		RequestID:  reqID,
		SessionRef: h.d.Session.String(),
		OnPublish: func(pubCtx context.Context) error {
			if publishFn == nil {
				return fmt.Errorf("authzd host: approval publish func not wired")
			}
			return publishFn(pubCtx, pa.payload)
		},
	})

	// authzd deliberately maps EVERY await error (timeout OR transport) to a
	// sticky deny with err=nil (never Halt). Surfacing timedOut lets the executor
	// classify a lapse as a timeout; the deny outcome is unchanged.
	timedOut := pipeline.IsTimeout(awaitErr)
	if pa.kind == kindMetaagentScope {
		approvedOut, by, err := h.resolveMetaagentScope(reqID, dec, awaitErr)
		return approvedOut, by, timedOut, err
	}
	approvedOut, by, err := h.resolveColdStart(ctx, reqID, pa, dec, awaitErr)
	return approvedOut, by, timedOut, err
}

// ResolveApproval is the EFFECT-FREE approval resolver used by the staged
// metaagent lifecycle (MetaagentDecide). It publishes the byte-identical
// ask payload (cold_start 5-button / metaagent_scope 3-button — the SAME
// PublishApproval path), awaits the human decision, and returns the resolved
// (approved, action) WITHOUT applying scope or writing any task — the
// MetaagentApply stage owns those effects. action is the cold-start 5-way
// action (coldstart.Action*) for cold_start; "" for metaagent_scope (a pure
// two-way approve/deny).
//
// An await error maps to a sticky no (approved=false, action=coldstart.ActionDeny
// for cold_start), returning err=nil so Decide treats it as a deny rather than a
// Halt — preserving the timeout→deny semantics.
func (h *Host) ResolveApproval(ctx context.Context, ask pipeline.ApprovalAsk, timeout time.Duration) (approved bool, action string, err error) {
	reqID, perr := h.PublishApproval(ctx, ask)
	if perr != nil {
		return false, "", fmt.Errorf("authzd host: publish approval: %w", perr)
	}

	h.mu.Lock()
	pa, ok := h.pending[reqID]
	if ok {
		delete(h.pending, reqID)
	}
	h.mu.Unlock()
	if !ok {
		return false, "", fmt.Errorf("authzd host: no pending approval for request %q", reqID)
	}
	if h.d.Orchestrator == nil {
		return false, "", fmt.Errorf("authzd host: orchestrator not wired; cannot resolve decision %q", reqID)
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	publishFn := h.d.Publish
	dec, awaitErr := h.d.Orchestrator.Await(waitCtx, approval.Request{
		RequestID:  reqID,
		SessionRef: h.d.Session.String(),
		OnPublish: func(pubCtx context.Context) error {
			if publishFn == nil {
				return fmt.Errorf("authzd host: approval publish func not wired")
			}
			return publishFn(pubCtx, pa.payload)
		},
	})

	if pa.kind == kindColdStart {
		if awaitErr != nil {
			h.d.Logger.Info("authzd host: cold-start approval await error; treating as deny (effect-free resolve)",
				"session", h.d.Session.String(), "reqID", reqID, "err", awaitErr.Error())
			return false, coldstart.ActionDeny, nil
		}
		act := coldstart.ActionForDecision(dec)
		approvedOut := act == coldstart.ActionApproveCleaned || act == coldstart.ActionApproveOriginal
		return approvedOut, act, nil
	}
	// metaagent_scope: pure two-way bool.
	if awaitErr != nil {
		h.d.Logger.Info("authzd host: metaagent-scope approval await error; treating as deny (effect-free resolve)",
			"session", h.d.Session.String(), "reqID", reqID, "err", awaitErr.Error())
		return false, "", nil
	}
	return dec.Approved, "", nil
}

// resolveColdStart owns the five-way action mapping + the apply + cold_start_task
// write effect (mirroring runnerHost.toolCallPostApprove). Returns approved=true
// on approve_*, false on run_without_scope / deny / await-error (sticky deny). A
// nil error is returned even on await failure: an await error maps to deny, NOT a
// host-primitive failure (which the executor would turn into Halt).
func (h *Host) resolveColdStart(ctx context.Context, reqID string, pc *pendingApproval, dec approval.Decision, awaitErr error) (bool, string, error) {
	var action string
	if awaitErr != nil {
		// An await error (ctx timeout / orchestrator failure) is a sticky deny,
		// NOT a host-primitive failure: return err=nil so the executor Denies
		// rather than Halts.
		h.d.Logger.Info("authzd host: cold-start approval await error; treating as deny",
			"session", h.d.Session.String(), "reqID", reqID, "err", awaitErr.Error())
		action = coldstart.ActionDeny
	} else {
		action = coldstart.ActionForDecision(dec)
	}

	approverID := dec.ApproverID
	approved, effectErr := h.runColdStartEffect(ctx, pc, action)
	if effectErr != nil {
		// Apply / write failed: log + treat as a deny (NOT a Halt — the runner
		// failing closed on a missing task is preferable to crashing the daemon).
		h.d.Logger.Info("authzd host: cold-start post-decision effect failed; treating as deny",
			"session", h.d.Session.String(), "reqID", reqID, "action", action, "err", effectErr.Error())
		return false, approverID, nil
	}
	return approved, approverID, nil
}

// resolveMetaagentScope is the mid-session @metaagent resolver: a two-way
// approve/deny, NOT coldstart's five-way mapping, and effect-free — the apply on
// approve and the deny→sticky-HardDeny conversion belong to MetaagentApply. An
// await error is a sticky no returned with err=nil, so the caller does not Halt.
func (h *Host) resolveMetaagentScope(reqID string, dec approval.Decision, awaitErr error) (bool, string, error) {
	if awaitErr != nil {
		h.d.Logger.Info("authzd host: metaagent-scope approval await error; treating as deny (sticky no)",
			"session", h.d.Session.String(), "reqID", reqID, "err", awaitErr.Error())
		return false, "", nil
	}
	return dec.Approved, dec.ApproverID, nil
}

// runColdStartEffect performs the apply + task write + notice for the chosen
// action, returning whether the executor should treat the outcome as approved.
// Byte-identical to ColdStartHandler.Handle's per-action switch.
func (h *Host) runColdStartEffect(ctx context.Context, pc *pendingApproval, action string) (bool, error) {
	switch action {
	case coldstart.ActionApproveCleaned, coldstart.ActionApproveOriginal:
		if h.d.ApplyScope != nil {
			if err := h.d.ApplyScope(ctx, pc.applied); err != nil {
				h.audit(ctx, action, nil)
				return false, fmt.Errorf("cold-start apply scope: %w", err)
			}
		}
		appliedCopy := pc.applied
		h.audit(ctx, action, &appliedCopy)
		h.notifyRequester(ctx, "Scope set. The agent is now working on your request.")
		status, text := coldstarttask.StatusApprovedCleaned, pc.cleanedTask
		if action == coldstart.ActionApproveOriginal {
			status, text = coldstarttask.StatusApprovedOriginal, ""
		}
		if err := h.writeTask(ctx, coldstarttask.Content{
			Status: status, CleanedText: text, InboxIdx: pc.inboxIdx,
		}); err != nil {
			return false, err
		}
		return true, nil

	case coldstart.ActionRunWithoutScope:
		h.audit(ctx, action, nil)
		h.notifyRequester(ctx, "Running your request without scope changes.")
		if err := h.writeTask(ctx, coldstarttask.Content{
			Status: coldstarttask.StatusRanWithoutScope, InboxIdx: pc.inboxIdx,
		}); err != nil {
			return false, err
		}
		return false, nil

	default: // coldstart.ActionDeny
		h.audit(ctx, action, nil)
		h.notifyRequester(ctx, "Your request was declined; the agent will not run it.")
		if err := h.writeTask(ctx, coldstarttask.Content{
			Status: coldstarttask.StatusDenied, InboxIdx: pc.inboxIdx,
		}); err != nil {
			return false, err
		}
		return false, nil
	}
}

func (h *Host) audit(ctx context.Context, action string, appliedDelta *scope.ScopeDelta) {
	if h.d.Audit != nil {
		h.d.Audit(ctx, action, appliedDelta)
	}
}

func (h *Host) writeTask(ctx context.Context, c coldstarttask.Content) error {
	if h.d.WriteTask == nil {
		return fmt.Errorf("authzd host: WriteTask not wired")
	}
	c.DecidedAt = h.d.Now()
	return h.d.WriteTask(ctx, c)
}

func (h *Host) notifyRequester(ctx context.Context, body string) {
	if h.d.NotifyRequester != nil {
		h.d.NotifyRequester(ctx, body)
	}
}

// Notify delivers a Decision.Notice via the metaagent_notice publish. Best-effort.
func (h *Host) Notify(ctx context.Context, n pipeline.Notice) error {
	if n.Text() == "" {
		return nil
	}
	if h.d.NoticePublish == nil {
		h.d.Logger.Info("authzd host: Notify (no publish wired; dropped)",
			"session", h.d.Session.String(), "text", n.Text())
		return nil
	}
	if err := h.d.NoticePublish(ctx, "", n.Text()); err != nil {
		h.d.Logger.Info("authzd host: Notify publish failed",
			"session", h.d.Session.String(), "err", err.Error())
	}
	return nil
}

// SetStatus is a thin log; authzd has no transient session-status surface.
func (h *Host) SetStatus(_ context.Context, s pipeline.StatusUpdate) error {
	if s.Text != "" {
		h.d.Logger.Info("authzd host: SetStatus (best-effort)",
			"session", h.d.Session.String(), "text", s.Text)
	}
	return nil
}

// Halt is a thin log. The cold-start hook's fail-closed path is a Halt; the
// hook itself writes StatusScopeReviewFailed (the runner reads it and halts).
func (h *Host) Halt(_ context.Context, reason string) error {
	h.d.Logger.Info("authzd host: Halt", "session", h.d.Session.String(), "reason", reason)
	return nil
}

// Audit is a thin log; the cold-start audit (metaagent_audit) is written by the
// hook's deps, not here.
func (h *Host) Audit(_ context.Context, recs []pipeline.AuditRecord) error {
	for _, r := range recs {
		h.d.Logger.Info("authzd host: Audit (best-effort)",
			"session", h.d.Session.String(), "kind", r.Kind)
	}
	return nil
}

var _ pipeline.Host = (*Host)(nil)
