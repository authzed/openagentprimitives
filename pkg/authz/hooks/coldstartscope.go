package hooks

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// User-facing notice strings, kept byte-identical to the strings the runner's
// runColdStart emitted via Notify (no-silent-errors: the user always learns the
// session was stopped). The executor delivers these via Decision.Notices.
const (
	noticeMarshalFailed = "Scope review for your request could not be started; the session was stopped."
	noticePublishFailed = "Couldn't reach the scope-review service for your request; the session was stopped."
	noticeReviewFailed  = "Scope review for your request could not be completed; the session was stopped. No action was taken."
)

// ColdStartScopeDeps is the dependency struct for the ColdStartScope hook.
//
// The runner pre-resolves everything the hook needs so the hook never imports
// pkg/agent/runner (cycle) or pkg/apis/v1alpha1: the requester, the autoApply
// flag, both wait deadlines, the envelope material (bound entities + tool
// names), and the publish/wait/get/placement closures.
type ColdStartScopeDeps struct {
	Requester identity.CanonicalUserID // canonical subject of the session initiator

	// AutoApply selects the wait deadline: false (extractAndApprove) waits the
	// human approval window (ApprovalTimeout); true (extractAndAutoApply) waits
	// the machine-speed extractor latency (LLMLatency).
	AutoApply       bool
	ApprovalTimeout time.Duration
	LLMLatency      time.Duration

	// Envelope material, pre-extracted by the runner (mirrors ToolReadsDecl: the
	// hook must NOT import the v1alpha1 AgentClass type).
	BoundEntities []scope.EnvelopeBoundEntity
	ToolNames     []string

	// Publish publishes the cold-start metaagent_request payload to authzd.
	Publish func(ctx context.Context, payload []byte) error
	// WaitForTask blocks until the cold_start_task lands or deadline elapses
	// (wraps authz.WaitForColdStartTask over the session memory scope).
	WaitForTask func(ctx context.Context, deadline time.Duration)
	// GetTask reads the cold_start_task decision (wraps coldstarttask.Get).
	GetTask func(ctx context.Context) (coldstarttask.Content, bool, error)
	// SetPlacement is the host callback the hook invokes on a usable decision to
	// communicate turn-0 placement (place cleaned / place raw / place nothing)
	// back to the runner, keeping the generic Decision free of authz payload.
	SetPlacement func(place bool, content []memory.ContentBlock)

	// OnScopeReviewAsked is called immediately after the cold-start
	// metaagent_request is successfully published to authzd. The runner wires this
	// to emit a DecisionAsked{scope_review} lifecycle event so the operator fold
	// can surface the pending review (ScopeReviewPending). No-op when nil.
	OnScopeReviewAsked func(ctx context.Context)

	// OnScopeReviewResolved is called once GetTask returns, recording the scope
	// review outcome in the signed lifecycle log.
	//   approved=true  — task found and usable (any status except ScopeReviewFailed)
	//   timedOut=true  — deadline elapsed with no task written by authzd
	// The runner wires this to emit DecisionResolved{scope_review, approved, timedOut}.
	// No-op when nil. Always called when OnScopeReviewAsked was called.
	OnScopeReviewResolved func(ctx context.Context, approved, timedOut bool)

	// RawPrompt is the initial prompt fallback when in.Turn is nil.
	RawPrompt string

	Logger *slog.Logger
}

// ColdStartScope is the SessionStart hook that drives the runner's cold-start
// scope review: it publishes the cold-start metaagent_request to authzd, waits
// for the cold_start_task decision, and maps the outcome:
//
//   - usable decision (Get found, status != ScopeReviewFailed): SetPlacement
//     (cleaned / raw / nothing) and return Allow. A denied / empty-remainder
//     decision is still usable — place=false, Allow (authzd already surfaced the
//     deny notice; denial is NOT a Halt).
//   - scope-review failure (marshal/publish error, timeout with no task, Get
//     error, or StatusScopeReviewFailed): return Halt + a user-facing Notice and
//     set NO placement. The runner maps this Halt to a fail-closed
//     ReasonAgentSessionScopeReviewFailed terminal write — the agent must NEVER
//     run unscoped.
//
// This is a security gate: when scope review cannot complete it FAILS CLOSED.
// The untrusted-text LLM extraction, approval and scope-apply live in authzd's
// ColdStartHandler, not here.
type ColdStartScope struct{ d ColdStartScopeDeps }

