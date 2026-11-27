package hooks

import (
	"context"
	"errors"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type standingRecordFunc func(context.Context, plangateaudit.Content) error

func (f standingRecordFunc) Record(ctx context.Context, c plangateaudit.Content) error {
	return f(ctx, c)
}

func TestStandingApprovalFailsClosed(t *testing.T) {
	for _, condition := range []string{"authority error", "missing recorder", "write error", "unreadable write", "incomplete authority", "plan changed", "human denial"} {
		t.Run(condition+": call denied and no automatic clearance", func(t *testing.T) {
			desc := permDesc(t, "execute", "agent_goal_execution", "respond_to_user", authz.Readwrite)
			plan := plangate.SessionPlan([]permsurface.Descriptor{desc})
			records := []plangateaudit.Content{plangate.PhaseAuthorityRecord(plan, 0, nil)}
			records[0].Event, records[0].PlanDigest, records[0].Mode = plangateaudit.EventPlanApproved, plan.Digest(), "enforcing"
			if condition == "human denial" {
				r := plangate.PhaseAuthorityRecord(plan, 0, nil)
				r.Event, r.PlanDigest = plangateaudit.EventDenied, plan.Digest()
				records = append(records, r)
			}
			calls := 0
			deriver := func(context.Context, plangate.Plan, int) (*plangateaudit.ApprovalAuthority, error) {
				calls++
				if condition == "authority error" {
					return nil, errors.New("revoked")
				}
				if condition == "plan changed" {
					plan.Phases[0].Max.Count++
				}
				a := &plangateaudit.ApprovalAuthority{Kind: "test", Reference: "grant", DecisionRef: "signed-human-decision", OccurrenceID: "occurrence", SessionUID: "root"}
				if condition == "incomplete authority" {
					a.DecisionRef = ""
				}
				return a, nil
			}
			var recorder PlanGateRecorder = standingRecordFunc(func(_ context.Context, c plangateaudit.Content) error {
				if condition == "write error" {
					return errors.New("storage unavailable")
				}
				if condition != "unreadable write" {
					records = append(records, c)
				}
				return nil
			})
			if condition == "missing recorder" {
				recorder = nil
			}
			h := NewPlanGate(PlanGateDeps{Mode: "enforcing", RequirePlan: true, CurrentPlan: func() (plangate.Plan, bool) { return plan, true }, Records: func() []plangateaudit.Content { return records }, Resolve: surfaceResolver(t, []permsurface.Descriptor{desc}), DeriveApproval: deriver, Recorder: recorder})
			decision := evalTool(t, h, "respond_to_user")
			assert.Equal(t, pipeline.Deny, decision.Verdict)
			assert.Nil(t, decision.Approval)
			if condition == "human denial" {
				assert.Zero(t, calls, "a standing grant cannot undo a human refusal")
			}
			for _, r := range records {
				require.NotEqual(t, plangateaudit.EventPhaseApproved, r.Event)
			}
		})
	}
}

// groupStandingGate is a one-phase plan whose standing authority comes back
// as goal_group_standing for approver, recorded into records.
func groupStandingGate(t *testing.T, bind func(context.Context, plangateaudit.Content, identity.Principal) error) (*PlanGate, *[]plangateaudit.Content) {
	t.Helper()
	desc := permDesc(t, "execute", "agent_goal_execution", "respond_to_user", authz.Readwrite)
	plan := plangate.SessionPlan([]permsurface.Descriptor{desc})
	records := []plangateaudit.Content{plangate.PhaseAuthorityRecord(plan, 0, nil)}
	records[0].Event, records[0].PlanDigest, records[0].Mode = plangateaudit.EventPlanApproved, plan.Digest(), "enforcing"
	deriver := func(context.Context, plangate.Plan, int) (*plangateaudit.ApprovalAuthority, error) {
		return &plangateaudit.ApprovalAuthority{Kind: "goal_group_standing", Approver: "approver-canonical", Reference: "grant",
			DecisionRef: "signed-human-decision", OccurrenceID: "occurrence", SessionUID: "root"}, nil
	}
	recorder := standingRecordFunc(func(_ context.Context, c plangateaudit.Content) error {
		records = append(records, c)
		return nil
	})
	return NewPlanGate(PlanGateDeps{Mode: "enforcing", RequirePlan: true, CurrentPlan: func() (plangate.Plan, bool) { return plan, true },
		Records: func() []plangateaudit.Content { return records }, Resolve: surfaceResolver(t, []permsurface.Descriptor{desc}),
		DeriveApproval: deriver, Recorder: recorder, BindStanding: bind}), &records
}

func TestGroupStandingApprovalBindsTheRecordedPhase(t *testing.T) {
	var bound []plangateaudit.Content
	var approvers []string
	h, records := groupStandingGate(t, func(_ context.Context, rec plangateaudit.Content, approver identity.Principal) error {
		c, err := approver.Canonical()
		require.NoError(t, err)
		bound = append(bound, rec)
		approvers = append(approvers, c.String())
		return nil
	})
	decision := evalTool(t, h, "respond_to_user")
	assert.Equal(t, pipeline.Allow, decision.Verdict)
	require.Len(t, bound, 1, "bound once")
	assert.Equal(t, []string{"approver-canonical"}, approvers, "the authority's approver is the delegator")
	var approved []plangateaudit.Content
	for _, r := range *records {
		if r.Event == plangateaudit.EventPhaseApproved {
			approved = append(approved, r)
		}
	}
	require.Len(t, approved, 1)
	assert.Equal(t, approved[0], bound[0], "bound with exactly the content that was recorded")

	evalTool(t, h, "respond_to_user")
	assert.Len(t, bound, 1, "a durably approved phase is not re-derived or re-bound")
}

func TestGroupStandingApprovalThatCannotBindDenies(t *testing.T) {
	t.Run("bind error: Deny naming the bind", func(t *testing.T) {
		h, _ := groupStandingGate(t, func(context.Context, plangateaudit.Content, identity.Principal) error {
			return errors.New("grant write failed")
		})
		decision := evalTool(t, h, "respond_to_user")
		assert.Equal(t, pipeline.Deny, decision.Verdict)
		assert.Contains(t, decision.Reason, "standing approval could not bind")
		assert.Nil(t, decision.Approval)
	})
	t.Run("no binder wired: Deny and nothing recorded", func(t *testing.T) {
		h, records := groupStandingGate(t, nil)
		decision := evalTool(t, h, "respond_to_user")
		assert.Equal(t, pipeline.Deny, decision.Verdict)
		for _, r := range *records {
			assert.NotEqual(t, plangateaudit.EventPhaseApproved, r.Event, "an approval nothing can bind is never recorded")
		}
	})
}
