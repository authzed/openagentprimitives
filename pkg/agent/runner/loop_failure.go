package runner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/authzed/openagentprimitives/pkg/x/besteffort"
)

// identityHaltReason maps a SessionStart Halt Decision.Reason to the terminal
// AgentSession Failed reason. The runner-side SessionStart executor carries two
// hooks — IdentityChoiceGate (identity_* reasons) and ColdStartScope
// (cold-start:*) — whose halts must terminalize with DIFFERENT reasons:
//
//   - identity_cancelled → IdentityChoiceCancelled, so the runner's direct
//     WriteFailed AGREES with the gate's own lifecycle fold (no two-writer
//     divergence over a sticky terminal phase).
//   - identity_choice_timeout → IdentityChoiceTimeout, matching the operator's
//     timeout backstop. The gate emits no lifecycle event on timeout, so the
//     runner owns the reason; ScopeReviewFailed here would make the operator's
//     later IdentityChoiceTimeout a sticky no-op behind an already-written
//     WRONG reason.
//   - any other identity_* halt (fail-closed: no channel, envelope build
//     failure, await transport error, unknown action) → IdentityChoiceFailed.
//   - everything else (cold-start scope halts) → ScopeReviewFailed.
//
// An identity halt is NEVER terminalized as ScopeReviewFailed.
func identityHaltReason(gateReason string) string {
	switch gateReason {
	case "identity_cancelled":
		return spiceboxv1alpha1.ReasonIdentityChoiceCancelled
	case "identity_choice_timeout":
		return spiceboxv1alpha1.ReasonIdentityChoiceTimeout
	default:
		if strings.HasPrefix(gateReason, "identity_") {
			return spiceboxv1alpha1.ReasonIdentityChoiceFailed
		}
		return spiceboxv1alpha1.ReasonAgentSessionScopeReviewFailed
	}
}

func (l *Loop) fail(ctx context.Context, reason, message string) error {
	// The runner DETECTS and RECORDS every terminal failure here, so it must
	// also LOG it: without this, a BudgetExceeded / MaxTokensExceeded session
	// writes Failed status and emits an internal lifecycle signal but leaves no
	// record in the runner's own logs of *why* it stopped — indistinguishable
	// from a hang, with channelsd's status watcher the only other witness.
	slog.Default().Info("session terminal failure",
		"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
		"reason", reason, "message", message)
	l.emitLifecycleSignal(ctx, lifecycle.SigSessionFailed, map[string]string{"reason": message})
	// Hand authority back to the operator-post region with the terminal phase,
	// so the operator's fold-based projection sees Failed (and stays sticky)
	// instead of re-deriving Running on its next reconcile. reason is the
	// failure-classification constant; message rides along so the signed log
	// says WHY on its own, without the CR's status beside it.
	l.emitLifecycleEvent(ctx, lifecyclecore.RunnerTerminal{
		Phase: lifecyclecore.PhaseFailed, Reason: reason, Message: message,
	})
	l.fireSessionEnd(ctx, "failed")
	if err := l.Status.WriteFailed(ctx, reason, message); err != nil {
		return fmt.Errorf("loop terminal write (reason=%s msg=%s): %w", reason, message, err)
	}
	return nil
}

