package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// fakeGroupGoalRuns answers GroupGoalRunChecker from a fixed verdict, standing
// in for the operator's occurrence record.
type fakeGroupGoalRuns struct {
	group bool
	err   error
	calls []string
}

func (f *fakeGroupGoalRuns) GroupGoalRun(_ context.Context, ns, name string) (bool, error) {
	f.calls = append(f.calls, ns+"/"+name)
	return f.group, f.err
}

// goalRunClient is the Slack-thread fixture session turned into a goal run.
// Nothing on it says whether the goal is group-owned: that is the operator's
// answer, never the session's.
func goalRunClient(t *testing.T) client.Client {
	t.Helper()
	cli := newFakeK8sClientWithExistingSession(t, "U_MANAGER")
	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), sessKeyForFixture(), &sess))
	sess.Spec.GoalExecution = &spiceboxv1alpha1.GoalExecutionReference{
		GoalID: "goal-1", OccurrenceID: "occ-1", GoalRevision: 1, ConsentDigest: "digest-1",
	}
	require.NoError(t, cli.Update(context.Background(), &sess))
	return cli
}

// bindDecisionHandlers binds a counting handler to each real category for the
// test's lifetime, leaving the production category registry untouched.
func bindDecisionHandlers(t *testing.T, calls *int, names ...string) {
	t.Helper()
	channelinteractions.ResetBindings()
	t.Cleanup(channelinteractions.ResetBindings)
	for _, name := range names {
		channelinteractions.Bind(name, trackingHandler(calls, channelinteractions.Outcome{Result: channelevents.OutcomeApproved}, nil))
	}
}

func canonicalOf(t *testing.T, ext channelevents.ExternalIdentity) string {
	t.Helper()
	c, err := identity.FromExternal(identity.Kind(ext.Kind), identity.TeamScope(ext.TeamScope), identity.RawExternalID(ext.ExternalID), identity.Email(ext.Email)).Canonical()
	require.NoError(t, err)
	return c.String()
}

var (
	goalManager = channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_MANAGER", Email: "manager@example.com"}
	goalMember  = channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_MEMBER", Email: "member@example.com"}
)

// A click on a card posted into a goal run's thread reaches this pipe on the
// session's ordinary inbound subject (Slack's listener and the fake harness
// both PublishIn), not the component tree. For a group goal's run that ingress
// is accepted and the category's own decider policy still decides: only a
// manager (the run's owner, so its approve set) may approve the plan-gate card,
// and only the data's owner the leakage card. A private goal's run keeps
// component-only ingress, and so does a run the operator cannot vouch for.
func TestHandleInteractionDecision_GoalRunChannelIngress(t *testing.T) {
	const dataID = "doc-1"
	cases := []struct {
		name      string
		category  string
		request   channelevents.InteractionRequestPayload
		runs      *fakeGroupGoalRuns // nil: no operator collaborator wired
		component bool
		decider   channelevents.ExternalIdentity
		// handled is whether the bound handler ran; rejected is the class of
		// the per-clicker rejection, empty when none; errContains is the
		// refusal the pipe returns when it refuses before standing.
		handled     bool
		rejected    string
		errContains string
	}{
		{
			name: "group run, plan-gate card, manager's channel click: accepted", category: categories.PlanPhase,
			runs: &fakeGroupGoalRuns{group: true}, decider: goalManager, handled: true,
		},
		{
			name: "group run, plan-gate card, member's channel click: refused not_authorized", category: categories.PlanPhase,
			runs: &fakeGroupGoalRuns{group: true}, decider: goalMember, rejected: "not_authorized",
		},
		{
			name:     "group run, leakage card, data owner's channel click: accepted",
			category: categories.InfoLeakage,
			request:  channelevents.InteractionRequestPayload{Resources: []channelevents.InteractionResourceRef{{Type: "data", ID: dataID}}},
			runs:     &fakeGroupGoalRuns{group: true}, decider: goalManager, handled: true,
		},
		{
			name:     "group run, leakage card, member's channel click: refused not_authorized",
			category: categories.InfoLeakage,
			request:  channelevents.InteractionRequestPayload{Resources: []channelevents.InteractionResourceRef{{Type: "data", ID: dataID}}},
			runs:     &fakeGroupGoalRuns{group: true}, decider: goalMember, rejected: "not_authorized",
		},
		{
			name: "private run, manager's channel click: refused, component ingress required", category: categories.PlanPhase,
			runs: &fakeGroupGoalRuns{group: false}, decider: goalManager, errContains: "component-only ingress",
		},
		{
			name: "private run, component click: accepted", category: categories.PlanPhase,
			runs: &fakeGroupGoalRuns{group: false}, component: true, decider: goalManager, handled: true,
		},
		{
			name: "operator check fails: channel click refused", category: categories.PlanPhase,
			runs: &fakeGroupGoalRuns{err: errors.New("goal service unavailable")}, decider: goalManager, errContains: "goal service unavailable",
		},
		{
			name: "no operator check wired: channel click refused", category: categories.PlanPhase,
			decider: goalManager, errContains: "component-only ingress",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			bindDecisionHandlers(t, &calls, categories.PlanPhase, categories.InfoLeakage)
			sessKey := sessKeyForFixture()
			manager := canonicalOf(t, goalManager)
			az := &fakeAuthz{
				checkApproveFn: func(c string) (bool, error) { return c == manager, nil },
				checkOwnerFn: func(resType, resID, c string) (bool, error) {
					return resType == "data" && resID == dataID && c == manager, nil
				},
			}
			natsRec := &fakeNATS{}
			p := newTestPipeline(t, goalRunClient(t), az, natsRec)
			if tc.runs != nil {
				p.GroupGoalRuns = tc.runs
			}
			mem := newTestMemory(t)
			p.Mem = mem
			notePendingInteractionRequest(t, mem, sessKey, tc.category, "req-1", tc.request)

			ctx := context.Background()
			if tc.component {
				ctx = channelevents.WithComponentDecisionIngress(ctx)
			}
			err := p.HandleInteractionDecision(ctx, mustBuildInteractionDecision(t, sessKey, tc.category, "req-1", "approve", tc.decider))
			if tc.errContains != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errContains)
				assert.Empty(t, natsRec.subjects, "a refused ingress publishes nothing")
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.handled, calls == 1, "bound handler ran")
			if tc.handled {
				applied := findInteractionAppliedPayload(t, natsRec, ".in")
				assert.Equal(t, channelevents.OutcomeApproved, applied.Outcome)
			}
			if tc.rejected != "" {
				assert.Equal(t, tc.rejected, findInteractionDecisionRejected(t, natsRec).Class)
				assert.False(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied), "a refused decider resolves nothing")
			}
			if tc.runs != nil && !tc.component {
				assert.Equal(t, []string{sessKey.Namespace + "/" + sessKey.Name}, tc.runs.calls, "the operator is asked about exactly this session")
			}
		})
	}
}
