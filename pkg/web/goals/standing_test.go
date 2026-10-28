package goals

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type standingRecorder struct {
	mem   memory.Memory
	scope memory.Scope
}

func (r standingRecorder) Record(ctx context.Context, c plangateaudit.Content) error {
	return plangateaudit.Record(ctx, r.mem, r.scope, c)
}

// Called by the signed production consent/dispatch regression for both default
// and unattended schedules. The real runner freezes the plan; the real HTTP
// client asks the authenticated production server; the enforcing hook records
// and replays the approval from signed memory before clearing a call.
func verifyProductionPlanApproval(t *testing.T, f *serverFixture, sess *v1.AgentSession, g domain.Goal, unattended bool, signer *provenance.Signer) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "plan_gate")
	signed := provenance.NewSigningMemory(f.mem, signer)
	handle, err := permsurface.NewPermHandle("execute", ExecutionResourceType)
	require.NoError(t, err)
	surface := []permsurface.Descriptor{{Handle: handle, Permission: "execute", ResourceType: ExecutionResourceType, StateImpact: authz.Readwrite}}
	l := &runner.Loop{Mem: signed, SessionKey: memory.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}, PlanGateMode: "enforcing", PlanGateSurface: surface, PlanGateMaxAutoApprove: 0}
	authored := []plangate.AuthoredPhase{{ID: "deliver", Label: "Deliver reminder", Permissions: []plangate.AuthoredPermission{{Handle: handle.String(), Why: "Approved private reminder"}}}}
	if unattended {
		status, _ := f.callSession(t, "root-token", sess.Name, domain.Request{Operation: "authorize_plan", PlanApproval: &domain.PlanApprovalRequest{Digest: "not-declared", Phase: 0}})
		require.Equal(t, 404, status, "standing consent cannot replace a fresh plan")
	}
	_, err = l.FreezeAndRecordPhases(ctx, authored)
	require.NoError(t, err)
	plan, ok := l.ActiveFrozenPlanNow()
	require.True(t, ok)
	request := domain.Request{Operation: "authorize_plan", PlanApproval: &domain.PlanApprovalRequest{Digest: plan.Digest(), Phase: 0}}
	status, response := f.callSession(t, "root-token", sess.Name, request)
	require.Equal(t, 200, status)
	if !unattended {
		assert.Nil(t, response.PlanApproval, "scheduling consent alone cannot authorize action plans")
		return
	}
	require.NotNil(t, response.PlanApproval)
	assert.Equal(t, g.Execution.Digest, response.PlanApproval.Reference)
	assert.Equal(t, g.Execution.Decision.RequestID, response.PlanApproval.DecisionRef)
	assert.Equal(t, sess.Spec.GoalExecution.OccurrenceID, response.PlanApproval.OccurrenceID)
	assert.Equal(t, string(sess.UID), response.PlanApproval.SessionUID)
	server := httptest.NewServer(f.s)
	t.Cleanup(server.Close)
	client := httpclient.New(server.URL, "root-token")
	deriver := runner.GoalPlanApprovalDeriver(func(ctx context.Context, request domain.Request) (domain.Response, error) {
		return client.Goals(ctx, sess.Namespace, sess.Name, request)
	})
	newGate := func() *hooks.PlanGate {
		return hooks.NewPlanGate(hooks.PlanGateDeps{Mode: "enforcing", RequirePlan: true, CurrentPlan: l.ActiveFrozenPlanNow, Records: l.PlanGateRecords,
			Resolve: func(string, map[string]any) (permsurface.Handle, bool) { return handle, true }, DeriveApproval: deriver,
			Recorder: standingRecorder{mem: signed, scope: memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}}})
	}
	input := pipeline.Input{Point: pipeline.PreToolCall, Session: pipeline.SessionRef{Namespace: sess.Namespace, Name: sess.Name}, Tool: &pipeline.ToolCallInfo{Name: "respond_to_user", UseID: "delivery"}}
	for i := 0; i < 2; i++ {
		// A new gate on each call stands for rehydration: only the signed log
		// survives. Both calls must clear without another human prompt.
		decision := newGate().Eval(ctx, input)
		require.Equal(t, pipeline.Allow, decision.Verdict)
		require.Nil(t, decision.Approval)
	}
	count := 0
	for _, rec := range l.PlanGateRecords() {
		if rec.Event == plangateaudit.EventPhaseApproved && rec.ApprovalAuthority != nil {
			count++
		}
	}
	assert.Equal(t, 1, count, "repeated calls reuse the durable per-plan clearance")
	for _, req := range []*domain.PlanApprovalRequest{nil, {Digest: "stale", Phase: 0}, {Digest: plan.Digest(), Phase: 1}} {
		status, _ := f.callSession(t, "root-token", sess.Name, domain.Request{Operation: "authorize_plan", PlanApproval: req})
		assert.NotEqual(t, 200, status)
	}
	status, _ = f.callSession(t, "token", "session", request)
	assert.Equal(t, 404, status, "source management session cannot derive root authority")
	status, _ = f.callSession(t, "foreign-token", sess.Name, request)
	assert.Equal(t, 401, status)
	// Broader permissions, slots, and new consents must not fit this grant.
	foreign, err := permsurface.NewPermHandle("write", "foreign_resource")
	require.NoError(t, err)
	for _, mutate := range []func(*plangate.Phase){
		func(p *plangate.Phase) { p.Permissions = append(p.Permissions, foreign) },
		func(p *plangate.Phase) {
			p.Slots = append(p.Slots, plangate.Slot{Type: "foreign_resource", ID: "other"})
		},
		func(p *plangate.Phase) { p.Consents = append(p.Consents, []byte(`{"requestRef":"new-consent"}`)) },
	} {
		wider := plangate.Plan{Phases: []plangate.Phase{plan.Phases[0]}}
		mutate(&wider.Phases[0])
		declared := plangate.PhaseAuthorityRecord(wider, 0, nil)
		declared.Event, declared.PlanDigest, declared.Mode = plangateaudit.EventPlanApproved, wider.Digest(), "enforcing"
		require.NoError(t, plangateaudit.Record(ctx, signed, memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}, declared))
		status, _ := f.callSession(t, "root-token", sess.Name, domain.Request{Operation: "authorize_plan", PlanApproval: &domain.PlanApprovalRequest{Digest: wider.Digest(), Phase: 0}})
		assert.Equal(t, 404, status, fmt.Sprintf("reject broadened authority: %+v", wider.Phases[0]))
	}
	_, err = l.FreezeAndRecordPhases(ctx, authored)
	require.NoError(t, err)
	denial := plangate.PhaseAuthorityRecord(plan, 0, nil)
	denial.Event, denial.PlanDigest = plangateaudit.EventDenied, plan.Digest()
	require.NoError(t, plangateaudit.Record(ctx, signed, memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}, denial))
	status, _ = f.callSession(t, "root-token", sess.Name, request)
	assert.Equal(t, 404, status, "a standing grant cannot reverse a recorded human denial")

}
