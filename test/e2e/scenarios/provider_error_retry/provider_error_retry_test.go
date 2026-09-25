//go:build e2e

// Package provider_error_retry_test is the end-to-end scenario for the
// provider-error → AwaitingRetry → retry-click → recovery flow, on the generic
// Interaction model (Slice B migrated provider_error_retry off its bespoke
// KindProviderErrorRetry envelopes onto interaction_request/decision/applied).
//
// What this proves end-to-end:
//
//  1. The first LLM Send returns a provider error → runner writes
//     AwaitingRetry status + condition on the AgentSession CR.
//
//  2. The in-process session watcher (startSessionWatcher) observes the
//     AwaitingRetry phase and publishes interaction_request(provider_error_retry,
//     AudienceParticipants) on the OUT NATS subject; the outbound relay routes it
//     to the fake channel's generic "interaction" sub-channel sender.
//
//  3. The fake channel driver records the interaction_request; the test asserts
//     Category=provider_error_retry, Audience=Participants, and the Lead/Body
//     carry the failure reason + the fenced error message (per Task 4's
//     publishRetryPrompt).
//
//  4. SimulateRetryClick publishes a generic interaction_decision
//     (provider_error_retry). The pipeline's HandleInteractionDecision re-checks
//     the clicker's interact standing (DecideParticipant → CheckSessionInteract),
//     invokes the bound decideProviderRetry handler, and publishes
//     interaction_applied(resolved) on OUT — the test asserts the fake driver
//     recorded it.
//
//  5. decideProviderRetry stamps the wake annotation; the AgentSession controller
//     sees it and transitions AwaitingRetry → Pending → Running, respawning a
//     new runner.
//
//  6. The second LLM Send succeeds; the runner calls respond_to_user,
//     the session reaches Idle, and the user message is delivered.
//
// In-process session watcher:
//
// The production session_watcher lives in internal/cmd/channelsd and cannot be
// imported. The inline startSessionWatcher function in this file mirrors
// the relevant subset of session_watcher.reconcile — it polls
// AgentSessions for AwaitingRetry, builds an interaction_request(provider_error_retry)
// envelope, and publishes it on the OUT NATS subject (identical to what
// publishRetryPrompt does in the production code). The harness's outbound
// relay picks it up and routes it to the fake sender, completing the loop.
//
// No real names: user@example.com is fictional per AGENTS.md.
package provider_error_retry_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestProviderErrorRetry_HappyPath: first Send errors, user clicks
// Retry, second Send succeeds, session reaches Idle with the
// assistant's reply delivered. Proves the runner → controller →
// channelsd → fake-channel → NATS → channelsd → controller → runner
// loop closes end-to-end over the generic Interaction model.
func TestProviderErrorRetry_HappyPath(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir: "../../testdata/agent-centerdot-companies",
	})

	// Seed the MCPStub so the AgentClass reaches Valid=True (without
	// this the MCPServer controller stamps AllowlistDrift and the
	// AgentSession never spawns). Handlers are placeholders — the
	// provider-error script never dispatches these tools.
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})

	// Block until the AgentClass converges; otherwise the first
	// SendUserMessage races the controller chain and the synthetic
	// inbound can land before the AgentSession controller is ready.
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	// LLM script: first call errors (simulates a provider quota error);
	// second call succeeds and delivers the reply. The respond_to_user
	// tool_result triggers a third LLM call; EndTurn closes the loop
	// so the runner doesn't re-enter an endless "what next" cycle.
	//
	// The respond_to_user and EndTurn rules are Repeating because a race in
	// envtest's informer cache can cause the AgentSession controller to see
	// a stale LastWakeAt and spawn a third runner after the second one
	// completes. The third runner reads the full conversation history and
	// makes additional LLM calls that are otherwise identical to the second
	// runner's calls. Making these rules Repeating absorbs any extra calls
	// without failing the test or leaving the runner stuck (each extra
	// EndTurn response triggers WriteIdle, which is idempotent when the
	// session is already Idle).
	const userMsg = "ship the report"
	h.LLM.OnUserMessage(userMsg).ReplyErr(
		providerErr(`anthropic: 400 {"error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Claude API. Please go to Plans & Billing to upgrade or purchase credits."}}`))
	h.LLM.OnUserMessage(userMsg).
		Reply(e2e.RespondToUser("Done — report is shipped."))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.EndTurn()).Repeating()

	// Start the in-process session watcher: mirrors internal/cmd/channelsd's
	// session_watcher.publishRetryPrompt so the harness closes the
	// AwaitingRetry → interaction_request(provider_error_retry) → fake-channel
	// leg. The production watcher is in a cmd package (not importable); we
	// inline the relevant subset here. The goroutine terminates via
	// t.Cleanup (context cancel).
	watcherCtx, watcherCancel := context.WithCancel(context.Background())
	t.Cleanup(watcherCancel)
	go startSessionWatcher(watcherCtx, h.K8s, h.NATSURL)

	// === Phase 1: inject the inbound, wait for AwaitingRetry ===
	h.SendUserMessage(userMsg)

	sess := waitForSessionPhase(t, h, spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry, 15*time.Second)
	require.NotNil(t, sess, "session must reach AwaitingRetry phase within 15s")

	// Verify the AwaitingRetry condition is stamped with the right reason.
	cond := findCondition(sess, spiceboxv1alpha1.AgentSessionConditionAwaitingRetry)
	require.NotNil(t, cond, "AwaitingRetry condition must be set")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "AwaitingRetry condition must be True")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionProviderErr, cond.Reason,
		"AwaitingRetry reason must be ProviderError")

	// === Phase 2: assert the fake channel recorded the retry interaction ===
	ch := singleChannel(t, h)
	drv := fakekind.DriverFor(ch.Namespace, ch.Name)
	require.NotNil(t, drv, "fake driver must be registered for channel %s/%s", ch.Namespace, ch.Name)

	// The session watcher polls every 250ms and publishes the
	// interaction_request; the relay routes it to the fake "interaction"
	// sender which records the prompt. Give 10s for the watcher to fire at
	// least once after AwaitingRetry.
	prompts := waitForRetryPrompts(t, drv, 1, 10*time.Second)
	require.Len(t, prompts, 1, "exactly one provider_error_retry interaction_request must be recorded")
	req := prompts[0].Payload
	assert.Equal(t, channelevents.AudienceParticipants, req.Audience.Scope,
		"the retry broadcast must be an AudienceParticipants prompt")
	assert.Contains(t, req.Lead, spiceboxv1alpha1.ReasonAgentSessionProviderErr,
		"Lead must name the failure reason")
	assert.Contains(t, req.Body, "credit balance is too low",
		"Body must carry the fenced provider error message")

	// === Phase 3: simulate the user clicking Retry (generic interaction_decision) ===
	h.SimulateRetryClick(t, sess, req.RequestRef)

	// The interaction_applied(resolved) edit must land — HandleInteractionDecision
	// publishes it on OUT after invoking decideProviderRetry (which also stamps the
	// wake annotation). Asserting it here proves the decision round-trip resolved
	// before the runner's second LLM call completes.
	applied := waitForRetryApplied(t, drv, req.RequestRef, 5*time.Second)
	assert.Equal(t, channelevents.OutcomeResolved, applied.Outcome, "retry decision must resolve")

	// === Phase 4: runner respawns, second LLM call succeeds → Idle ===
	// ExpectAgentReply polls until the outbound user_message arrives or
	// DefaultTimeout elapses.
	got := h.ExpectAgentReply(e2e.Contains("report is shipped"))
	t.Logf("agent replied: %q", got.Text)

	// Confirm the session reached Idle after the retry succeeded.
	idle := waitForSessionPhase(t, h, spiceboxv1alpha1.AgentSessionPhaseIdle, 15*time.Second)
	require.NotNil(t, idle, "session must reach Idle after retry succeeds")

	// LLM must have been called at least twice: first errored, second
	// succeeded. AssertAllRulesConsumed ensures neither the error rule
	// nor the success rule was left unmatched.
	reqs := h.LLM.Requests()
	require.GreaterOrEqual(t, len(reqs), 2,
		"LLM must have been called at least twice: first errored, second succeeded")
	h.AssertAllRulesConsumed()
}

