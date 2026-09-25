package runner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// hostSession is the minimal session identity the host needs for envelopes
// and audit scope.
type hostSession struct {
	Namespace string
	Name      string
	Class     string
}

// runnerHost adapts pipeline.Host onto the runner's existing primitives:
// approval orchestrator + NATS publish (Task 9), channel Notify, l.fail, and
// memory audit kinds.
type runnerHost struct {
	l    *Loop
	sess hostSession

	// pendingMu guards pendingApprovals. Initialized lazily.
	pendingMu        sync.Mutex
	pendingApprovals *pendingApprovals

	// approvalDenyReason carries the rich, anti-confabulation deny message
	// (SYSTEM_TIMEOUT / SYSTEM_DECISION_DENIED / SYSTEM_APPROVAL_ERROR; see
	// approvalFailureContent) that AwaitDecision computes on a tool_call denial.
	// The generic executor only knows "approval denied or timed out"; dispatch
	// reads this field after a PreToolCall Deny so the LLM still sees the
	// timeout-vs-deny framing. Guarded by approvalDenyMu: the executor may run
	// on a different goroutine than the reader.
	approvalDenyMu     sync.Mutex
	approvalDenyReason string

	// coldStart* capture the SessionStart ColdStartScope hook's turn-0 placement
	// (place cleaned / place raw / place nothing) so it reaches Run() after the
	// SessionStart executor returns Allow. The generic Decision deliberately
	// carries no authz-specific placement payload, so the hook hands it over via
	// the SetPlacement host callback (setColdStartPlacement).
	// coldStartPlacementSet distinguishes "hook never ran (ineligible)" from
	// "hook ran and chose place=false".
	coldStartMu           sync.Mutex
	coldStartPlacementSet bool
	coldStartPlace        bool
	coldStartContent      []memory.ContentBlock

	// identityHandoff captures the IdentityChoiceGate's userPassthrough outcome
	// so loop.Run can distinguish a handoff-halt (Verdict==Halt, Reason
	// "identity_handoff_passthrough") from a genuine fail-closed halt. On handoff
	// the runner exits NON-terminally, re-parking for the passthrough
	// credential-link flow instead of writing Failed.
	identityHandoffMu sync.Mutex
	identityHandoff   bool

	// suppressHaltWrite makes Halt record-only: it captures the reason but does
	// NOT perform the destructive l.fail terminal write. Set ONLY for the
	// per-call SessionStart executor, where Run() must be the sole terminal
	// writer so the fail-closed ReasonAgentSessionScopeReviewFailed survives
	// (instead of the generic ReasonAgentSessionRunnerCrash) and no double
	// terminal status write happens.
	suppressHaltWrite bool

	// proxyExec marks a detached MCP-UI app-tool call; its approval waits skip
	// the shared enterApprovalPause so the live turn's clock/progress/plan-card
	// are untouched (D-D3).
	proxyExec bool

	// approvalEvent, when non-nil, receives every observable point of this call's
	// approval (see approvalObservation). Set once in executeToolContained from
	// containParams.approvalEvent and never mutated after — fixed for the host's
	// lifetime, like proxyExec above.
	approvalEvent func(approvalObservation)
}

// setColdStartPlacement records the SessionStart hook's turn-0 placement
// decision. Called by ColdStartScope.Eval via the SetPlacement dep.
func (h *runnerHost) setColdStartPlacement(place bool, content []memory.ContentBlock) {
	h.coldStartMu.Lock()
	defer h.coldStartMu.Unlock()
	h.coldStartPlacementSet = true
	h.coldStartPlace = place
	h.coldStartContent = content
}

// takeColdStartPlacement returns and clears the captured placement. found is
// false when no placement was set (the hook never ran — ineligible session), in
// which case Run() places the prompt verbatim (today's behavior).
func (h *runnerHost) takeColdStartPlacement() (found, place bool, content []memory.ContentBlock) {
	h.coldStartMu.Lock()
	defer h.coldStartMu.Unlock()
	found, place, content = h.coldStartPlacementSet, h.coldStartPlace, h.coldStartContent
	h.coldStartPlacementSet = false
	h.coldStartPlace = false
	h.coldStartContent = nil
	return found, place, content
}

