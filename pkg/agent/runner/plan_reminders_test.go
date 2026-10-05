package runner

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlanRemindersPrepareExactCardsWithoutAutoApproval(t *testing.T) {
	l, mem := freezeLoop(t)
	expires := time.Now().Add(time.Minute)
	owner := channelevents.ExternalIdentity{Kind: "email", ExternalID: "owner@example.com", Email: "owner@example.com"}
	card := channelevents.InteractionRequestPayload{Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester, Requester: &owner}, Actions: []channelevents.InteractionAction{{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision}}, AgentSessionRef: channelevents.SessionRef{Namespace: "ns", Name: "s"}, Category: "goal_execution_consent", RequestRef: "reviewed", Lead: "Private reminder", ExpiresAt: &expires}
	phases := []plans.Phase{{ID: "schedule", Label: "Schedule", Permissions: []plans.PhasePermission{{Handle: "perm:read:tracker_issue", Why: "read"}}, Reminders: []plans.ReminderRequest{{Resource: "agent_goal_domain:domain", ExecutionRequest: goals.ExecutionRequest{ID: "goal", Revision: 2, RequestID: "reminder"}}}}}
	calls := 0
	call := func(_ context.Context, r goals.Request) (goals.Response, error) {
		calls++
		assert.Equal(t, "plan", r.Execution.ApprovalMode)
		assert.Equal(t, "agent_goal_domain:domain", r.Resource)
		assert.Equal(t, "reminder", r.Execution.RequestID)
		return goals.Response{Approval: &card}, nil
	}
	authored, err := PreparePlanReminders(context.Background(), phases, call, true)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	_, err = l.FreezeAndRecordPhases(context.Background(), authored)
	require.NoError(t, err)
	frozen, _ := l.ActiveFrozenPlanNow()
	state, err := plangate.Fold(frozen, l.PlanGateRecords())
	require.NoError(t, err)
	assert.False(t, state.PhaseApproved(0), "a readonly phase cannot auto-approve reminder consent")
	for _, rec := range l.PlanGateRecords() {
		assert.NotEqual(t, plangateaudit.EventPhaseApproved, rec.Event)
	}

	var published channelevents.Envelope
	host := planGateHost(t, &published)
	host.l.Mem = mem
	host.l.SpiceDBLookupSubjects = func(context.Context, string, string) ([]string, error) {
		return []string{"user:b3duZXJAZXhhbXBsZS5jb20", "user:other-approver"}, nil
	}
	ask := planPhaseAsk()
	ask.Payload["covered"] = []plangateaudit.Content{plangate.PhaseAuthorityRecord(frozen, 0, nil)}
	pending, err := host.buildPlanGatePending(context.Background(), "combined", ask, "plan_phase")
	require.NoError(t, err)
	require.NoError(t, pending.onPublish(context.Background()))
	var parent channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(published.Payload, &parent))
	require.Len(t, parent.Consents, 1)
	assert.Equal(t, "Approve this plan and 1 reminder schedule?", parent.Lead)
	require.Len(t, parent.Audience.Approvers, 1, "private reminder terms must not reach other plan approvers")
	assert.Equal(t, card.RequestRef, parent.Consents[0].RequestRef)
	assert.Equal(t, channelevents.PlanConsentExcerpt(parent.Consents), parent.Excerpt)
	assert.NotEmpty(t, parent.Details, "exact child instructions remain available on every channel")
	assert.False(t, parent.ExpiresAt.After(expires))
	require.NoError(t, parent.Validate())

	_, err = PreparePlanReminders(context.Background(), phases, call, false)
	require.Error(t, err)
	assert.Equal(t, 1, calls, "bounded and delegated sessions must not even prepare a request")
}