// NewColdStartScope creates a ColdStartScope hook.
func NewColdStartScope(d ColdStartScopeDeps) *ColdStartScope {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &ColdStartScope{d: d}
}

func (h *ColdStartScope) Name() string             { return "cold_start_scope" }
func (h *ColdStartScope) Points() []pipeline.Point { return []pipeline.Point{pipeline.SessionStart} }

func (h *ColdStartScope) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	rawPrompt := h.d.RawPrompt
	if in.Turn != nil && in.Turn.Text != "" {
		rawPrompt = in.Turn.Text
	}
	rawContent := []memory.ContentBlock{{Type: "text", Text: rawPrompt}}

	// AutoApply is NOT on the wire. Whether the human approval gate is waived is
	// authzd's call, resolved from the session's authz_session_config snapshot —
	// this subject is inside the runner's own NATS grant, so a flag sent from
	// here would be a component asking to skip the gate that constrains it.
	// AutoApply still sizes the local wait deadline below.
	payload, merr := json.Marshal(map[string]any{
		"requester":       h.d.Requester.String(),
		"text":            rawPrompt,
		"coldStart":       true,
		"envelope":        ColdStartEnvelope(h.d.BoundEntities, h.d.ToolNames),
		"approvalTimeout": h.d.ApprovalTimeout.String(),
	})
	if merr != nil {
		// Marshalling our own request can't fail in practice; if it did we have
		// no way to run scope review — fail closed.
		h.d.Logger.Info("cold-start: marshal request failed; failing session closed", "err", merr.Error())
		return haltWithNotice("cold-start: marshal scope-review request: "+merr.Error(), noticeMarshalFailed)
	}

	if h.d.Publish == nil {
		h.d.Logger.Info("cold-start: publish func not wired; failing session closed")
		return haltWithNotice("cold-start: publish func not wired", noticeReviewFailed)
	}
	if perr := h.d.Publish(ctx, payload); perr != nil {
		h.d.Logger.Info("cold-start: publish failed; failing session closed",
			"session", in.Session.String(), "err", perr.Error())
		return haltWithNotice("cold-start: publish scope-review request: "+perr.Error(), noticePublishFailed)
	}

	// Scope review is now in flight: record it in the lifecycle log so the
	// operator can surface the pending wait (ScopeReviewPending status condition).
	if h.d.OnScopeReviewAsked != nil {
		h.d.OnScopeReviewAsked(ctx)
	}

	// Size the wait by mode. extractAndApprove blocks on a HUMAN clicking the
	// approval block, so it waits the approval window (minutes); extractAndAutoApply
	// has no human, so it waits the machine-speed extractor latency.
	deadline := h.d.ApprovalTimeout
	if h.d.AutoApply {
		deadline = h.d.LLMLatency
	}
	if h.d.WaitForTask != nil {
		waitCtx, cancel := context.WithTimeout(ctx, deadline)
		h.d.WaitForTask(waitCtx, deadline)
		cancel()
	}

	if h.d.GetTask == nil {
		h.d.Logger.Info("cold-start: get-task func not wired; failing session closed",
			"session", in.Session.String())
		// No task result: treat as timeout (no decision arrived).
		if h.d.OnScopeReviewResolved != nil {
			h.d.OnScopeReviewResolved(ctx, false, true)
		}
		return haltWithNotice("cold-start: get-task func not wired", noticeReviewFailed)
	}
	cst, found, gerr := h.d.GetTask(ctx)
	if gerr != nil || !found {
		h.d.Logger.Info("cold-start: scope review did not complete; failing session closed",
			"session", in.Session.String(), "found", found, "err", errStr(gerr))
		// timedOut is true only when the deadline elapsed cleanly with no task written
		// (not-found, no error). A read error is not a timeout.
		timedOut := !found && gerr == nil
		if h.d.OnScopeReviewResolved != nil {
			h.d.OnScopeReviewResolved(ctx, false, timedOut)
		}
		return haltWithNotice(
			"cold-start: scope review did not complete (timeout or authzd error); refusing to run unscoped",
			noticeReviewFailed)
	}
	// authzd explicitly reported that scope review failed (extractor LLM error,
	// no handler). Fail closed: halt rather than run the raw prompt unscoped.
	if cst.Status == coldstarttask.StatusScopeReviewFailed {
		h.d.Logger.Info("cold-start: authzd reported scope review failed; failing session closed",
			"session", in.Session.String())
		if h.d.OnScopeReviewResolved != nil {
			h.d.OnScopeReviewResolved(ctx, false, false)
		}
		return haltWithNotice(
			"cold-start: authzd reported scope review failed; refusing to run unscoped",
			noticeReviewFailed)
	}

	// Usable decision: map status to placement and record the resolved outcome.
	// approved mirrors whether the task will actually run: approved=true for
	// approve_cleaned/approve_original/ran_without_scope, approved=false for
	// denied (human said no — the task does not run, though the session may
	// continue in the channel). Both are non-timeout, non-failure outcomes.
	place, content := ColdStartTurnContent(cst, rawContent)
	if h.d.OnScopeReviewResolved != nil {
		h.d.OnScopeReviewResolved(ctx, place, false)
	}
	if h.d.SetPlacement != nil {
		h.d.SetPlacement(place, content)
	}
	return pipeline.Decision{}
}