// setIdentityHandoff records that the IdentityChoiceGate resolved to
// userPassthrough. Called by IdentityChoiceGate.Eval before it returns the
// handoff Halt.
func (h *runnerHost) setIdentityHandoff(v bool) {
	h.identityHandoffMu.Lock()
	defer h.identityHandoffMu.Unlock()
	h.identityHandoff = v
}

// takeIdentityHandoff returns and clears the handoff flag. loop.Run reads it
// after the SessionStart executor returns Halt to decide between a
// non-terminal passthrough handoff and a fail-closed terminal write.
func (h *runnerHost) takeIdentityHandoff() bool {
	h.identityHandoffMu.Lock()
	defer h.identityHandoffMu.Unlock()
	v := h.identityHandoff
	h.identityHandoff = false
	return v
}

// takeApprovalDenyReason returns and clears the rich tool_call deny message
// captured during AwaitDecision, or "" if none was set.
func (h *runnerHost) takeApprovalDenyReason() string {
	h.approvalDenyMu.Lock()
	defer h.approvalDenyMu.Unlock()
	r := h.approvalDenyReason
	h.approvalDenyReason = ""
	return r
}

func (h *runnerHost) setApprovalDenyReason(reason string) {
	h.approvalDenyMu.Lock()
	defer h.approvalDenyMu.Unlock()
	h.approvalDenyReason = reason
}

// newRunnerHost constructs a runnerHost for the given Loop and session.
func newRunnerHost(l *Loop, sess hostSession) *runnerHost {
	return &runnerHost{l: l, sess: sess}
}

// Notify delivers a notice to the channel via l.Notify (best-effort).
func (h *runnerHost) Notify(ctx context.Context, n pipeline.Notice) error {
	if h.l.Notify == nil {
		return nil // best-effort: kubectl-driven sessions have no channel surface
	}
	h.l.Notify(ctx, n.Text())
	return nil
}

// SetStatus delivers a status update as a Notify. The runner does NOT own the
// phase=AwaitingApproval transition — channelsd's pipeline writes it on the
// inbound approval leg. SetStatus is therefore best-effort Notify.
func (h *runnerHost) SetStatus(ctx context.Context, s pipeline.StatusUpdate) error {
	if h.l.Notify == nil {
		return nil
	}
	h.l.Notify(ctx, s.Text)
	return nil
}

// Halt writes the terminal Failed status via l.fail. Reserved for hook panic /
// host-primitive failure — every authz deny is a Deny, not Halt.
//
// With suppressHaltWrite (the per-call SessionStart executor) it records only
// and does NOT write: Run's SessionStart site reads Outcome.Verdict==Halt and
// performs the single terminal write with ReasonAgentSessionScopeReviewFailed.
func (h *runnerHost) Halt(ctx context.Context, reason string) error {
	if h.suppressHaltWrite {
		return nil
	}
	// Toolguard halts carry their own failure reason so operators can
	// distinguish a policy-driven stop from a crashed hook.
	if strings.HasPrefix(reason, "tool_guard:") {
		return h.l.fail(ctx, spiceboxv1alpha1.ReasonAgentSessionToolGuardHalt, reason)
	}
	return h.l.fail(ctx, spiceboxv1alpha1.ReasonAgentSessionRunnerCrash,
		fmt.Sprintf("authz hook halt: %s", reason))
}

// Audit fans each AuditRecord to the matching memory kind by Kind string.
// Best-effort with logging; does not abort the gate decision.
func (h *runnerHost) Audit(ctx context.Context, recs []pipeline.AuditRecord) error {
	for _, r := range recs {
		if err := h.writeAudit(ctx, r); err != nil {
			slog.Default().Info("runnerHost.Audit: write failed",
				"kind", r.Kind,
				"session", h.sess.Namespace+"/"+h.sess.Name,
				"err", err.Error())
		}
	}
	return nil
}

