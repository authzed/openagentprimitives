package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/authz"
	authzdhost "github.com/authzed/openagentprimitives/pkg/authz/authzd/pipelinehost"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/metaagentaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// runMetaagentLifecycle drives ONE metaagent request (cold-start OR mid-session)
// through the four staged control-plane hooks in sequence:
//
//	MetaagentReceived → MetaagentExtract → MetaagentDecide → MetaagentApply
//
// Each stage runs its own one-hook registry via Executor.Run against the SAME
// per-request authzd Host (which owns the scratch threading the stages). A
// non-Allow Outcome from any stage short-circuits the rest:
//   - Received Deny: requester not authorized (mid_session) — abort.
//   - Extract Halt: extractor failed (fail-closed) — abort (task already written
//     for cold_start; runner halts on StatusScopeReviewFailed).
//   - Extract Deny: nothing applicable — handled in Extract (no Decide/Apply).
//   - Decide always returns Allow (it records the resolved bool into scratch);
//     Apply runs on both approve and deny.
//
// Kind/Trigger are derived from req.coldStart and stamped onto Input.Metaagent
// here (the single source of truth). The extractor LLM runs inside Extract's
// Eval — i.e. inside authzd's process — preserving the prompt-injection
// quarantine.
func (w *MetaagentWorker) runMetaagentLifecycle(ctx context.Context, req metaagentRequest) error {
	scopeRef := req.scopeRef
	sessRef := sessRefString(scopeRef)
	mg := w.mg

	// Three triggers, and the kind is knowable WITHOUT the LLM — it is the
	// trigger source, not a classification. mid_session is the default because
	// it is the narrowest: an explicit mention.
	kind, trigger := "mid_session", "mention"
	switch {
	case req.coldStart:
		kind, trigger = "cold_start", "session_start"
	case req.ambient:
		// The turn was not addressed to the metaagent. It reached here because
		// the session runs with metaagent.trigger shadow or inline AND the
		// prefilter judged it to look like intent.
		kind, trigger = "ambient", "inbound_turn"
	}

	// autoApply is resolved from the session's authz_session_config snapshot,
	// NEVER from the request payload — see cold_start_policy.go for why the wire
	// cannot be trusted with a gate-disabling flag. mid_session never
	// auto-applies (no class mode grants it), so the resolve runs only for
	// cold_start.
	var autoApply bool
	if req.coldStart {
		// Cold start is once per session. Refuse a replay BEFORE any effect:
		// no scope apply, no task rewrite, and no second signed audit record.
		decided, derr := coldStartAlreadyDecided(ctx, mg.Memory, scopeRef)
		if derr != nil {
			return derr
		}
		if decided {
			slog.Info("authzd: cold-start request for a session that already has a decision; refusing (cold start is once per session)",
				"session", sessRef, "requester", req.requester)
			return nil
		}
		pol, perr := resolveColdStartPolicy(ctx, mg.Memory, scopeRef)
		if perr != nil {
			// Fail CLOSED through the existing scope-review-failed channel: the
			// runner halts rather than running unscoped, the requester is told,
			// and the refusal is on the record.
			slog.Error("authzd: cold-start policy unresolvable; failing the session closed",
				"session", sessRef, "requester", req.requester, "err", perr.Error())
			mg.notify(ctx, scopeRef, req.requester,
				"I couldn't confirm this session's permission policy, so I stopped it for safety. Please try again.")
			writeColdStartAudit(ctx, mg.Memory, scopeRef, req, "", "scope_review_failed(policy_unresolvable)", nil)
			if werr := coldstarttask.Put(ctx, mg.Memory, scopeRef, coldstarttask.Content{
				Status:    coldstarttask.StatusScopeReviewFailed,
				DecidedAt: time.Now().UTC(),
			}); werr != nil {
				return fmt.Errorf("cold-start policy unresolvable (%w); writing scope-review-failed task also failed: %w", perr, werr)
			}
			return perr
		}
		autoApply = pol.autoApply
	}

	// Input.Requester feeds the mid_session manage_scope gate (Received, below)
	// directly as a bare identity.CanonicalUserID. mid_session's req.requester
	// arrives "user:"-prefixed at the wire (the Slack @metaagent listener
	// canonicalizes to the Subject form before publishing —
	// pkg/channels/channelkinds/slack/metaagent_listener.go's resolveCanonicalForSlackUser),
	// so strip it here via the fail-closed accessor. cold_start's req.requester
	// is ALREADY bare (the runner's Loop.StartedByCanonical, never
	// "user:"-prefixed — pkg/authz/hooks/coldstartscope.go publishes it
	// verbatim) and is never consulted by the gate (cold_start skips it
	// outright, in MetaagentReceived.Eval below); casting it directly preserves
	// cold_start's existing bytes. Running a bare canonical through the
	// accessor would spuriously fail closed — a bare canonical has no ':' and
	// is not a "user:"-prefixed subject.
	reqCanon := identity.CanonicalFromTrusted(req.requester,
		"requester claimed on the metaagent envelope by its publisher")
	if kind == "mid_session" || kind == "ambient" {
		var cerr error
		reqCanon, cerr = identity.Subject(req.requester).CanonicalUserID()
		if cerr != nil {
			// Fail closed: a malformed or non-user requester cannot manage scope.
			// runMetaagentLifecycle's caller (handleOne) logs the returned error.
			return fmt.Errorf("metaagent: requester %q is not a user subject: %w", req.requester, cerr)
		}
	}

	host := w.newAuthzdHost(req)
	in := pipeline.Input{
		Session:   pipeline.SessionRef{Namespace: nsOf(scopeRef), Name: nameOf(scopeRef)},
		Requester: reqCanon,
		Metaagent: &pipeline.MetaagentInfo{
			Kind:      kind,
			Trigger:   trigger,
			Requester: req.requester,
			Text:      req.text,
			AutoApply: autoApply,
			InboxIdx:  0,
		},
	}

	// extract wraps the kind-appropriate extractor into the shared
	// func(ctx, text)(ColdStartExtraction, error) shape. cold_start carries a
	// cleaned task; mid_session uses Metaagent.Extractor (no cleaned task).
	extract := func(ctx context.Context, text string) (scope.ColdStartExtraction, error) {
		if req.coldStart {
			if w.coldStart == nil || w.coldStart.Extractor == nil {
				return scope.ColdStartExtraction{}, fmt.Errorf("cold-start extractor not wired")
			}
			return w.coldStart.Extractor.ExtractColdStart(ctx, ExtractorInput{
				UserRequest:        text,
				Requester:          req.requester,
				AgentClassEnvelope: req.envelope,
			})
		}
		if mg.Extractor == nil {
			return scope.ColdStartExtraction{}, fmt.Errorf("metaagent extractor not wired")
		}
		d, err := mg.Extractor.Extract(ctx, ExtractorInput{
			UserRequest:        text,
			Requester:          req.requester,
			AgentClassEnvelope: req.envelope,
		})
		if err != nil {
			return scope.ColdStartExtraction{}, err
		}
		return scope.ColdStartExtraction{ScopeDelta: d}, nil
	}

	compose := func(ctx context.Context, applied scope.ScopeDelta, skipped []scope.SkippedItem, caveats []scope.CaveatItem) (string, string, string) {
		if mg.Composer == nil {
			return "", "", ""
		}
		comp, cerr := mg.Composer.Compose(ctx, ComposerInput{Applied: applied, Skipped: skipped, Caveats: caveats})
		if cerr != nil {
			return "", "", ""
		}
		return comp.ApproverSummary, joinLines(comp.SkippedExplanations), joinLines(comp.CaveatExplanations)
	}

	writeTask := func(ctx context.Context, c coldstarttask.Content) error {
		c.DecidedAt = time.Now().UTC()
		return coldstarttask.Put(ctx, mg.Memory, scopeRef, c)
	}
	notify := func(ctx context.Context, body string) { mg.notify(ctx, scopeRef, req.requester, body) }
	audit := func(ctx context.Context, action string, applied *scope.ScopeDelta) {
		writeColdStartAudit(ctx, mg.Memory, scopeRef, req, host.RequestID(), action, applied)
	}

	// Received: gate (mid_session manage_scope) + stamp kind. The checker takes
	// a bare canonical (identity.CanonicalUserID); in.Requester is already bare
	// by construction above (matching CheckInteract's contract).
	checker := w.manageScopeChecker
	received := pipeline.NewRegistry()
	received.Register(hooks.NewMetaagentReceived(hooks.MetaagentReceivedDeps{
		SetKind: host.SetKind,
		CheckManageScope: func() func(ctx context.Context, ns, name string, subject identity.CanonicalUserID) (bool, error) {
			if checker == nil {
				return nil
			}
			return func(ctx context.Context, ns, name string, subject identity.CanonicalUserID) (bool, error) {
				return authz.CheckSessionManageScope(ctx, checker,
					authz.SessionRef{Namespace: ns, Name: name},
					subject, true /*fullyConsistent*/)
			}
		}(),
	}), hooks.OrderMetaagentReceived)
	if out, _ := pipeline.NewExecutor(received).Run(ctx, pipeline.MetaagentReceived, in, host); out.Verdict != pipeline.Allow {
		return nil
	}

	// Extract: LLM → classified delta + shape (fail-closed).
	extract2 := pipeline.NewRegistry()
	extract2.Register(hooks.NewMetaagentExtract(hooks.MetaagentExtractDeps{
		Extract:         extract,
		Envelope:        req.envelope,
		RequesterPerms:  scope.RequesterPerms{},
		SetExtract:      host.SetExtract,
		WriteTask:       writeTask,
		NotifyRequester: notify,
		Audit:           audit,
	}), hooks.OrderMetaagentExtract)
	if out, _ := pipeline.NewExecutor(extract2).Run(ctx, pipeline.MetaagentExtract, in, host); out.Verdict != pipeline.Allow {
		return nil
	}

	// Decide: compose + approval round-trip (effect-free), record into scratch.
	decideReg := pipeline.NewRegistry()
	decideReg.Register(hooks.NewMetaagentDecide(hooks.MetaagentDecideDeps{
		Envelope:    req.envelope,
		PeekExtract: host.PeekExtract,
		Compose:     compose,
		RequestApproval: func(ctx context.Context, ask pipeline.ApprovalAsk) (bool, string, error) {
			return host.ResolveApproval(ctx, ask, approvalTimeoutFor(req, ask.Kind))
		},
		SetDecide:        host.SetDecide,
		SetApproved:      host.SetApproved,
		ColdStartTimeout: req.approvalTimeout,
	}), hooks.OrderMetaagentDecide)
	if out, _ := pipeline.NewExecutor(decideReg).Run(ctx, pipeline.MetaagentDecide, in, host); out.Verdict != pipeline.Allow {
		return nil
	}

	// Apply: mutate scope / write task + record + notify. For mid-session
	// requests, also emit a ScopeMutated lifecycle event so the operator's signed
	// timeline records live scope changes (in-flight, no phase change).
	applyReg := pipeline.NewRegistry()
	applyReg.Register(hooks.NewMetaagentApply(
		w.metaagentApplyDeps(req, host, host.RequestID)), hooks.OrderMetaagentApply)
	// SHADOW STOPS HERE, and this is the only place it stops.
	//
	// Everything above ran for real: the gate, the extractor LLM, the
	// classification, the approval decision. That is the point — shadow exists
	// to measure what WOULD have happened, and a mode that short-circuited
	// earlier would measure only how often the prefilter fires, which is the
	// cheap half of the question.
	//
	// Apply is the single stage that changes anything, so cutting the path here
	// is what makes "classify fully, apply nothing" true rather than
	// approximately true. It logs at INFO with the resolved decision so the
	// shadow run is readable in exactly the place an operator goes looking.
	if req.shadow {
		delta, _, _ := host.PeekExtract()
		slog.Info("authzd: metaagent SHADOW — classified, not applied",
			"session", sessRef,
			"kind", kind,
			"trigger", trigger,
			"addResources", len(delta.Add.Resources),
			"removeResources", len(delta.Remove.Resources))
		return nil
	}

	// Apply is the terminal stage (no short-circuit), but its verdict still
	// carries the apply outcome. The hook owns its own user-facing notify/audit
	// logging; we surface a non-Allow verdict here so an apply-stage failure
	// isn't lost — mirroring the `out, _ :=` pattern of the earlier stages.
	if out, _ := pipeline.NewExecutor(applyReg).Run(ctx, pipeline.MetaagentApply, in, host); out.Verdict != pipeline.Allow {
		slog.Info("authzd: metaagent apply stage non-Allow",
			"session", sessRef, "verdict", out.Verdict, "reason", out.Reason, "hook", out.FiredHook)
	}
	return nil
}