// haltWithNotice builds a fail-closed Halt decision carrying a user-facing
// Notice. Routing the notice through Decision.Notices lets the executor own
// delivery (no synchronous host call from the hook).
func haltWithNotice(reason, userText string) pipeline.Decision {
	return pipeline.Decision{
		Verdict: pipeline.Halt,
		Reason:  reason,
		Notices: []pipeline.Notice{{
			Notice: notice.New(categories.SessionHalted, notice.Args{
				Lead: "This session was stopped before it could run",
				Body: userText,
				// Terminal, so there is nothing to retry here — the next step
				// is a fresh attempt, not a repeat of this one.
				NextStep: "Start a new session once the cause above is resolved.",
				Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester},
			}),
			ToRequester: true,
		}},
	}
}

// ColdStartEnvelope builds the static AgentClass envelope (bound entities +
// tools) the cold-start classifier needs, from material the runner pre-extracts.
// Pure: no I/O, no AgentClass dependency.
func ColdStartEnvelope(boundEntities []scope.EnvelopeBoundEntity, toolNames []string) scope.AgentClassEnvelope {
	var env scope.AgentClassEnvelope
	for _, be := range boundEntities {
		env.BoundEntities = append(env.BoundEntities, scope.EnvelopeBoundEntity{
			ResourceType: be.ResourceType,
			Permission:   be.Permission,
		})
	}
	for _, name := range toolNames {
		env.Tools = append(env.Tools, scope.EnvelopeTool{Name: name})
	}
	return env
}

// ColdStartTurnContent maps an approver's cold_start_task decision to a
// placement decision for the first inbox turn. Pure: no I/O.
//
//   - StatusApprovedCleaned   → place the cleaned text (no turn when empty).
//   - StatusApprovedOriginal,
//     StatusRanWithoutScope    → place the raw content.
//   - StatusDenied (or unknown
//     status)                 → place nothing (turn aborted; authzd already
//     notified the requester).
func ColdStartTurnContent(cst coldstarttask.Content, raw []memory.ContentBlock) (place bool, content []memory.ContentBlock) {
	switch cst.Status {
	case coldstarttask.StatusApprovedCleaned:
		if cst.CleanedText == "" {
			return false, nil
		}
		return true, []memory.ContentBlock{{Type: "text", Text: cst.CleanedText}}
	case coldstarttask.StatusApprovedOriginal, coldstarttask.StatusRanWithoutScope:
		return true, raw
	default: // StatusDenied + any unrecognized status: run nothing.
		return false, nil
	}
}

// errStr returns err.Error() or "" for a nil error — for structured log fields
// where a nil error should not render as "<nil>".
func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

var _ pipeline.Hook = (*ColdStartScope)(nil)