// ----- in-process session watcher -------------------------------------------

// startSessionWatcher is a minimal analog to internal/cmd/channelsd's session_watcher.
// It polls AgentSessions in the "default" namespace every 250ms and, when a
// channel-attached session is in phase=AwaitingRetry, publishes an
// interaction_request(provider_error_retry) envelope on the OUT NATS subject.
// The outbound relay routes it to the channel kind's "interaction" sub-channel
// sender.
//
// Production code: internal/cmd/channelsd/session_watcher.go:publishRetryPrompt.
// This function mirrors that logic exactly; keep in sync.
func startSessionWatcher(ctx context.Context, k8s client.Client, natsURL string) {
	nc, err := nats.Connect(natsURL, nats.Name("e2e-session-watcher"))
	if err != nil {
		fmt.Printf("startSessionWatcher: nats.Connect: %v\n", err)
		return
	}
	defer nc.Close()

	// retryReported tracks (uid → RetryAttempts) so we don't re-post
	// the same attempt. Mirrors session_watcher.retryReported.
	retryReported := map[string]int32{}

	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		var sessions spiceboxv1alpha1.AgentSessionList
		if err := k8s.List(ctx, &sessions, client.InNamespace("default")); err != nil {
			if ctx.Err() != nil {
				return
			}
			fmt.Printf("startSessionWatcher: list: %v\n", err)
			continue
		}

		for i := range sessions.Items {
			sess := &sessions.Items[i]
			if sess.Spec.InputChannel == nil {
				continue
			}
			if sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry {
				continue
			}

			uid := string(sess.UID)
			prev, seen := retryReported[uid]
			if seen && prev == sess.Status.RetryAttempts {
				continue // already published for this attempt
			}

			if err := publishRetryPrompt(nc, sess); err != nil {
				fmt.Printf("startSessionWatcher: publishRetryPrompt %s/%s: %v\n",
					sess.Namespace, sess.Name, err)
				continue
			}
			retryReported[uid] = sess.Status.RetryAttempts
		}
	}
}