// approvalTimeoutFor returns the per-await cap for a metaagent approval. The
// ApprovalAsk.Timeout already carries the right value (the Decide hook sets
// cold_start = req.approvalTimeout, mid_session = MidSessionApprovalTimeout), but
// ResolveApproval needs it as an explicit arg. We mirror the hook's choice; a
// zero cold_start timeout falls back to the Decide hook's default behaviour
// via the executor elsewhere — here we substitute a sane non-zero cap.
func approvalTimeoutFor(req metaagentRequest, askKind string) time.Duration {
	if askKind == "metaagent_scope" {
		return hooks.MidSessionApprovalTimeout
	}
	if req.approvalTimeout > 0 {
		return req.approvalTimeout
	}
	return 10 * time.Minute
}

// metaagentApplyDeps builds the MetaagentApply stage's dependency set for one
// request. The live lifecycle and the post-restart re-drive (approval_applied.go)
// run the SAME hook through this ONE wiring, so the apply / deny-conversion /
// task-write semantics exist in exactly one place — a re-drive can never diverge
// from what the live path would have done.
//
// requestID is a function rather than a value because the live path only learns
// the id partway through the run (the Host mints it at Decide), while a re-drive
// already knows the id it is resuming.
func (w *MetaagentWorker) metaagentApplyDeps(req metaagentRequest, host *authzdhost.Host, requestID func() string) hooks.MetaagentApplyDeps {
	scopeRef := req.scopeRef
	sessRef := sessRefString(scopeRef)
	mg := w.mg
	return hooks.MetaagentApplyDeps{
		PeekExtract:  host.PeekExtract,
		PeekApproved: host.PeekApproved,
		ApplyScope: func(ctx context.Context, d scope.ScopeDelta) error {
			return mg.ApplyScopeChange(ctx, scopeRef, sessRef, d)
		},
		WriteTask: func(ctx context.Context, c coldstarttask.Content) error {
			c.DecidedAt = time.Now().UTC()
			return coldstarttask.Put(ctx, mg.Memory, scopeRef, c)
		},
		NotifyRequester: func(ctx context.Context, body string) {
			mg.notify(ctx, scopeRef, req.requester, body)
		},
		Audit: func(ctx context.Context, action string, applied *scope.ScopeDelta) {
			writeColdStartAudit(ctx, mg.Memory, scopeRef, req, requestID(), action, applied)
		},
		EmitScopeMutated: func(ctx context.Context) {
			// Only mid-session scope changes emit ScopeMutated; cold-start scope
			// changes are recorded by the runner's DecisionAsked/DecisionResolved pair.
			if req.coldStart {
				return
			}
			// ScopeMutated is an audit-only, no-phase-change event; its fold
			// position never affects the derived phase, so it is written with the
			// unstamped ordering key (folds by createdAt).
			if err := lifecycle.Append(ctx, mg.Memory, scopeRef, lifecyclecore.ScopeMutated{}, time.Now().UTC(), lifecycle.OrderKey{}); err != nil {
				slog.Info("authzd: lifecycle ScopeMutated emit failed (best-effort)",
					"session", sessRef, "err", err.Error())
			}
		},
	}
}

