package hooks

import (
	"context"
	"errors"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
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