// publishRetryPrompt builds and publishes the interaction_request(provider_error_retry)
// envelope on the OUT NATS subject for sess. Mirrors session_watcher.publishRetryPrompt:
// an AudienceParticipants broadcast whose Lead names the failure reason and whose
// Body fences the (≤500-char-truncated) error message, with a single Retry
// decision action.
func publishRetryPrompt(nc *nats.Conn, sess *spiceboxv1alpha1.AgentSession) error {
	cond := meta.FindStatusCondition(sess.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionAwaitingRetry)
	message := ""
	if cond != nil {
		message = cond.Message
	}
	const maxMsg = 500 // = internal/cmd/channelsd session_watcher.sessionFailureMessageMax
	body := ""
	if trimmed := strings.TrimSpace(message); trimmed != "" {
		if len(trimmed) > maxMsg {
			trimmed = trimmed[:maxMsg] + "…"
		}
		body = "```\n" + trimmed + "\n```\n"
	}
	body += "Click Retry to try again."

	req := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		Category:        categories.ProviderErrorRetry,
		RequestRef:      fmt.Sprintf("provider-error-retry-%s-%s-%d", sess.Namespace, sess.Name, sess.Status.RetryAttempts),
		Lead:            "⚠️ Agent failed: " + sess.Status.FailureReason,
		Body:            body,
		Actions: []channelevents.InteractionAction{{
			ID:    "retry",
			Label: "Retry",
			Style: channelevents.ActionStylePrimary,
			Kind:  channelevents.ActionKindDecision,
		}},
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	}
	if err := req.Validate(); err != nil {
		return fmt.Errorf("build interaction_request: %w", err)
	}
	return channelevents.PublishOut(nc.Publish, sess.Namespace, sess.Name,
		channelevents.KindInteractionRequest, req)
}

// ----- helpers ---------------------------------------------------------------

// providerErr wraps an error string as an error value.
func providerErr(msg string) error {
	return &simpleError{msg}
}

type simpleError struct{ msg string }

func (e *simpleError) Error() string { return e.msg }

