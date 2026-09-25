//go:build e2e

package channel_test

import (
	"context"
	"encoding/json"
	"github.com/authzed/openagentprimitives/test/e2e"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
)

// injectToolApprovalInteraction posts a synthetic tool_approval
// interaction_request through the fake "interaction" sub-channel sender —
// mirroring what the outbound relay does given a runner-published
// interaction_request(tool_approval), but bypassing the runner so these smokes
// stay focused on the harness's approval API contract on the generic
// Interaction model. Returns nothing; ExpectApprovalPrompt drains the recorded
// prompt from Driver.InteractionPrompts() filtered by category.
func injectToolApprovalInteraction(t *testing.T, ch *spiceboxv1alpha1.Channel, requestID, sessionName, toolName, resourceType, resourceID, permission string) {
	t.Helper()
	fakeKind := fakekind.Kind{}
	sender := fakeKind.SubChannelSender("interaction", channelkinds.Deps{Channel: ch})
	require.NotNil(t, sender, "fake interaction sender should exist")

	details, err := json.Marshal(channelevents.ToolApprovalDetails{
		Permission:   permission,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		ArgsJSON:     `{"company_id":"` + resourceID + `"}`,
	})
	require.NoError(t, err, "marshal tool approval details")

	prompt := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: ch.Namespace, Name: sessionName},
		Category:        categories.ToolApproval,
		RequestRef:      requestID,
		Lead:            "Approval needed for `" + toolName + "`",
		Fields: []channelevents.InteractionField{
			{Label: "Tool", Value: "`" + toolName + "`"},
		},
		Details: details,
		Audience: channelevents.InteractionAudience{
			Scope: channelevents.AudienceApprovers,
			Approvers: []channelevents.ExternalIdentity{
				{Kind: "fake", ExternalID: "owner-1@example.com", Email: "owner-1@example.com"},
			},
		},
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision, Style: channelevents.ActionStylePrimary},
			{ID: "deny", Label: "Deny", Kind: channelevents.ActionKindDecision, Style: channelevents.ActionStyleDanger},
		},
	}
	env, err := channelevents.BuildEnvelope(ch.Namespace, sessionName,
		channelevents.KindInteractionRequest, prompt)
	require.NoError(t, err, "build interaction_request envelope")
	_, err = sender.Send(context.Background(), channelkinds.SessionInfo{
		Namespace: ch.Namespace, Name: sessionName,
	}, env)
	require.NoError(t, err, "fake interaction sender.Send")
}

// TestApproval_PromptCapturedAndApproveDecisionPublishes exercises the
// envelope-level approval round-trip for the harness's approval API on the
// generic Interaction model:
//
//  1. Boot the harness with the centerdot AgentDir so we get a real
//     fake-kind Channel CR (and the AgentClass converges to Valid=True
//     before we look up the Driver).
//  2. Inject a synthetic tool_approval interaction_request through the fake
//     "interaction" sub-channel sender — bypasses the runner so this smoke
//     stays focused on the API contract.
//  3. ExpectApprovalPrompt (draining Driver.InteractionPrompts() filtered by
//     category tool_approval) with ForTool + ForResource must match.
//  4. Subscribe to the interaction_decision subject BEFORE calling Approve so
//     the publish is guaranteed observable.
//  5. After Approve, decode the published KindInteractionDecision envelope and
//     assert category + actionId + decider email round-trip correctly.
//
// We deliberately do NOT assert the pipeline's HandleInteractionDecision
// succeeded — there is no parked PendingInteraction for this synthetic prompt,
// so the pipeline subscription logs a benign "no pending interaction" and drops
// it; we only care that the harness's publish landed on the right subject with
// the right payload shape. Real-runner end-to-end tool_approval coverage lives
// in the centerdot contacts_owner_approval / contacts_owner_deny scenarios.
func TestApproval_PromptCapturedAndApproveDecisionPublishes(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-centerdot-companies"),
		DefaultTimeout: 5 * time.Second,
	})
	// Pre-seed the MCPStub so the AgentClass converges before we
	// look up the Channel CR via the fake driver registry. The
	// names MUST match centerdot/02-mcpserver.yaml.
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)

	ch := h.SingleChannel("TestApproval_PromptCapturedAndApproveDecisionPublishes")

	const (
		requestID    = "req-smoke-1"
		sessionName  = "smoke-session"
		toolName     = "list_contacts_for_company"
		resourceType = "crm_company"
		resourceID   = "acme-id"
		permission   = "list_contacts"
	)
	injectToolApprovalInteraction(t, ch, requestID, sessionName, toolName, resourceType, resourceID, permission)

	// Subscribe to the decision subject BEFORE Approve so the publish is
	// guaranteed observable. The pipeline's own subscription (wired by
	// startChannelsdPlumbing) also fires on the same NATS message.
	decisionSubject := channelevents.SubjectIn(
		channelevents.SubjectPrefix(ch.Namespace, sessionName), channelevents.KindInteractionDecision)
	sub, err := h.NATS().SubscribeSync(decisionSubject)
	require.NoError(t, err, "subscribe decision subject")
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	approval := h.ExpectApprovalPrompt(
		e2e.ForTool(toolName),
		e2e.ForResource(resourceType+":"+resourceID),
	)
	require.NotNil(t, approval, "approval handle")
	assert.Equal(t, requestID, approval.Prompt().RequestID, "RequestID round-trips")
	assert.Equal(t, permission, approval.Prompt().Permission, "Permission round-trips")
	assert.Equal(t, ch.Namespace+"/"+sessionName, approval.Prompt().SessionRef,
		"SessionRef encodes ns/name")

	approval.Approve(e2e.AsUser("owner-1@example.com"))

	// Read the published decision envelope.
	msg, err := sub.NextMsg(2 * time.Second)
	require.NoError(t, err, "decision envelope should be published within 2s")

	var decisionEnv channelevents.Envelope
	require.NoError(t, json.Unmarshal(msg.Data, &decisionEnv), "decode envelope")
	assert.Equal(t, channelevents.KindInteractionDecision, decisionEnv.Kind, "envelope.Kind")
	assert.Equal(t, ch.Namespace, decisionEnv.Session.Namespace, "envelope.Session.Namespace")
	assert.Equal(t, sessionName, decisionEnv.Session.Name, "envelope.Session.Name")

	var dpl channelevents.InteractionDecisionPayload
	require.NoError(t, json.Unmarshal(decisionEnv.Payload, &dpl), "decode payload")
	assert.Equal(t, categories.ToolApproval, dpl.Category, "decision Category")
	assert.Equal(t, requestID, dpl.RequestRef, "decision RequestRef round-trips")
	assert.Equal(t, "approve", dpl.ActionID, "decision actionId")
	assert.Equal(t, "fake", dpl.Decider.Kind.String(), "Decider.Kind")
	assert.Equal(t, "owner-1@example.com", dpl.Decider.Email.String(), "Decider.Email reflects AsUser")
	assert.Equal(t, "owner-1@example.com", dpl.Decider.ExternalID.String(), "Decider.ExternalID reflects AsUser")
}