func TestPlanReminderApprovalWithoutPermissionedCalls(t *testing.T) {
	for _, approved := range []bool{true, false} {
		t.Run(map[bool]string{true: "approve", false: "deny"}[approved], func(t *testing.T) {
			l, _ := freezeLoop(t)
			l.Status = LocalStatusPatcher()
			l.Approval = approval.New()
			l.ChannelKind = "browser"
			l.SpiceDBLookupSubjects = func(context.Context, string, string) ([]string, error) {
				return []string{"user:b3duZXJAZXhhbXBsZS5jb20"}, nil
			}
			expires := time.Now().UTC().Add(time.Minute)
			owner := channelevents.ExternalIdentity{Kind: "email", Email: "owner@example.com", ExternalID: "owner@example.com"}
			card := channelevents.InteractionRequestPayload{AgentSessionRef: channelevents.SessionRef{Namespace: "ns", Name: "s"}, Category: "goal_execution_consent", RequestRef: "one", Lead: "Private reminder", ExpiresAt: &expires, Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester, Requester: &owner}, Actions: []channelevents.InteractionAction{{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision}}}
			raw, err := json.Marshal(card)
			require.NoError(t, err)
			_, err = l.FreezeAndRecordPhases(context.Background(), []plangate.AuthoredPhase{{ID: "schedule", Label: "Schedule reminder", Consents: []json.RawMessage{raw}}})
			require.NoError(t, err)
			published := 0
			l.InteractionRequestPublish = func(_ context.Context, _, _ string, env channelevents.Envelope) error {
				var request channelevents.InteractionRequestPayload
				if err := json.Unmarshal(env.Payload, &request); err != nil {
					return err
				}
				published++
				require.Len(t, request.Consents, 1)
				l.Approval.DeliverDecision(request.RequestRef, approval.Decision{Approved: approved, ApproverID: "user:b3duZXJAZXhhbXBsZS5jb20"})
				return nil
			}
			err = l.RequestPlanReminderApproval(context.Background())
			if !approved {
				require.ErrorContains(t, err, "denied or expired")
			} else {
				require.NoError(t, err)
				require.NoError(t, l.RequestPlanReminderApproval(context.Background()))
			}
			assert.Equal(t, 1, published)
			plan, _ := l.ActiveFrozenPlanNow()
			state, err := plangate.Fold(plan, l.PlanGateRecords())
			require.NoError(t, err)
			assert.Equal(t, approved, state.PhaseApproved(0))
		})
	}
}

func TestNewReminderDoesNotReapplyApprovedExpiredPhase(t *testing.T) {
	l, mem := freezeLoop(t)
	owner := channelevents.ExternalIdentity{Kind: "email", Email: "owner@example.com", ExternalID: "owner@example.com"}
	expired, future := time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Hour)
	card := channelevents.InteractionRequestPayload{AgentSessionRef: channelevents.SessionRef{Namespace: "ns", Name: "s"}, Category: "goal_execution_consent", RequestRef: "old", Lead: "Private reminder", ExpiresAt: &expired, Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester, Requester: &owner}, Actions: []channelevents.InteractionAction{{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision}}}
	oldRaw, err := json.Marshal(card)
	require.NoError(t, err)
	old := plangate.AuthoredPhase{ID: "old", Label: "Old reminder", Consents: []json.RawMessage{oldRaw}}
	_, err = l.FreezeAndRecordPhases(context.Background(), []plangate.AuthoredPhase{old})
	require.NoError(t, err)
	plan, _ := l.ActiveFrozenPlanNow()
	rec := plangate.PhaseAuthorityRecord(plan, 0, nil)
	rec.Event = plangateaudit.EventPhaseApproved
	require.NoError(t, plangateaudit.Record(context.Background(), mem, memory.Scope{Kind: "session", ID: "ns/s"}, rec))
	card.RequestRef, card.ExpiresAt = "new", &future
	newRaw, err := json.Marshal(card)
	require.NoError(t, err)
	_, err = l.FreezeAndRecordPhases(context.Background(), []plangate.AuthoredPhase{old, {ID: "new", Label: "New reminder", Consents: []json.RawMessage{newRaw}}})
	require.NoError(t, err)
	plan, _ = l.ActiveFrozenPlanNow()
	var published channelevents.Envelope
	h := planGateHost(t, &published)
	l.InteractionRequestPublish, l.SpiceDBLookupSubjects = h.l.InteractionRequestPublish, h.l.SpiceDBLookupSubjects
	l.SpiceDBLookupSubjects = func(context.Context, string, string) ([]string, error) {
		return []string{"user:b3duZXJAZXhhbXBsZS5jb20"}, nil
	}
	h.l = l
	ask := planPhaseAsk()
	ask.Payload["covered"] = []plangateaudit.Content{plangate.PhaseAuthorityRecord(plan, 0, nil), plangate.PhaseAuthorityRecord(plan, 1, nil)}
	pending, err := h.buildPlanGatePending(context.Background(), "new-plan", ask, "plan_phase")
	require.NoError(t, err)
	require.NoError(t, pending.onPublish(context.Background()))
	var parent channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(published.Payload, &parent))
	require.Len(t, parent.Consents, 1)
	assert.Equal(t, "new", parent.Consents[0].RequestRef)
	assert.True(t, parent.ExpiresAt.After(time.Now()))
	assert.Len(t, pending.planGatePayload.covered, 2, "old exact authority remains in the audit")
}
