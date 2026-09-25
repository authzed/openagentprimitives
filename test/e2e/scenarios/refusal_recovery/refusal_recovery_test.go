//go:build e2e

// Package refusal_recovery_test proves a provider content-policy refusal is
// recoverable end-to-end on the generic Interaction model: refusal →
// AwaitingRetry(reason=Refusal) → interaction_request(provider_error_retry) →
// Retry click (interaction_decision) → clean re-run → respond_to_user → Idle.
// No real names: user@example.com is fictional per AGENTS.md.
//
// This is a near-clone of test/e2e/scenarios/provider_error_retry: the only
// behavioral difference is the failure mode (a scripted stop_reason=refusal
// instead of a ReplyErr) and the expected AwaitingRetry reason
// (ReasonAgentSessionRefusal instead of ReasonAgentSessionProviderErr). The
// package-local session-watcher and polling helpers below are copied
// verbatim from that scenario's test file — each scenario package carries
// its own copy since the production watcher lives in internal/cmd/channelsd and
// cannot be imported.
package refusal_recovery_test

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

// TestRefusalRecovery_HappyPath: first Send returns a provider content-policy
// refusal (stop_reason=refusal, no error), user clicks Retry, second Send
// succeeds, session reaches Idle with the assistant's reply delivered.
// Proves the runner → controller → channelsd → fake-channel → NATS →
// channelsd → controller → runner loop closes end-to-end for the refusal
// recovery path over the generic Interaction model, and that the AwaitingRetry
// reason is Refusal (not ProviderError).
func TestRefusalRecovery_HappyPath(t *testing.T) {
	h := e2e.Start(t, e2e.Options{AgentDir: "../../testdata/agent-centerdot-companies"})
	h.MCP.OnTool("list_companies", func(_ map[string]any) any { return map[string]any{"results": []any{}} })
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any { return map[string]any{"results": []any{}} })
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	const userMsg = "make the one-sheeter"
	// First Send: a content-policy refusal (successful Send, stop_reason=refusal).
	h.LLM.OnUserMessage(userMsg).Reply(e2e.Refusal())
	// After retry: the same user message is replayed cleanly; second Send succeeds.
	h.LLM.OnUserMessage(userMsg).Reply(e2e.RespondToUser("Here is your one-sheeter."))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	watcherCtx, watcherCancel := context.WithCancel(context.Background())
	t.Cleanup(watcherCancel)
	go startSessionWatcher(watcherCtx, h.K8s, h.NATSURL)

	// Phase 1: inbound → AwaitingRetry with reason=Refusal.
	h.SendUserMessage(userMsg)
	sess := waitForSessionPhase(t, h, spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry, 15*time.Second)
	require.NotNil(t, sess, "session must reach AwaitingRetry after a refusal")
	cond := findCondition(sess, spiceboxv1alpha1.AgentSessionConditionAwaitingRetry)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionRefusal, cond.Reason,
		"AwaitingRetry reason must be Refusal, not ProviderError")

	// Phase 2: retry interaction recorded on the fake channel.
	ch := singleChannel(t, h)
	drv := fakekind.DriverFor(ch.Namespace, ch.Name)
	require.NotNil(t, drv)
	prompts := waitForRetryPrompts(t, drv, 1, 10*time.Second)
	require.Len(t, prompts, 1)
	req := prompts[0].Payload
	assert.Equal(t, channelevents.AudienceParticipants, req.Audience.Scope,
		"the retry broadcast must be an AudienceParticipants prompt")
	assert.Contains(t, req.Lead, spiceboxv1alpha1.ReasonAgentSessionRefusal,
		"Lead must name the Refusal reason")

	// Phase 3: click Retry (generic interaction_decision).
	h.SimulateRetryClick(t, sess, req.RequestRef)
	applied := waitForRetryApplied(t, drv, req.RequestRef, 5*time.Second)
	assert.Equal(t, channelevents.OutcomeResolved, applied.Outcome, "retry decision must resolve")

	// Phase 4: clean re-run succeeds → Idle, reply delivered.
	got := h.ExpectAgentReply(e2e.Contains("one-sheeter"))
	t.Logf("agent replied: %q", got.Text)
	idle := waitForSessionPhase(t, h, spiceboxv1alpha1.AgentSessionPhaseIdle, 15*time.Second)
	require.NotNil(t, idle, "session must reach Idle after the retry succeeds")

	// The retry must have re-run through replay() with the refused turn
	// excluded and the refusal nudge appended (pkg/agent/runner/loop.go,
	// appendRefusalNudge): scan every observed LLM request for the
	// distinctive "content policy" phrase as direct evidence of a clean
	// re-run, not just a second Send.
	reqs := h.LLM.Requests()
	require.GreaterOrEqual(t, len(reqs), 2)
	foundNudge := false
	for _, rq := range reqs {
		for _, m := range rq.Messages {
			for _, b := range m.Content {
				if strings.Contains(b.Text, "content policy") {
					foundNudge = true
				}
			}
		}
	}
	assert.True(t, foundNudge,
		"the post-refusal retry must replay through replay() with the refusal nudge applied — proves the refused turn was excluded and the retry re-ran cleanly")
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