// writeAudit dispatches a single AuditRecord to the appropriate memory kind:
// the info-leakage family, plus approval_resolved, which captures the approver
// identity after every approval (tool_call, leakage_share, cold_start) with its
// approvalKind/approved/approver fields in string-keyed Details. Both route to
// infoleakageaudit for durable storage. Other Kinds (authzdecision, approval)
// are written by their own hook deps (RecordDecision, etc.), not here.
func (h *runnerHost) writeAudit(ctx context.Context, r pipeline.AuditRecord) error {
	if h.l.AuditMemoryAppend == nil {
		return nil
	}
	switch r.Kind {
	case "approval_resolved":
		rec := infoleakageaudit.AuditRecord{
			At:      hostNow(),
			Kind:    r.Kind,
			Session: h.sess.Namespace + "/" + h.sess.Name,
			Details: map[string]string{},
		}
		if v, ok := r.Fields["approvalKind"].(string); ok {
			rec.Details["approvalKind"] = v
		}
		if v, ok := r.Fields["approver"].(string); ok {
			rec.Details["approver"] = v
		}
		if v, ok := r.Fields["approved"].(bool); ok {
			if v {
				rec.Details["approved"] = "true"
			} else {
				rec.Details["approved"] = "false"
			}
		}
		return h.l.AuditMemoryAppend(ctx, rec)

	case "read_denied", "read_permitted", "read_unchecked",
		"unmapped_tool", "unmapped_arg", "unmapped_tool_floored",
		"leakage_detected", "leakage_approved", "leakage_denied",
		"leakage_approval_timeout", "respond_no_leak",
		"respond_capability_bypass", "would_block_leakage",
		"unsupported_channel_logOnly", "audience_resolution_failed",
		"scope_disallow_blocked", "mcp_trust_denied",
		// The trifecta's three kinds. Absent from this list, every one of them
		// hit the unrecognised-kind branch below and was logged-and-dropped —
		// so the gate's whole audit trail was written nowhere.
		//
		// The near-misses are the loss that matters. The hook records a
		// verdict even when it ALLOWS, because "two-leg near-misses are the
		// dataset logging mode exists to collect" — and they were invisible
		// anyway, which makes logging mode a control that observes nothing.
		"trifecta", "trifecta_closure_denied", "trifecta_unresolvable",
		// The per-datum fine-grained egress kinds. Like the trifecta kinds
		// above, they were absent here and so logged-and-dropped — which made
		// the tag-coverage telemetry (respond_no_leak_per_datum marks a refined
		// disclosure; its absence with taint present marks a coarse fallback)
		// invisible, and the feature's whole "measure the precision we get"
		// premise unobservable.
		"tool_call_no_leak_per_datum", "tool_call_leak_per_datum",
		"tool_call_destination_unresolved", "respond_no_leak_per_datum",
		"respond_leak_per_datum",
		"tool_call_no_leak_coarse", "tool_call_leak_coarse",
		// The attribution-failure kinds: a result whose provenance the gate
		// could not establish. Third instance of the same bug as the two
		// blocks above, which is why it is called out rather than appended
		// quietly — a gate's LOGGING-mode record is the one thing that mode
		// exists to produce, and a kind missing here is logged-and-dropped.
		// unattributable_result comes from info_leak_read's plural path,
		// audience_taint_skipped_unparseable from info_leak_audience's id
		// parse; both mean "this read reached the model and nothing recorded
		// what it read".
		"unattributable_result", "audience_taint_skipped_unparseable",
		"session_end":
		rec := infoleakageaudit.AuditRecord{
			At:      hostNow(),
			Kind:    r.Kind,
			Session: h.sess.Namespace + "/" + h.sess.Name,
		}
		if v, ok := r.Fields["tool"].(string); ok {
			rec.Tool = v
		}
		if v, ok := r.Fields["requester"].(string); ok {
			rec.Requester = v
		}
		if v, ok := r.Fields["leakedTo"].([]string); ok {
			rec.LeakedTo = v
		}
		if v, ok := r.Fields["audience"].([]string); ok {
			rec.Audience = v
		}
		// details
		details := map[string]string{}
		for k, val := range r.Fields {
			if s, ok := val.(string); ok {
				details[k] = s
			}
		}
		if len(details) > 0 {
			rec.Details = details
		}
		return h.l.AuditMemoryAppend(ctx, rec)
	}
	// Unknown kinds: log and ignore (don't fail).
	slog.Default().Info("runnerHost.writeAudit: unrecognised audit kind; skipping",
		"kind", r.Kind, "session", h.sess.Namespace+"/"+h.sess.Name)
	return nil
}

// hostNow returns the current UTC time. Defined as a var for test overrides.
var hostNow = func() time.Time { return time.Now().UTC() }

// PublishApproval and AwaitDecision are implemented in host_approval.go.

// Compile-time assertion: runnerHost must satisfy pipeline.Host.
var _ pipeline.Host = (*runnerHost)(nil)