// fireSessionEnd runs the SessionEnd pipeline point so the SessionCleanup hook
// (finalize-audit + wired-if-present external-state removal) executes on every
// terminal path: completion, idle, AND failure. It is CLEANUP, not a gate — the
// Outcome verdict is discarded (a Deny/Halt from a cleanup hook would be
// meaningless) and the executor still applies the hook's audit. Best-effort: a
// failure is logged but MUST NOT alter the terminal status the caller is about
// to write. reason is one of completed|idle|failed.
func (l *Loop) fireSessionEnd(ctx context.Context, reason string) {
	switch reason {
	case "idle":
		// The idle exit is the agent having FINISHED, not the agent waiting on an
		// answer: PauseCauseReply is OnAwaitYield's (await_user_message), and a
		// surface reading it opens a "reply to the agent" affordance. Sharing it
		// here would open one at the end of every turn, over the agent's closing
		// statement.
		l.emitActivity(ctx, true, channelevents.PauseCauseIdle)
	case "failed":
		l.emitActivity(ctx, true, channelevents.PauseCauseFailed)
	}
	host := newRunnerHost(l, hostSession{
		Namespace: l.SessionKey.Namespace,
		Name:      l.SessionKey.Name,
		Class:     l.AgentName,
	})
	snap := l.usageSnapshot()
	if _, err := l.executor().Run(ctx, pipeline.SessionEnd, pipeline.Input{
		Session: pipeline.SessionRef{
			Namespace: l.SessionKey.Namespace,
			Name:      l.SessionKey.Name,
			Class:     l.AgentName,
		},
		Requester: l.authSubject, // may be "" at SessionEnd; the contract allows it
		End: &pipeline.SessionEndInfo{
			Reason:              reason,
			Model:               l.Model,
			InputTokens:         snap.InputTokens,
			OutputTokens:        snap.OutputTokens,
			CacheCreationTokens: snap.CacheCreationTokens,
			CacheReadTokens:     snap.CacheReadTokens,
			ByModel:             l.usageByModelSnapshot(),
		},
	}, host); err != nil {
		slog.Default().Info("fireSessionEnd: SessionEnd executor errored (best-effort)",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "reason", reason, "err", err.Error())
	}
}

// failProviderError routes a Provider.Send error to either the retryable
// AwaitingRetry path (channel-attached: the user gets a Retry button) or the
// terminal Failed path (kubectl-driven: existing exit-code UX). The loop's two
// other ProviderErr sites — the stop-reason advisory and
// tool-terminal-without-submit, both in loop.go — intentionally stay on l.fail:
// they are protocol violations with no retry path.
func (l *Loop) failProviderError(ctx context.Context, message string) error {
	if l.ChannelAttached && !l.Status.IsLocal() {
		slog.Default().Info("provider error; pausing for user retry",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "message", message)
		l.emitActivity(ctx, true, channelevents.PauseCauseRetry)
		l.emitLifecycleSignal(ctx, lifecycle.SigSessionAwaitingRetry,
			map[string]string{"reason": message})
		// Retryable provider error: the core's retry budget sub-machine moves to
		// AwaitingRetry (and fails closed past the retry cap).
		l.emitLifecycleEvent(ctx, lifecyclecore.ProviderError{})
		if err := l.Status.WriteAwaitingRetry(ctx, spiceboxv1alpha1.ReasonAgentSessionProviderErr, message); err != nil {
			return fmt.Errorf("loop AwaitingRetry write (msg=%s): %w", message, err)
		}
		return nil
	}
	return l.fail(ctx, spiceboxv1alpha1.ReasonAgentSessionProviderErr, message)
}