// findCondition returns the named condition from sess.Status.Conditions,
// or nil if absent.
func findCondition(sess *spiceboxv1alpha1.AgentSession, condType string) *metav1.Condition {
	return meta.FindStatusCondition(sess.Status.Conditions, condType)
}

// singleChannel returns the one Channel CR in the default namespace.
// Fatals if none or many exist.
func singleChannel(t *testing.T, h *e2e.Harness) *spiceboxv1alpha1.Channel {
	t.Helper()
	var channels spiceboxv1alpha1.ChannelList
	require.NoError(t, h.K8s.List(context.Background(), &channels))
	require.Len(t, channels.Items, 1, "scenario assumes exactly one Channel CR")
	return &channels.Items[0]
}

// waitForSessionPhase polls until an AgentSession in the default namespace
// reaches the given phase or the deadline elapses. Returns the freshest
// session on success, nil on timeout (caller requires via require.NotNil).
func waitForSessionPhase(t *testing.T, h *e2e.Harness, phase string, deadline time.Duration) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	cutoff := time.Now().Add(deadline)
	for time.Now().Before(cutoff) {
		var list spiceboxv1alpha1.AgentSessionList
		if err := h.K8s.List(context.Background(), &list, client.InNamespace("default")); err == nil {
			for i := range list.Items {
				if list.Items[i].Status.Phase == phase {
					// Re-fetch with a direct Get so the caller sees the
					// freshest status including conditions written concurrently
					// with the phase.
					var fresh spiceboxv1alpha1.AgentSession
					if err := h.K8s.Get(context.Background(),
						client.ObjectKeyFromObject(&list.Items[i]), &fresh); err == nil {
						return &fresh
					}
				}
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	// Log last-seen phases for diagnostics.
	var list spiceboxv1alpha1.AgentSessionList
	if err := h.K8s.List(context.Background(), &list, client.InNamespace("default")); err == nil {
		for i := range list.Items {
			t.Logf("waitForSessionPhase(%s): last-seen session %s/%s phase=%q",
				phase, list.Items[i].Namespace, list.Items[i].Name, list.Items[i].Status.Phase)
		}
	}
	return nil
}

// waitForRetryPrompts polls until the driver has at least minCount recorded
// interaction_request envelopes for the provider_error_retry category, or the
// deadline elapses.
func waitForRetryPrompts(t *testing.T, drv *fakekind.Driver, minCount int, deadline time.Duration) []fakekind.InteractionPrompt {
	t.Helper()
	cutoff := time.Now().Add(deadline)
	for time.Now().Before(cutoff) {
		if got := filterRetryPrompts(drv.InteractionPrompts()); len(got) >= minCount {
			return got
		}
		time.Sleep(100 * time.Millisecond)
	}
	got := filterRetryPrompts(drv.InteractionPrompts())
	t.Logf("waitForRetryPrompts: timed out; got %d provider_error_retry prompt(s), want ≥%d",
		len(got), minCount)
	return got
}

// waitForRetryApplied polls until the driver records an interaction_applied
// envelope for the provider_error_retry category matching requestRef, or the
// deadline elapses. Fatals on timeout so the caller can focus on the next
// assertion.
func waitForRetryApplied(t *testing.T, drv *fakekind.Driver, requestRef string, deadline time.Duration) channelevents.InteractionAppliedPayload {
	t.Helper()
	cutoff := time.Now().Add(deadline)
	for time.Now().Before(cutoff) {
		for _, a := range drv.InteractionApplieds() {
			if a.Payload.Category == categories.ProviderErrorRetry && a.Payload.RequestRef == requestRef {
				return a.Payload
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("waitForRetryApplied: timed out after %s waiting for provider_error_retry applied (requestRef=%s)",
		deadline, requestRef)
	return channelevents.InteractionAppliedPayload{}
}

// filterRetryPrompts keeps only the provider_error_retry category prompts.
func filterRetryPrompts(all []fakekind.InteractionPrompt) []fakekind.InteractionPrompt {
	var out []fakekind.InteractionPrompt
	for _, p := range all {
		if p.Payload.Category == categories.ProviderErrorRetry {
			out = append(out, p)
		}
	}
	return out
}
