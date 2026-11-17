package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalconsent"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// fakeGoalManager answers ManageGoals with a canned response and records
// each request.
type fakeGoalManager struct {
	out  goals.ManageResponse
	err  error
	reqs []goals.ManageRequest
}

func (f *fakeGoalManager) ManageGoals(_ context.Context, r goals.ManageRequest) (goals.ManageResponse, error) {
	f.reqs = append(f.reqs, r)
	return f.out, f.err
}

// countingConsent counts commits and pre-checks; it never reaches a server.
type countingConsent struct{ commits, checks int }

func (c *countingConsent) CommitGoalConsent(context.Context, memory.Entry) error {
	c.commits++
	return nil
}
func (c *countingConsent) CheckGoalConsent(context.Context, memory.Entry, string, bool) error {
	c.checks++
	return nil
}

func TestMemoryClientGoalsReturnsLiveNonNilClient(t *testing.T) {
	_, _, cli := newTestMemoryServer(t)
	g := cli.Goals()
	require.NotNil(t, g, "Goals() must return a genuinely non-nil interface")
	gc, ok := g.(*goalsClient)
	require.True(t, ok)
	assert.Same(t, cli.client, gc.api, "backed by the same httpclient the facade holds")
	assert.Same(t, cli.client, gc.consent, "and commits decisions through it")
}

func TestBuildListenerDeps_WiresGoals(t *testing.T) {
	m := newChannelManager(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	require.Nil(t, m.buildListenerDeps(&spiceboxv1alpha1.Channel{}, nil).Goals, "unset: a genuinely nil Deps.Goals")
	client := &goalsClient{}
	m.goals = client
	assert.Same(t, client, m.buildListenerDeps(&spiceboxv1alpha1.Channel{}, nil).Goals)
}

func TestGoalsClientAct(t *testing.T) {
	viewer := identity.CanonicalFromTrusted("viewer-1", "test")
	unsigned, err := json.Marshal(goalconsent.Content{Goal: goals.Goal{ID: "goal-1", Execution: &goals.ExecutionConsent{Digest: "d1"}}})
	require.NoError(t, err)
	for _, tc := range []struct {
		name     string
		api      *fakeGoalManager
		sentinel error
		contains string
	}{
		{name: "refusal: the server's sentinel, with the action, goal and viewer", api: &fakeGoalManager{err: fmt.Errorf("%w: only a manager can", goals.ErrDenied)},
			sentinel: goals.ErrDenied, contains: "remove agent_goal:goal-1 for viewer-1"},
		{name: "no approval asked: done, nothing recorded", api: &fakeGoalManager{}},
		{name: "an approval request not signed by the operator: refused before any check or record",
			api:      &fakeGoalManager{out: goals.ManageResponse{Approval: &memory.Entry{Kind: goalconsent.KindName, ID: "goalconsent-request-d1", Content: unsigned}}},
			contains: "platform-signed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			consent := &countingConsent{}
			c := &goalsClient{api: tc.api, consent: consent, mem: memory.NewLocal(inmem.NewBackend())}
			err := c.Act(context.Background(), viewer, "team", "agent_goal:goal-1", goals.ActRemove, "")
			require.Len(t, tc.api.reqs, 1)
			assert.Equal(t, goals.ManageRequest{Operation: goals.ManageAct, Viewer: "viewer-1", Namespace: "team", Goal: "agent_goal:goal-1", Action: goals.ActRemove}, tc.api.reqs[0])
			assert.Zero(t, consent.checks, "never asked the server to check a decision")
			assert.Zero(t, consent.commits, "never committed one")
			if tc.sentinel == nil && tc.contains == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			if tc.sentinel != nil {
				assert.ErrorIs(t, err, tc.sentinel)
			}
			assert.Contains(t, err.Error(), tc.contains)
		})
	}
}

// An approval's act carries the plan digest the viewer was shown to the goal
// server, unchanged.
func TestGoalsClientActCarriesThePlanDigest(t *testing.T) {
	viewer := identity.CanonicalFromTrusted("viewer-1", "test")
	api := &fakeGoalManager{}
	c := &goalsClient{api: api, consent: &countingConsent{}, mem: memory.NewLocal(inmem.NewBackend())}
	require.NoError(t, c.Act(context.Background(), viewer, "team", "agent_goal:goal-1", goals.ActRenew, "approval-binding:d1"))
	require.Len(t, api.reqs, 1)
	assert.Equal(t, goals.ManageRequest{Operation: goals.ManageAct, Viewer: "viewer-1", Namespace: "team", Goal: "agent_goal:goal-1", Action: goals.ActRenew, Digest: "approval-binding:d1"}, api.reqs[0])
}

func TestGoalsClientStandingPlan(t *testing.T) {
	viewer := identity.CanonicalFromTrusted("viewer-1", "test")
	plan := &plangate.Plan{Phases: []plangate.Phase{{Label: "read"}}}
	for _, tc := range []struct {
		name   string
		action string
		out    goals.ManageResponse
		want   channelkinds.GoalPlan
		err    error
	}{
		{name: "renew: the plan to approve, its digest and the goal's title", action: goals.ActRenew,
			out:  goals.ManageResponse{Plan: plan, Title: "Weekly", Digest: "approval-binding:d1"},
			want: channelkinds.GoalPlan{Title: "Weekly", Plan: *plan, Digest: "approval-binding:d1"}},
		{name: "resume with no digest: not found, nothing to bind an approval to", action: goals.ActResume,
			out: goals.ManageResponse{Plan: plan, Title: "Weekly"}, err: goals.ErrNotFound},
		{name: "view with no plan: not found", out: goals.ManageResponse{Title: "Weekly"}, err: goals.ErrNotFound},
		{name: "remove with no plan: the title alone", action: goals.ActRemove,
			out: goals.ManageResponse{Title: "Weekly"}, want: channelkinds.GoalPlan{Title: "Weekly"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeGoalManager{out: tc.out}
			c := &goalsClient{api: api}
			got, err := c.StandingPlan(context.Background(), viewer, "team", "agent_goal:goal-1", tc.action)
			require.Len(t, api.reqs, 1)
			assert.Equal(t, goals.ManageRequest{Operation: goals.ManagePlan, Viewer: "viewer-1", Namespace: "team", Goal: "agent_goal:goal-1", Action: tc.action}, api.reqs[0])
			if tc.err != nil {
				assert.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// An in-modal decision is recorded in the goal's management host scope, a
// session-shaped scope no AgentSession can have. channelsd's signed writes
// reach it through the operator's ordinary memory route, and read back.
func TestGoalConsentRecordReachesTheManagementHostScope(t *testing.T) {
	_, _, cli := newTestMemoryServer(t)
	g := goals.Goal{ID: "goal-1", Domain: goals.Domain{Namespace: "team", Owner: "group:g1#member", Class: "assistant", ClassUID: "u"}}
	scope := memory.Scope{Kind: "session", ID: goals.ManagementHost(g).Session}
	raw, err := json.Marshal(goalconsent.Content{Goal: g})
	require.NoError(t, err)
	ctx := t.Context()
	_, err = cli.signed.Put(ctx, memory.Entry{Scope: scope, Kind: goalconsent.KindName, ID: "goalconsent-decision-d1", Content: raw, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)})
	require.NoError(t, err)
	got, found, err := goalconsent.Find(ctx, cli.signed, scope, "goalconsent-decision-d1")
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, got.Provenance)
	assert.Equal(t, "system:channelsd", got.Provenance.Publisher)
}