// handleRefusal parks a session whose provider response carried
// stop_reason=refusal. The refused assistant turn was already persisted (with
// Refused=true) and its tool_use blocks are deliberately NOT dispatched — a
// refused turn is untrustworthy and its args may be truncated. Channel-attached
// sessions take the recoverable AwaitingRetry path (Retry button + rephrase);
// local sessions fail terminally, having no retry surface. This is the ONLY
// refusal path.
func (l *Loop) handleRefusal(ctx context.Context) error {
	const userMsg = "The model declined to complete that response. This can happen with certain content or very large outputs. You can retry, or rephrase your request."
	const advisory = "terminal stop_reason refusal: content-policy block; not dispatched, parked for retry"
	slog.Default().Info("provider refusal; parking for retry",
		"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name)
	besteffort.Log(slog.Default().Info, "AppendRunnerNote (refusal)",
		l.Status.AppendRunnerNote(ctx, advisory),
		"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name)

	if l.ChannelAttached && !l.Status.IsLocal() {
		l.emitActivity(ctx, true, channelevents.PauseCauseRetry)
		l.emitLifecycleSignal(ctx, lifecycle.SigSessionAwaitingRetry,
			map[string]string{"reason": userMsg})
		l.emitLifecycleEvent(ctx, lifecyclecore.ProviderError{})
		if err := l.Status.WriteAwaitingRetry(ctx, spiceboxv1alpha1.ReasonAgentSessionRefusal, userMsg); err != nil {
			return fmt.Errorf("loop AwaitingRetry write (refusal): %w", err)
		}
		return nil
	}
	return l.fail(ctx, spiceboxv1alpha1.ReasonAgentSessionRefusal, userMsg)
}

// stopReasonAdvisory returns a per-stop_reason advisory the loop injects into
// the next user turn (or uses as the failure reason) when the LLM halted for any
// reason other than the expected `tool_use` / `end_turn`; "" for those two,
// which the normal loop path handles.
//
// Of Anthropic's documented stop_reasons, `stop_sequence` and `pause_turn` are
// unexpected in this runtime — we configure no stop sequences and use no
// server-tools pause/resume — so they get an advisory saying so. Unknown values
// get a generic one, rather than silently hiding novel provider behavior.
//
// `refusal` is handled by handleRefusal BEFORE this is ever called; its case
// below is kept only so stopReasonAdvisory and userFacingStopReason stay
// exhaustive per stop_reason and unit-testable in isolation.
func stopReasonAdvisory(stopReason string, maxTokens int) string {
	switch stopReason {
	case "", "tool_use", "end_turn":
		return ""
	case "max_tokens":
		return fmt.Sprintf(
			"[runner-warning] Your previous assistant turn was TRUNCATED at max_tokens=%d. "+
				"The tool_use arguments may be incomplete or empty as a result. "+
				"Adjust by: (a) emitting LESS prose before the tool call, "+
				"(b) splitting long content across multiple respond_to_user calls, "+
				"or (c) writing a more concise summary. Then retry the tool with COMPLETE args.",
			maxTokens,
		)
	case "stop_sequence":
		return "[runner-warning] Your previous assistant turn ended on a `stop_sequence` match. " +
			"This runtime does not configure stop sequences, so encountering one is unexpected — " +
			"the response may be incomplete. Continue with whatever step you intended next; " +
			"if the same thing keeps happening, simplify your phrasing."
	case "pause_turn":
		return "[runner-warning] Your previous assistant turn ended on `pause_turn`. " +
			"This runtime does not use the server-tools pause/resume protocol, so the pause cannot " +
			"be honored — the response is effectively cut short. Re-issue the request without the " +
			"server tool, or split the work into smaller in-runtime steps."
	case "refusal":
		return "[runner-warning] Your previous assistant turn ended in a `refusal` (content-policy block). " +
			"Reframe the user's request in policy-compliant terms or, if the request truly cannot be " +
			"fulfilled, explain that politely via respond_to_user and call agent_work_complete with a " +
			"summary noting the refusal. Do NOT keep retrying the same prompt."
	default:
		return fmt.Sprintf(
			"[runner-warning] Your previous assistant turn ended with an unexpected stop_reason=%q. "+
				"This runtime expects only `tool_use` or `end_turn`. The response may be incomplete or "+
				"malformed; retry with a clearer continuation, or call agent_work_complete with what you "+
				"have so far if you cannot make progress.",
			stopReason,
		)
	}
}

// userFacingStopReason returns the human-readable text the USER sees when a turn
// ends on a terminal non-tool_use stop_reason. It deliberately carries NO
// agent-facing detail (no "[runner-warning]", no tool names): stopReasonAdvisory
// carries that, for the agent + operator logs. Kept in sync with its cases.
func userFacingStopReason(stopReason string) string {
	switch stopReason {
	case "refusal":
		return "I'm sorry, but I can't help with that request. Please try rephrasing it."
	case "max_tokens":
		return "My response was cut off because it grew too long. Please ask for a shorter answer or split the request into smaller parts."
	case "pause_turn", "stop_sequence":
		return "My response was interrupted before I could finish. Please try again."
	default:
		return "I wasn't able to complete a response to that. Please try again, or rephrase your request."
	}
}

// budgetFailReason maps a Budget.Check result to the AgentSession Failed
// condition reason: expiration is its own reason; everything else is a generic
// budget breach.
func budgetFailReason(checkReason string) string {
	if checkReason == BudgetReasonExpired {
		return spiceboxv1alpha1.ReasonAgentSessionExpired
	}
	return spiceboxv1alpha1.ReasonAgentSessionBudget
}
