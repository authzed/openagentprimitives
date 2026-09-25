package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

// mustMarshalInteractionApplied builds a raw .out.interaction_applied NATS
// message body (envelope-wrapped) for the given payload — what
// handleInteractionAppliedMessage decodes.
func mustMarshalInteractionApplied(t *testing.T, ns, name string, pl channelevents.InteractionAppliedPayload) []byte {
	t.Helper()
	env, err := channelevents.BuildEnvelope(ns, name, channelevents.KindInteractionApplied, pl)
	require.NoError(t, err, "build interaction applied envelope")
	data, err := json.Marshal(env)
	require.NoError(t, err, "marshal envelope")
	return data
}

// TestInteractionAppliedSubjectIsSessionScoped mirrors nats_test.go's sibling
// subject-shape checks (approvalAppliedSubject etc.).
func TestInteractionAppliedSubjectIsSessionScoped(t *testing.T) {
	got := interactionAppliedSubject("ns1", "sess1")
	assert.Equal(t, "ap.session.ns1.sess1.out.interaction_applied", got)
	assert.NotContains(t, got, "*", "must not be a wildcard subject")
}

// TestHandleInteractionAppliedMessage_IdentityChoice_DeliversDecision verifies
// the runner-resume coupling this task adds: an .out.interaction_applied
// message for the identity_choice category unblocks the IdentityChoiceGate's
// Await with the right action — read back out of OutcomeText, since
// InteractionAppliedPayload carries no Action field.
func TestHandleInteractionAppliedMessage_IdentityChoice_DeliversDecision(t *testing.T) {
	orch := approval.New()
	decidedBy := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE"}
	data := mustMarshalInteractionApplied(t, "default", "sess-1", channelevents.InteractionAppliedPayload{
		Category:    categories.IdentityChoice,
		RequestRef:  "req-1",
		Outcome:     channelevents.OutcomeApproved,
		OutcomeText: "userPassthrough",
		Reason:      "clicked",
		DecidedBy:   &decidedBy,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d, err := orch.Await(ctx, approval.Request{
		RequestID: "req-1",
		OnPublish: func(context.Context) error {
			// Await registers the request into orch's pending map BEFORE
			// invoking OnPublish, so delivering here — exactly like the real
			// gate's IdentityChoicePublish → subscribeInteractionApplied round
			// trip — is race-free (mirrors identitygate_test.go's fixture).
			handleInteractionAppliedMessage(data, orch)
			return nil
		},
	})
	require.NoError(t, err, "Await must not time out once delivered")
	assert.Equal(t, "userPassthrough", d.Action, "Decision.Action reads back OutcomeText")
	assert.Equal(t, "U_ALICE", d.ApproverID, "Decision.ApproverID reads back DecidedBy.ExternalID")
	assert.Equal(t, "clicked", d.Reason)
}

// contentInspectionCategory mirrors the string literal
// handleInteractionAppliedMessage matches. It duplicates
// categories.ContentInspection and should be replaced by it.
const contentInspectionCategory = "content_inspection"

// TestHandleInteractionAppliedMessage_ContentInspection_DeliversDecision
// verifies the second runner-resuming category this task adds: an
// .out.interaction_applied message for content_inspection unblocks the
// runner-host approval gate's Await with an Approved-shaped Decision (no
// Action — content_inspection is a pure allow/deny, unlike identity_choice's
// 3-way answer). Outcome maps onto Decision.Approved: approved→true,
// denied/expired→false — a timeout (Outcome=expired) resolves the pause as
// NOT approved rather than leaving the gate blocked.
func TestHandleInteractionAppliedMessage_ContentInspection_DeliversDecision(t *testing.T) {
	cases := []struct {
		name         string
		outcome      string
		wantApproved bool
	}{
		{"approved outcome → Approved=true", channelevents.OutcomeApproved, true},
		{"denied outcome → Approved=false", channelevents.OutcomeDenied, false},
		{"expired outcome (timeout) → Approved=false", channelevents.OutcomeExpired, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orch := approval.New()
			decidedBy := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE"}
			data := mustMarshalInteractionApplied(t, "default", "sess-1", channelevents.InteractionAppliedPayload{
				Category:   contentInspectionCategory,
				RequestRef: "req-1",
				Outcome:    tc.outcome,
				Reason:     "policy",
				DecidedBy:  &decidedBy,
			})

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			d, err := orch.Await(ctx, approval.Request{
				RequestID: "req-1",
				OnPublish: func(context.Context) error {
					handleInteractionAppliedMessage(data, orch)
					return nil
				},
			})
			require.NoError(t, err, "Await must not time out once delivered")
			assert.Equal(t, tc.wantApproved, d.Approved)
			assert.Equal(t, "", d.Action, "content_inspection carries no Action — pure allow/deny")
			assert.Equal(t, "U_ALICE", d.ApproverID, "Decision.ApproverID reads back DecidedBy.ExternalID")
			assert.Equal(t, "policy", d.Reason)
		})
	}
}

// TestHandleInteractionAppliedMessage_ToolApprovalAndLeakage_DeliverDecision
// verifies the Slice C2 bridge collapse: tool_approval and info_leakage now
// resume off the SAME generic interaction_applied bridge as content_inspection
// (the two legacy subscribeApprovalApplied / subscribeLeakageApprovalApplied
// subscribers were deleted). Both are Approved-shaped gates — Outcome maps onto
// Decision.Approved (approved→true, denied/expired→false), with no Action.
func TestHandleInteractionAppliedMessage_ToolApprovalAndLeakage_DeliverDecision(t *testing.T) {
	cases := []struct {
		name         string
		category     string
		outcome      string
		wantApproved bool
	}{
		{"tool_approval approved → Approved=true", categories.ToolApproval, channelevents.OutcomeApproved, true},
		{"tool_approval denied → Approved=false", categories.ToolApproval, channelevents.OutcomeDenied, false},
		{"tool_approval expired (timeout) → Approved=false", categories.ToolApproval, channelevents.OutcomeExpired, false},
		{"info_leakage approved → Approved=true", categories.InfoLeakage, channelevents.OutcomeApproved, true},
		{"info_leakage denied → Approved=false", categories.InfoLeakage, channelevents.OutcomeDenied, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orch := approval.New()
			decidedBy := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_OWNER"}
			data := mustMarshalInteractionApplied(t, "default", "sess-1", channelevents.InteractionAppliedPayload{
				Category:   tc.category,
				RequestRef: "req-1",
				Outcome:    tc.outcome,
				Reason:     "resolved",
				DecidedBy:  &decidedBy,
			})

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			d, err := orch.Await(ctx, approval.Request{
				RequestID: "req-1",
				OnPublish: func(context.Context) error {
					handleInteractionAppliedMessage(data, orch)
					return nil
				},
			})
			require.NoError(t, err, "Await must not time out once delivered")
			assert.Equal(t, tc.wantApproved, d.Approved)
			assert.Equal(t, "", d.Action, "tool_approval/info_leakage carry no Action — pure Approved-shaped gates")
			assert.Equal(t, "U_OWNER", d.ApproverID, "Decision.ApproverID reads back DecidedBy.ExternalID")
			assert.Equal(t, "resolved", d.Reason)
		})
	}
}