// TestApproval_DenyPublishesDeny mirrors the Approve smoke but for the
// Deny path. Uses distinct identifiers from the Approve test because
// the fake kind's Driver registry is process-global keyed by
// (namespace, channelName) — the centerdot fixture's single Channel
// name is reused across tests in the same package, so prompts
// accumulate. A unique RequestID + ForResource predicate makes the
// match deterministic regardless of prior tests' leftovers.
func TestApproval_DenyPublishesDeny(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-centerdot-companies"),
		DefaultTimeout: 5 * time.Second,
	})
	h.MCP.OnTool("list_companies", func(_ map[string]any) any { return map[string]any{"results": []any{}} })
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any { return map[string]any{"results": []any{}} })
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)

	ch := h.SingleChannel("TestApproval_DenyPublishesDeny")

	const (
		requestID    = "req-deny-unique"
		sessionName  = "deny-session"
		toolName     = "list_contacts_for_company"
		resourceType = "crm_company"
		resourceID   = "deny-test-target"
		permission   = "list_contacts"
	)
	injectToolApprovalInteraction(t, ch, requestID, sessionName, toolName, resourceType, resourceID, permission)

	decisionSubject := channelevents.SubjectIn(
		channelevents.SubjectPrefix(ch.Namespace, sessionName), channelevents.KindInteractionDecision)
	sub, err := h.NATS().SubscribeSync(decisionSubject)
	require.NoError(t, err, "subscribe decision subject")
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	// Match by ForResource with the deny-test-target — guarantees we
	// pick OUR prompt out of the driver's accumulated queue even if a
	// prior test left smoke-1 entries in place.
	approval := h.ExpectApprovalPrompt(
		e2e.ForTool(toolName),
		e2e.ForResource(resourceType+":"+resourceID),
	)
	require.NotNil(t, approval)
	assert.Equal(t, requestID, approval.Prompt().RequestID, "should match this test's prompt")
	approval.Deny(e2e.AsUser("owner-1@example.com"))

	msg, err := sub.NextMsg(2 * time.Second)
	require.NoError(t, err, "decision envelope should be published")
	var decisionEnv channelevents.Envelope
	require.NoError(t, json.Unmarshal(msg.Data, &decisionEnv))
	var dpl channelevents.InteractionDecisionPayload
	require.NoError(t, json.Unmarshal(decisionEnv.Payload, &dpl))
	assert.Equal(t, categories.ToolApproval, dpl.Category, "decision Category")
	assert.Equal(t, "deny", dpl.ActionID, "decision actionId is deny")
	assert.Equal(t, requestID, dpl.RequestRef, "decision RequestRef matches our prompt")
}