// newAuthzdHost builds the authzd pipeline Host wired with the shared approval
// closures (Orchestrator / Publish / ApplyScope / WriteTask / NotifyRequester /
// Audit). Both cold-start and the mid-session @metaagent path run the same
// staged lifecycle (runMetaagentLifecycle, dispatched from handleOne) and
// construct the Host through this one helper so the wiring stays single-sourced.
//
// The metaagent-scope path never invokes ApplyScope/WriteTask on the Host — the
// apply and the deny→sticky conversion belong to the MetaagentApply stage of
// runMetaagentLifecycle — so wiring them here is harmless: that AwaitDecision
// branch owns no effect.
func (w *MetaagentWorker) newAuthzdHost(req metaagentRequest) *authzdhost.Host {
	scopeRef := req.scopeRef
	sessRef := sessRefString(scopeRef)
	mg := w.mg
	// host is captured by the Audit closure below so the outcome record can be
	// stamped with the request id the Host mints later; the closure only runs
	// well after New returns.
	var host *authzdhost.Host
	host = authzdhost.New(authzdhost.Deps{
		Session:      authzdhost.SessionRef{Namespace: nsOf(scopeRef), Name: nameOf(scopeRef)},
		Orchestrator: w.approvalOrch,
		Publish: func(ctx context.Context, payload []byte) error {
			ns, name, serr := metaagentScopeSession(scopeRef.ID)
			if serr != nil {
				return fmt.Errorf("metaagent scope approval: %w", serr)
			}
			if w.natsConn == nil {
				return fmt.Errorf("metaagent: nats not wired; cannot publish scope approval")
			}
			recordScopeApprovalRequested(ctx, mg.Memory, scopeRef, payload)
			// The Host builds the payload bytes; json.RawMessage passes them
			// through the helper's marshal unchanged, so the wire shape is
			// byte-identical while the SUBJECT comes from the one grammar.
			return channelevents.PublishMetaagentOut(w.natsConn.Publish, ns, name,
				channelevents.KindMetaagentScopeApproval, json.RawMessage(payload))
		},
		ApplyScope: func(ctx context.Context, d scope.ScopeDelta) error {
			return mg.ApplyScopeChange(ctx, scopeRef, sessRef, d)
		},
		WriteTask: func(ctx context.Context, c coldstarttask.Content) error {
			return coldstarttask.Put(ctx, mg.Memory, scopeRef, c)
		},
		NotifyRequester: func(ctx context.Context, body string) {
			mg.notify(ctx, scopeRef, req.requester, body)
		},
		Audit: func(ctx context.Context, action string, applied *scope.ScopeDelta) {
			writeColdStartAudit(ctx, mg.Memory, scopeRef, req, host.RequestID(), action, applied)
		},
	})
	return host
}

