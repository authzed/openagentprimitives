package pipeline

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
)

// waiverDecision builds a precondition_waiver decision. The waiver card carries
// the SAME grant fields as a tool_approval card (ToolApprovalDetails), because
// approving it binds the same shape of slot grant — the difference is the
// category and the consent it represents, not the tuple written.
func waiverDecision(actionID, stateImpact string) channelinteractions.Decision {
	det, _ := json.Marshal(channelevents.ToolApprovalDetails{
		Permission: "write", ResourceType: "repo", ResourceID: "r1", ArgsHash: "h1", StateImpact: stateImpact,
	})
	return channelinteractions.Decision{
		Session: channelevents.SessionRef{Namespace: "default", Name: "demo-session"},
		Payload: channelevents.InteractionDecisionPayload{Category: categories.PreconditionWaiver, RequestRef: "req-w1", ActionID: actionID},
		Request: &channelevents.InteractionRequestPayload{Category: categories.PreconditionWaiver, RequestRef: "req-w1", Details: det},
	}
}

func TestPreconditionWaiverHandler(t *testing.T) {
	// Approving a waiver binds a SLOT grant — the tuple on the RESOURCE pointing
	// at the session — carrying the permission, exactly as tool_approval does.
	// Asserting the tuple SHAPE (not merely OutcomeApproved) is what makes this
	// fail if BindApproved is never called.
	t.Run("approve: binds one slot grant carrying resource, id and permission", func(t *testing.T) {
		rec := &slotGrantRecorder{}
		out, err := preconditionWaiverHandler(approvalPipeline(t, rec))(context.Background(), waiverDecision("approve", "readwrite"))
		require.NoError(t, err)
		assert.Equal(t, channelevents.OutcomeApproved, out.Result)
		require.Len(t, rec.relations, 1)
		rel := rec.relations[0]
		assert.Equal(t, "repo", rel.ResourceType)
		assert.Equal(t, "r1", rel.ResourceID)
		assert.Equal(t, authz.SlotGrantRelationName("write"), rel.Relation)
		assert.Equal(t, "agentsession", rel.SubjectType)
		assert.Equal(t, "default/demo-session", rel.SubjectID)
	})
	t.Run("approve external: keeps the short 30s leash, not the session-wide horizon", func(t *testing.T) {
		rec := &slotGrantRecorder{}
		_, err := preconditionWaiverHandler(approvalPipeline(t, rec))(context.Background(), waiverDecision("approve", "external"))
		require.NoError(t, err)
		require.Len(t, rec.relations, 1)
		assert.WithinDuration(t, time.Now().Add(30*time.Second), rec.relations[0].ExpiresAt, time.Minute,
			"an external effect is waived for the moment, not for the session")
	})
	t.Run("approve non-external: bounded by the session horizon, never unbounded", func(t *testing.T) {
		rec := &slotGrantRecorder{}
		_, err := preconditionWaiverHandler(approvalPipeline(t, rec))(context.Background(), waiverDecision("approve", "readwrite"))
		require.NoError(t, err)
		require.Len(t, rec.relations, 1)
		assert.False(t, rec.relations[0].ExpiresAt.IsZero(), "a slot grant with no expiry is the leak the expiry exists to prevent")
		assert.True(t, rec.relations[0].ExpiresAt.After(time.Now().Add(time.Hour)),
			"and it must outlast the call that prompted it")
	})
	t.Run("deny writes nothing, OutcomeDenied", func(t *testing.T) {
		rec := &slotGrantRecorder{}
		out, err := preconditionWaiverHandler(approvalPipeline(t, rec))(context.Background(), waiverDecision("deny", "external"))
		require.NoError(t, err)
		assert.Equal(t, channelevents.OutcomeDenied, out.Result)
		assert.Empty(t, rec.relations, "deny must have no side effect")
	})
	t.Run("grant-write failure returns error (pipe surfaces render-error via D4)", func(t *testing.T) {
		rec := &slotGrantRecorder{fail: true}
		_, err := preconditionWaiverHandler(approvalPipeline(t, rec))(context.Background(), waiverDecision("approve", "external"))
		require.Error(t, err)
	})
	t.Run("unknown actionID returns error", func(t *testing.T) {
		rec := &slotGrantRecorder{}
		_, err := preconditionWaiverHandler(approvalPipeline(t, rec))(context.Background(), waiverDecision("bogus", "external"))
		require.Error(t, err)
		assert.Empty(t, rec.relations)
	})
}

// The waiver is a BINDING: a human was shown one instance and consented to
// waiving its precondition. Recording that as a grant alone leaves the others
// merely un-granted rather than EXCLUDED, so the session_scope narrowing must
// name exactly the approved instance.
func TestPreconditionWaiverDecision_narrowsScopeToTheApprovedInstance(t *testing.T) {
	rec := &slotGrantRecorder{}
	p := approvalPipeline(t, rec)

	det, _ := json.Marshal(channelevents.ToolApprovalDetails{
		Permission: "view", ResourceType: "crm_company", ResourceID: "4210", ArgsHash: "h1", StateImpact: "readonly",
	})
	d := channelinteractions.Decision{
		Session: channelevents.SessionRef{Namespace: "default", Name: "demo-session"},
		Payload: channelevents.InteractionDecisionPayload{Category: categories.PreconditionWaiver, RequestRef: "req-w2", ActionID: "approve"},
		Request: &channelevents.InteractionRequestPayload{Category: categories.PreconditionWaiver, RequestRef: "req-w2", Details: det},
	}

	out, err := preconditionWaiverHandler(p)(context.Background(), d)
	require.NoError(t, err)
	assert.Equal(t, channelevents.OutcomeApproved, out.Result)

	got, _, err := sessionscope.Get(
		memory.WithSystemApproval(context.Background(), "test"),
		p.Mem, memory.Scope{Kind: "session", ID: "default/demo-session"})
	require.NoError(t, err)

	var ids []string
	for _, r := range got.Resources {
		if r.ResourceType == "crm_company" {
			ids = append(ids, r.IDs...)
		}
	}
	assert.Equal(t, []string{"4210"}, ids,
		"waiving this instance must exclude the others, not merely permit this one")
}
