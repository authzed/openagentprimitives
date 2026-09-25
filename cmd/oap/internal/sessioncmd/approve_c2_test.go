package sessioncmd

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

// TestCollectPendings_C2_ToolAndLeakageFromGenericList pins the Slice C2
// single-read cutover: a tool_approval and an info_leakage entry on the generic
// PendingInteractions list resolve to their CLI approvalKinds (no typed
// pendingToolGrants/pendingLeakageApprovals lists remain), and each dispatches a
// generic KindInteractionDecision envelope tagged with the family's Category.
func TestCollectPendings_C2_ToolAndLeakageFromGenericList(t *testing.T) {
	sess := sessionWith(
		spiceboxv1alpha1.PendingInteraction{RequestID: "t1", Category: categories.ToolApproval, ApproverSubject: "crm_company:1#owner"},
		spiceboxv1alpha1.PendingInteraction{RequestID: "l1", Category: categories.InfoLeakage, ApproverSubject: "crm_company:2#owner"},
	)

	got := collectPendings(sess)
	require.Len(t, got, 2, "both approval families come from the single generic list")
	assert.Equal(t, approvalKindTool, got[0].Kind, "tool_approval category → tool_call kind")
	assert.Equal(t, approvalKindLeakage, got[1].Kind, "info_leakage category → leakage_share kind")

	cases := []struct {
		name     string
		kind     approvalKind
		reqID    string
		category string
	}{
		{"tool_call → interaction_decision(tool_approval)", approvalKindTool, "t1", string(categories.ToolApproval)},
		{"leakage → interaction_decision(info_leakage)", approvalKindLeakage, "l1", string(categories.InfoLeakage)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := capturePublish(t, tc.kind, tc.reqID, "owner@example.com", "approve")
			assert.Equal(t, channelevents.KindInteractionDecision, env.Kind)
			var pl channelevents.InteractionDecisionPayload
			require.NoError(t, json.Unmarshal(env.Payload, &pl))
			assert.Equal(t, tc.category, pl.Category)
			assert.Equal(t, tc.reqID, pl.RequestRef)
			assert.Equal(t, "approve", pl.ActionID)
			assert.NoError(t, pl.Validate(), "decider must satisfy InteractionDecisionPayload.Validate")
		})
	}
}