// writeColdStartAudit writes one metaagent_audit record for a metaagent
// outcome, mirroring ColdStartHandler.auditColdStart. Best-effort: a write
// failure must not block the metaagent lifecycle, but it IS logged (no
// silent errors).
//
// requestID links the outcome to its request-time record. It is "" only when no
// approval was ever published (auto-apply, or a failure before the human gate).
// A stamped outcome is how the post-restart re-drive recognizes a request that
// has already been decided and must not be applied twice.
func writeColdStartAudit(
	ctx context.Context, mem memory.Memory, scopeRef memory.Scope,
	req metaagentRequest, requestID, action string, appliedDelta *scope.ScopeDelta,
) {
	if err := metaagentaudit.Record(ctx, mem, scopeRef, metaagentaudit.Content{
		Ts:               time.Now().UTC(),
		RequestID:        requestID,
		Requester:        req.requester,
		RequestText:      req.text,
		ApproverDecision: action,
		AppliedDelta:     appliedDelta,
	}); err != nil {
		slog.Info("authzd: metaagentaudit.Record failed",
			"session", scopeRef.ID, "requestId", requestID, "action", action, "err", err.Error())
	}
}

// recordScopeApprovalRequested persists the request-time metaagent_audit
// record from the published metaagent_scope_approval payload, so channel
// Show Details can render details after channelsd's cache is gone.
// Best-effort: failure is logged (never silently dropped) and does not
// block the approval publish.
//
// The record also carries the CLASSIFICATION (the delta the human is being asked
// to approve, plus what was skipped and the caveats). That is what makes the
// record re-drivable: an approval whose await died with a previous authzd
// process is resolved from here, and without the delta the record could prove a
// request happened but not what approving it should do.
func recordScopeApprovalRequested(ctx context.Context, mem memory.Memory, scopeRef memory.Scope, payload []byte) {
	var pl scope.MetaagentApprovalPayload
	if err := json.Unmarshal(payload, &pl); err != nil {
		slog.Info("authzd: decode scope-approval payload for audit record failed",
			"session", scopeRef.ID, "err", err.Error())
		return
	}
	if err := metaagentaudit.RecordRequested(ctx, mem, scopeRef, metaagentaudit.Content{
		Ts:          time.Now().UTC(),
		RequestID:   pl.RequestID,
		Requester:   pl.Requester,
		RequestText: pl.Verbatim,
		ColdStart:   pl.ColdStart,
		CleanedTask: pl.CleanedTask,
		Classification: metaagentaudit.Classification{
			Applied: pl.Applied,
			Skipped: pl.Skipped,
			Caveats: pl.Caveats,
		},
		ComposerOutput: metaagentaudit.ComposerOutput{
			ApproverSummary: pl.ApproverSummary,
			SkippedExplain:  pl.SkippedExplain,
			CaveatExplain:   pl.CaveatExplain,
		},
	}); err != nil {
		slog.Info("authzd: metaagentaudit.RecordRequested failed",
			"session", scopeRef.ID, "requestId", pl.RequestID, "err", err.Error())
	}
}

func nsOf(s memory.Scope) string   { return cutBefore(s.ID) }
func nameOf(s memory.Scope) string { return cutAfter(s.ID) }

func cutBefore(id string) string {
	for i := 0; i < len(id); i++ {
		if id[i] == '/' {
			return id[:i]
		}
	}
	return id
}

func cutAfter(id string) string {
	for i := 0; i < len(id); i++ {
		if id[i] == '/' {
			return id[i+1:]
		}
	}
	return ""
}