// TestHandleInteractionAppliedMessage_NonIdentityChoiceCategory_Ignored verifies
// the category filter: an interaction_applied for a different category (e.g.
// credential_link, which resolves out-of-band and never blocks this
// orchestrator) must NOT deliver a decision — a category collision on
// RequestRef would otherwise misroute an unrelated resolution into the gate.
func TestHandleInteractionAppliedMessage_NonIdentityChoiceCategory_Ignored(t *testing.T) {
	orch := approval.New()
	data := mustMarshalInteractionApplied(t, "default", "sess-1", channelevents.InteractionAppliedPayload{
		Category:    categories.CredentialLink,
		RequestRef:  "req-1",
		Outcome:     channelevents.OutcomeResolved,
		OutcomeText: "irrelevant",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := orch.Await(ctx, approval.Request{
		RequestID: "req-1",
		OnPublish: func(context.Context) error {
			handleInteractionAppliedMessage(data, orch)
			return nil
		},
	})
	require.Error(t, err, "a non-identity_choice category must not deliver a decision; Await times out")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestHandleInteractionAppliedMessage_MalformedPayload_DoesNotPanic verifies
// the decode failure paths (bad envelope JSON, bad payload JSON) log and
// return rather than panicking — no-silent-crash on a malformed wire message.
func TestHandleInteractionAppliedMessage_MalformedPayload_DoesNotPanic(t *testing.T) {
	orch := approval.New()
	assert.NotPanics(t, func() {
		handleInteractionAppliedMessage([]byte("not json"), orch)
	})

	badPayloadEnv, err := json.Marshal(channelevents.Envelope{
		Version: 1,
		Kind:    channelevents.KindInteractionApplied,
		Session: channelevents.SessionRef{Namespace: "default", Name: "sess-1"},
		Payload: json.RawMessage(`"not an object"`),
	})
	require.NoError(t, err)
	assert.NotPanics(t, func() {
		handleInteractionAppliedMessage(badPayloadEnv, orch)
	})
}
