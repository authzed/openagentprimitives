package goals

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	core "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionschedule"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/goals/sqlstore"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalconsent"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// LookupResourceIDs answers "which agent_goal_domains may user view" from
// the same model CheckOnResource uses, and records each lookup.
func (a *groupAuthority) LookupResourceIDs(ctx context.Context, typ, perm string, u identity.CanonicalUserID) ([]string, bool, error) {
	a.lookups = append(a.lookups, typ+"#"+perm+"@"+u.String())
	seen := map[string]bool{}
	var out []string
	for t := range a.tuples {
		if t.ResourceType != typ || seen[t.ResourceID] {
			continue
		}
		seen[t.ResourceID] = true
		ok, err := a.CheckOnResource(ctx, typ, t.ResourceID, perm, u, true)
		if err != nil {
			return nil, false, err
		}
		if ok {
			out = append(out, t.ResourceID)
		}
	}
	return out, false, nil
}

// manageFlow is an approved daily series (alice and carol manage it, bob is
// a plain member) whose server also serves the management API.
type manageFlow struct {
	*groupConsentFlow
	dave, erin string
}

func newManageFlow(t *testing.T) *manageFlow {
	t.Helper()
	c := newApprovedSeries(t)
	c.f.s.Consent = c.publisher
	return &manageFlow{groupConsentFlow: c, dave: canonOf(t, "dave@example.com"), erin: canonOf(t, "erin@example.com")}
}

func (m *manageFlow) manage(t *testing.T, token string, r core.ManageRequest) (int, core.ManageResponse, string) {
	t.Helper()
	b, err := json.Marshal(r)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/goals/manage", bytes.NewReader(b))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	m.f.s.ServeHTTP(w, req)
	var out core.ManageResponse
	if w.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	}
	return w.Code, out, w.Body.String()
}

func (m *manageFlow) list(t *testing.T, viewer string) core.ManageResponse {
	t.Helper()
	status, out, body := m.manage(t, "channelsd", core.ManageRequest{Operation: core.ManageList, Viewer: viewer, Namespace: groupNS, Class: "assistant", Limit: 10})
	require.Equal(t, http.StatusOK, status, body)
	return out
}

// act takes action as viewer the way a surface does: an approval (resume,
// renew) first reads the plan it approves and hands its digest back.
func (m *manageFlow) act(t *testing.T, viewer, action string) (int, core.ManageResponse, string) {
	t.Helper()
	shown := ""
	if action == core.ActResume || action == core.ActRenew {
		if status, out, _ := m.plan(t, viewer, action); status == http.StatusOK {
			shown = out.Digest
		}
	}
	return m.actShown(t, viewer, action, shown)
}

// actShown takes action as viewer, naming the plan digest it was shown.
func (m *manageFlow) actShown(t *testing.T, viewer, action, digest string) (int, core.ManageResponse, string) {
	t.Helper()
	return m.manage(t, "channelsd", core.ManageRequest{Operation: core.ManageAct, Viewer: viewer, Namespace: groupNS, Goal: GoalResourceType + ":" + m.goal.ID, Action: action, Digest: digest})
}

// plan reads the goal's plan as viewer, as shown before action.
func (m *manageFlow) plan(t *testing.T, viewer, action string) (int, core.ManageResponse, string) {
	t.Helper()
	return m.manage(t, "channelsd", core.ManageRequest{Operation: core.ManagePlan, Viewer: viewer, Namespace: groupNS, Goal: GoalResourceType + ":" + m.goal.ID, Action: action})
}

// decide completes an in-modal approval as channelsd does: the viewer's
// decision on the operator-signed request, recorded and committed through
// the consent-card path.
func (m *manageFlow) decide(t *testing.T, request memory.Entry, viewer string) error {
	t.Helper()
	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:channelsd"), "system:channelsd")
	return pipeline.DecideGoalConsentFirstParty(ctx, m.f.signed, consentHTTP{server: m.f.s, token: "channelsd"}, request, identity.CanonicalFromTrusted(viewer, "test viewer"), true)
}

func TestManageNeedsTheChannelsdToken(t *testing.T) {
	m := newManageFlow(t)
	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{name: "no token: 401", want: http.StatusUnauthorized},
		{name: "a session's own token: 401 (no session-less actor from a runner)", token: "token", want: http.StatusUnauthorized},
		{name: "an unknown token: 401", token: "nope", want: http.StatusUnauthorized},
		{name: "channelsd: served", token: "channelsd", want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, _, body := m.manage(t, tc.token, core.ManageRequest{Operation: core.ManageList, Viewer: m.bob, Namespace: groupNS, Class: "assistant", Limit: 10})
			assert.Equal(t, tc.want, status, body)
		})
	}
}

// Only a person is a viewer: a run session (which acts as itself) or a
// subject set is refused before anything is looked up.
func TestManageViewerMustBeAPerson(t *testing.T) {
	m := newManageFlow(t)
	for _, viewer := range []string{"agentsession:" + groupNS + "/run-1", groupOwner, "user:*", ""} {
		t.Run(viewer, func(t *testing.T) {
			status, _, body := m.manage(t, "channelsd", core.ManageRequest{Operation: core.ManageAct, Viewer: viewer, Namespace: groupNS, Goal: GoalResourceType + ":" + m.goal.ID, Action: core.ActPause})
			assert.Equal(t, http.StatusBadRequest, status, body)
		})
	}
	assert.Empty(t, m.f.auth.lookups, "nothing was looked up")
	assert.Equal(t, core.Active, m.current(t).State)
}

func TestManageListForViewer(t *testing.T) {
	m := newManageFlow(t)
	// A second goal in the same group, so the cap and the total differ.
	m.f.turnProof(t, m.alice, "r2")
	status, _, body := m.f.as(t, "r2", groupCreate("create-2"))
	require.Equal(t, http.StatusOK, status, body)

	t.Run("a manager sees the group's goals, with what they may do", func(t *testing.T) {
		out := m.list(t, m.alice)
		assert.Equal(t, 2, out.Total)
		require.Len(t, out.Goals, 2)
		var row core.GoalSummary
		for _, g := range out.Goals {
			if g.Resource == GoalResourceType+":"+m.goal.ID {
				row = g
			}
		}
		require.NotEmpty(t, row.Resource, "the approved goal is listed")
		assert.Equal(t, m.goal.Title, row.Title)
		assert.Equal(t, groupOwner, row.Group)
		assert.Equal(t, string(core.Active), row.State)
		assert.NotEmpty(t, row.Schedule)
		require.NotNil(t, row.ApprovalExpires)
		assert.True(t, row.ApprovalExpires.Equal(m.goal.Execution.Terms.ExpiresAt))
		require.NotNil(t, row.NextRun)
		assert.ElementsMatch(t, []string{m.alice, m.carol}, row.Managers)
		assert.False(t, row.NoManagerInGroup)
		assert.True(t, row.CanPause)
		assert.True(t, row.CanManage)
		assert.True(t, row.CanRenew)
		assert.False(t, row.RenewalPending)
		assert.Contains(t, m.f.auth.lookups, ResourceType+"#view_memory@"+m.alice, "found by LookupResources on view_memory")
	})
	t.Run("the cap limits rows, not the total", func(t *testing.T) {
		status, out, body := m.manage(t, "channelsd", core.ManageRequest{Operation: core.ManageList, Viewer: m.alice, Namespace: groupNS, Class: "assistant", Limit: 1})
		require.Equal(t, http.StatusOK, status, body)
		assert.Len(t, out.Goals, 1)
		assert.Equal(t, 2, out.Total)
	})
	t.Run("a plain member may pause but not manage", func(t *testing.T) {
		out := m.list(t, m.bob)
		require.Len(t, out.Goals, 2)
		for _, row := range out.Goals {
			assert.True(t, row.CanPause)
			assert.False(t, row.CanManage)
			assert.False(t, row.CanRenew)
		}
	})
	t.Run("a non-member sees none", func(t *testing.T) {
		out := m.list(t, m.dave)
		assert.Empty(t, out.Goals)
		assert.Zero(t, out.Total)
	})
	t.Run("another class's goals are not listed", func(t *testing.T) {
		status, out, body := m.manage(t, "channelsd", core.ManageRequest{Operation: core.ManageList, Viewer: m.alice, Namespace: groupNS, Class: "other", Limit: 10})
		require.Equal(t, http.StatusOK, status, body)
		assert.Empty(t, out.Goals)
	})
	t.Run("a pending renewal is shown", func(t *testing.T) {
		m.renew(t)
		for _, row := range m.list(t, m.alice).Goals {
			if row.Resource == GoalResourceType+":"+m.goal.ID {
				assert.True(t, row.RenewalPending)
			}
		}
	})
}

// NoManagerInGroup: no subject of the goal's manager relation is still in
// its group. Platform admins step in.
func TestManageNoManagerInGroupAndTheAdminFallback(t *testing.T) {
	m := newManageFlow(t)
	m.f.auth.members[m.alice], m.f.auth.members[m.carol] = false, false
	m.f.auth.admins[m.erin] = true

	out := m.list(t, m.erin)
	require.Len(t, out.Goals, 1, "an admin sees the class's group goals")
	row := out.Goals[0]
	assert.True(t, row.NoManagerInGroup)
	assert.True(t, row.CanManage, "an admin is the fallback manager")
	assert.True(t, row.CanPause)

	status, _, body := m.act(t, m.erin, core.ActPause)
	require.Equal(t, http.StatusOK, status, body)
	m.goal = m.current(t)
	status, res, body := m.act(t, m.erin, core.ActResume)
	require.Equal(t, http.StatusOK, status, body)
	require.NotNil(t, res.Approval, "the resume asks for the admin's approval in the same click")
	require.NoError(t, m.decide(t, *res.Approval, m.erin))
	g := m.current(t)
	assert.Equal(t, core.Active, g.State)
	require.NotNil(t, g.Execution.Decision)
	assert.Equal(t, m.erin, g.Execution.Decision.Owner, "the admin is the approver")
	assert.True(t, core.ApprovalInForce(g, time.Now()))
}

func TestManageAct(t *testing.T) {
	cases := []struct {
		name, action string
		viewer       func(m *manageFlow) string
		status       int
		state        core.State
	}{
		{name: "member pauses: ok", action: core.ActPause, viewer: func(m *manageFlow) string { return m.bob }, status: http.StatusOK, state: core.Paused},
		{name: "member removes: denied", action: core.ActRemove, viewer: func(m *manageFlow) string { return m.bob }, status: http.StatusForbidden, state: core.Active},
		{name: "member renews: denied", action: core.ActRenew, viewer: func(m *manageFlow) string { return m.bob }, status: http.StatusForbidden, state: core.Active},
		{name: "member resumes: denied", action: core.ActResume, viewer: func(m *manageFlow) string { return m.bob }, status: http.StatusForbidden, state: core.Active},
		{name: "non-member pauses: not found", action: core.ActPause, viewer: func(m *manageFlow) string { return m.dave }, status: http.StatusNotFound, state: core.Active},
		{name: "manager removes: cancelled", action: core.ActRemove, viewer: func(m *manageFlow) string { return m.carol }, status: http.StatusOK, state: core.Cancelled},
		{name: "unknown action: invalid", action: "delete", viewer: func(m *manageFlow) string { return m.carol }, status: http.StatusBadRequest, state: core.Active},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newManageFlow(t)
			status, res, body := m.act(t, tc.viewer(m), tc.action)
			assert.Equal(t, tc.status, status, body)
			assert.Nil(t, res.Approval, "nothing to approve")
			assert.Equal(t, tc.state, m.current(t).State)
		})
	}
}

// Resume is a manager's in-modal approval of the last approved terms: the
// click is the decision, recorded on the consent-card path, and nothing is
// posted into the conversation.
func TestManageResumeIsAnInModalApproval(t *testing.T) {
	m := newManageFlow(t)
	before := *m.goal.LastApprovedTerms
	status, _, body := m.act(t, m.bob, core.ActPause)
	require.Equal(t, http.StatusOK, status, body)
	published := m.published

	status, res, body := m.act(t, m.carol, core.ActResume)
	require.Equal(t, http.StatusOK, status, body)
	require.NotNil(t, res.Approval)
	g := m.current(t)
	assert.Equal(t, core.Active, g.State)
	require.NotNil(t, g.Execution)
	assert.Nil(t, g.Execution.Decision, "approved only once the viewer's decision is recorded")
	assert.True(t, g.Execution.ManagementHosted())
	assert.Equal(t, "goalconsent-request-"+g.Execution.Digest, res.Approval.ID)
	require.NoError(t, m.publisher.Notify(context.Background(), core.Event{Goal: g, Action: "request_execution"}))
	assert.Equal(t, published, m.published, "no card is posted into the conversation")

	require.NoError(t, m.decide(t, *res.Approval, m.carol))
	g = m.current(t)
	require.NotNil(t, g.Execution.Decision)
	assert.Equal(t, m.carol, g.Execution.Decision.Owner, "the viewer is the approver")
	require.NoError(t, m.f.s.VerifyDecision(context.Background(), g, *g.Execution.Decision), "the same signed decision record a card click makes")
	assert.True(t, core.ApprovalInForce(g, time.Now()))
	want := before
	want.DueAt, want.ExpiresAt, want.ScheduleWindows = g.Execution.Terms.DueAt, g.Execution.Terms.ExpiresAt, g.Execution.Terms.ScheduleWindows
	assert.Equal(t, want, g.Execution.Terms, "the last approved terms, with a fresh window")
	assert.Len(t, m.grantFor(g.Execution.Digest), 2, "the domain-anchored grant is written")
	assert.Equal(t, published, m.published, "still nothing posted")
}

func TestManageResumeChecksTheApproversStartGateLive(t *testing.T) {
	m := newManageFlow(t)
	status, _, body := m.act(t, m.bob, core.ActPause)
	require.Equal(t, http.StatusOK, status, body)
	m.f.auth.startDenied = map[string]bool{m.carol: true}
	status, _, body = m.act(t, m.carol, core.ActResume)
	assert.Equal(t, http.StatusForbidden, status, body)
	assert.Contains(t, body, "start sessions")
	assert.Equal(t, core.Paused, m.current(t).State, "checked before resuming, so the goal is not left half-resumed")
}

// A click whose decision never landed (channelsd failed after the request)
// is offered again on the next click, not asked twice.
func TestManageResumeRetryOffersTheSameRequest(t *testing.T) {
	m := newManageFlow(t)
	status, _, body := m.act(t, m.bob, core.ActPause)
	require.Equal(t, http.StatusOK, status, body)
	status, first, body := m.act(t, m.carol, core.ActResume)
	require.Equal(t, http.StatusOK, status, body)
	require.NotNil(t, first.Approval)
	revision := m.current(t).Revision

	status, again, body := m.act(t, m.carol, core.ActResume)
	require.Equal(t, http.StatusOK, status, body)
	require.NotNil(t, again.Approval)
	assert.Equal(t, first.Approval.ID, again.Approval.ID)
	assert.Equal(t, revision, m.current(t).Revision, "nothing new was requested")
	require.NoError(t, m.decide(t, *again.Approval, m.carol))

	status, done, body := m.act(t, m.carol, core.ActResume)
	assert.Equal(t, http.StatusOK, status, body, "a repeated click is not an error")
	assert.Nil(t, done.Approval, "an approved, active goal has nothing left to approve")
}

// The audit event of a management change names no session and keeps the
// first-party attestation, with the viewer and action.
func TestManageChangeIsAuditedWithTheFirstPartyAttestation(t *testing.T) {
	m := newManageFlow(t)
	status, _, body := m.act(t, m.bob, core.ActPause)
	require.Equal(t, http.StatusOK, status, body)
	events, err := m.store.Pending(context.Background(), 1000)
	require.NoError(t, err)
	last := events[len(events)-1]
	assert.Equal(t, "pause", last.Action)
	assert.Empty(t, last.Session)
	var att firstPartyAttestation
	require.NoError(t, json.Unmarshal(last.ActorProof, &att))
	assert.Equal(t, firstPartyAttestation{RecordedBy: "system:operator", Via: "channelsd-token", Unsigned: true,
		Surface: "management", Viewer: m.bob, Goal: m.goal.ID, Action: core.ActPause, At: att.At}, att,
		"recorded by the operator on channelsd's token's word, and says it is unsigned")

	// It must not pass for a signed component entry: no publisher claim, and
	// nothing an entry verifier would read as provenance.
	var raw map[string]any
	require.NoError(t, json.Unmarshal(last.ActorProof, &raw))
	assert.NotContains(t, raw, "publisher")
	assert.NotContains(t, raw, "provenance")
	var asEntry memory.Entry
	require.NoError(t, json.Unmarshal(last.ActorProof, &asEntry))
	assert.Nil(t, asEntry.Provenance, "decoded as an entry, it carries no provenance to verify")
	assert.NotContains(t, string(last.ActorProof), "system:channelsd")
}

func TestManageRenew(t *testing.T) {
	t.Run("approves the pending renewal", func(t *testing.T) {
		m := newManageFlow(t)
		renewed := m.renew(t)
		status, res, body := m.act(t, m.carol, core.ActRenew)
		require.Equal(t, http.StatusOK, status, body)
		require.NotNil(t, res.Approval)
		assert.Equal(t, "goalconsent-request-"+renewed.PendingRenewal.Digest, res.Approval.ID, "the renewal already asked for")
		require.NoError(t, m.decide(t, *res.Approval, m.carol))
		g := m.current(t)
		assert.Equal(t, renewed.PendingRenewal.Digest, g.Execution.Digest)
		assert.Equal(t, m.carol, g.Execution.Decision.Owner)
		assert.Equal(t, m.goal.Execution.Digest, g.PriorExecution.Digest, "the renewed approval keeps its runs")
		assert.Equal(t, m.goal.Revision, g.Revision, "renewal changes consent in place")
	})
	t.Run("with none pending, renews on the approval in force", func(t *testing.T) {
		m := newManageFlow(t)
		published := m.published
		status, res, body := m.act(t, m.alice, core.ActRenew)
		require.Equal(t, http.StatusOK, status, body)
		require.NotNil(t, res.Approval)
		pending := m.current(t).PendingRenewal
		require.NotNil(t, pending)
		assert.True(t, pending.ManagementHosted())
		require.NoError(t, m.publisher.Notify(context.Background(), core.Event{Goal: m.current(t), Action: "request_renewal"}))
		assert.Equal(t, published, m.published, "no card is posted into the conversation")
		assert.Empty(t, m.notices)
		require.NoError(t, m.decide(t, *res.Approval, m.alice))
		g := m.current(t)
		assert.Equal(t, pending.Digest, g.Execution.Digest)
		assert.Equal(t, m.alice, g.Execution.Decision.Owner)
		assert.Equal(t, m.goal.Execution.Digest, g.PriorExecution.Digest)
		assert.Len(t, m.grantFor(g.Execution.Digest), 2)
	})
	t.Run("after the approval ran out, renews on the last approved terms", func(t *testing.T) {
		m := newManageFlow(t)
		expiry := m.goal.Execution.Terms.ExpiresAt
		now := expiry.Add(time.Hour)
		m.f.s.Service.Now = func() time.Time { return now }
		status, res, body := m.act(t, m.alice, core.ActRenew)
		require.Equal(t, http.StatusOK, status, body)
		require.NotNil(t, res.Approval)
		g := m.current(t)
		require.NotNil(t, g.Execution)
		assert.True(t, g.Execution.Terms.DueAt.After(now), "a fresh window")
		assert.True(t, g.Execution.ManagementHosted())
	})
}

func TestManageStandingPlan(t *testing.T) {
	m := newManageFlow(t)
	for _, tc := range []struct {
		name   string
		viewer string
		status int
	}{
		{name: "a member sees the standing plan", viewer: m.bob, status: http.StatusOK},
		{name: "a non-member does not", viewer: m.dave, status: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, out, body := m.manage(t, "channelsd", core.ManageRequest{Operation: core.ManagePlan, Viewer: tc.viewer, Namespace: groupNS, Goal: GoalResourceType + ":" + m.goal.ID})
			require.Equal(t, tc.status, status, body)
			if tc.status != http.StatusOK {
				return
			}
			require.NotNil(t, out.Plan)
			assert.Equal(t, m.goal.Execution.Terms.StandingPlan.Digest(), out.Plan.Digest())
			assert.Equal(t, m.goal.Title, out.Title)
			assert.Empty(t, out.Digest, "viewing approves nothing, so nothing is bound")
		})
	}
}

// The goal must be in the namespace the surface asked about.
func TestManageGoalInAnotherNamespaceIsNotFound(t *testing.T) {
	m := newManageFlow(t)
	status, _, body := m.manage(t, "channelsd", core.ManageRequest{Operation: core.ManageAct, Viewer: m.bob, Namespace: "elsewhere", Goal: GoalResourceType + ":" + m.goal.ID, Action: core.ActPause})
	assert.Equal(t, http.StatusNotFound, status, body)
	assert.Equal(t, core.Active, m.current(t).State)
}

// requestOtherTerms has alice ask, in her own conversation, for a daily
// series under a different standing plan (the read phase re-pointed at
// another repo), leaving it undecided in Execution.
func (m *manageFlow) requestOtherTerms(t *testing.T) core.Goal {
	t.Helper()
	g := m.current(t)
	phases := groupStandingPhases()
	phases[0].Slots[0].ID = "acme/other"
	terms := core.ExecutionTerms{
		Bounds:            core.ExecutionBounds{DurationSeconds: 180, Turns: 10, Tokens: 10000, ApprovalSeconds: 90},
		AllowedOperations: []string{"respond_to_user", "report_goal_result"}, Evidence: []string{"weekly summary"},
		ActionApproval: "standing_plan",
	}
	dailyTerms(&terms)
	status, r, body := m.f.as(t, "r1", core.Request{Operation: "request_execution", Scope: "group", Resource: groupResource(),
		Execution: core.ExecutionRequest{RequestID: "exec-other", ID: g.ID, Revision: g.Revision, Terms: terms, StandingPlan: phases}})
	require.Equal(t, http.StatusOK, status, body)
	require.NotNil(t, r.Goal.Execution)
	require.Nil(t, r.Goal.Execution.Decision)
	require.False(t, r.Goal.Execution.ManagementHosted())
	return *r.Goal
}

// The plan a manager is shown before resume or renew is the plan the act
// approves: the read and the act share one selection, and the read's digest
// is what the act approves.
func TestManageShownPlanIsThePlanApproved(t *testing.T) {
	cases := []struct {
		name   string
		action string
		setup  func(t *testing.T, m *manageFlow)
	}{
		{name: "resume a paused goal: its last approved terms", action: core.ActResume, setup: func(t *testing.T, m *manageFlow) {
			status, _, body := m.act(t, m.bob, core.ActPause)
			require.Equal(t, http.StatusOK, status, body)
		}},
		{name: "renew with a renewal pending: the pending renewal", action: core.ActRenew, setup: func(t *testing.T, m *manageFlow) { m.renew(t) }},
		{name: "renew with none pending: a renewal of the approval in force", action: core.ActRenew, setup: func(*testing.T, *manageFlow) {}},
		{name: "renew after the approval ran out: the last approved terms", action: core.ActRenew, setup: func(t *testing.T, m *manageFlow) {
			now := m.goal.Execution.Terms.ExpiresAt.Add(time.Hour)
			m.f.s.Service.Now = func() time.Time { return now }
		}},
		{name: "renew while another manager's request is open: the last approved terms, not that request", action: core.ActRenew, setup: func(t *testing.T, m *manageFlow) {
			m.requestOtherTerms(t)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newManageFlow(t)
			approved := *m.goal.LastApprovedTerms.StandingPlan
			tc.setup(t, m)
			other := m.current(t).Execution

			status, shown, body := m.plan(t, m.carol, tc.action)
			require.Equal(t, http.StatusOK, status, body)
			require.NotNil(t, shown.Plan)
			require.NotEmpty(t, shown.Digest)
			assert.Equal(t, m.goal.Title, shown.Title, "the server supplies the title")
			assert.Equal(t, approved, *shown.Plan, "every case here approves the plan last approved")
			if other != nil && other.Decision == nil && !other.ManagementHosted() {
				assert.NotEqual(t, *other.Terms.StandingPlan, *shown.Plan, "the open request's plan is not what renew approves")
			}

			status, res, body := m.actShown(t, m.carol, tc.action, shown.Digest)
			require.Equal(t, http.StatusOK, status, body)
			require.NotNil(t, res.Approval)
			require.NoError(t, m.decide(t, *res.Approval, m.carol))
			g := m.current(t)
			require.NotNil(t, g.Execution.Decision)
			assert.Equal(t, m.carol, g.Execution.Decision.Owner)
			assert.Equal(t, *shown.Plan, *g.Execution.Terms.StandingPlan, "the plan shown is the plan approved")
			binding, err := approvalBinding(g.Execution.Terms)
			require.NoError(t, err)
			assert.Equal(t, shown.Digest, binding, "and so are the rest of its terms")
		})
	}
}

// An approval names the plan it was shown; anything else is refused before
// anything is requested or replaced.
func TestManageApprovalIsBoundToThePlanShown(t *testing.T) {
	t.Run("no digest: refused, the goal stays paused", func(t *testing.T) {
		m := newManageFlow(t)
		status, _, body := m.act(t, m.bob, core.ActPause)
		require.Equal(t, http.StatusOK, status, body)
		status, res, body := m.actShown(t, m.carol, core.ActResume, "")
		assert.Equal(t, http.StatusPreconditionFailed, status, body)
		assert.Contains(t, body, "reopen")
		assert.Nil(t, res.Approval)
		assert.Equal(t, core.Paused, m.current(t).State)
	})
	t.Run("a digest of some other plan: refused", func(t *testing.T) {
		m := newManageFlow(t)
		before := m.current(t)
		status, _, body := m.actShown(t, m.carol, core.ActRenew, "approval-binding:not-this-one")
		assert.Equal(t, http.StatusPreconditionFailed, status, body)
		after := m.current(t)
		assert.Nil(t, after.PendingRenewal, "no renewal was requested")
		assert.Equal(t, before.Revision, after.Revision)
	})
	t.Run("the plan changed after it was shown: refused, the new approval untouched", func(t *testing.T) {
		m := newManageFlow(t)
		status, shown, body := m.plan(t, m.carol, core.ActRenew)
		require.Equal(t, http.StatusOK, status, body)
		// Meanwhile alice asks for, and approves, a different plan.
		m.goal = m.requestOtherTerms(t)
		require.NoError(t, m.publisher.Notify(context.Background(), core.Event{Goal: m.goal, Action: "request_execution"}))
		_, err := m.click(t, "alice@example.com")
		require.NoError(t, err)
		approved := m.current(t)
		require.True(t, core.ApprovalInForce(approved, time.Now()))

		status, res, body := m.actShown(t, m.carol, core.ActRenew, shown.Digest)
		assert.Equal(t, http.StatusPreconditionFailed, status, body)
		assert.Nil(t, res.Approval)
		after := m.current(t)
		assert.Nil(t, after.PendingRenewal, "nothing was renewed")
		assert.Equal(t, approved.Execution.Digest, after.Execution.Digest)
	})
	t.Run("the digest of the plan shown: approved", func(t *testing.T) {
		m := newManageFlow(t)
		status, shown, body := m.plan(t, m.carol, core.ActRenew)
		require.Equal(t, http.StatusOK, status, body)
		status, res, body := m.actShown(t, m.carol, core.ActRenew, shown.Digest)
		require.Equal(t, http.StatusOK, status, body)
		require.NotNil(t, res.Approval)
	})
}

// A system renewal whose request record is gone with the run that hosted it
// is never re-created there: renew falls through to a renewal of the
// approval in force, hosted at the management host.
func TestManageRenewNeverRecreatesAGoneRunScopedRenewalRequest(t *testing.T) {
	m := newManageFlow(t)
	ctx := context.Background()
	// Requested from run-1 but never published: its request record is not in
	// the run's scope, as after the run's scope is collected.
	renewed, err := m.f.s.Service.RequestRenewal(ctx, m.goal.Domain, m.goal.ID, renewalRun, time.Now())
	require.NoError(t, err)
	require.True(t, renewed.PendingRenewal.SystemRenewal())

	status, res, body := m.act(t, m.carol, core.ActRenew)
	require.Equal(t, http.StatusOK, status, body)
	require.NotNil(t, res.Approval)
	g := m.current(t)
	require.NotNil(t, g.PendingRenewal)
	assert.True(t, g.PendingRenewal.ManagementHosted(), "renewed at the management host")
	assert.NotEqual(t, renewed.PendingRenewal.Digest, g.PendingRenewal.Digest, "the run's renewal was replaced")
	_, found, err := goalconsent.Find(operatorCtx(), m.f.mem, memory.Scope{Kind: "session", ID: renewalRun.Session}, "goalconsent-request-"+renewed.PendingRenewal.Digest)
	require.NoError(t, err)
	assert.False(t, found, "nothing was written into the run's scope")

	require.NoError(t, m.decide(t, *res.Approval, m.carol))
	after := m.current(t)
	assert.Equal(t, g.PendingRenewal.Digest, after.Execution.Digest)
	assert.Equal(t, m.carol, after.Execution.Decision.Owner)
}

// newManageFlowTerms is newManageFlow over an approved goal with the given
// terms (nil: one run, due within the hour).
func newManageFlowTerms(t *testing.T, terms func(*core.ExecutionTerms)) *manageFlow {
	t.Helper()
	c := newGroupConsentFlowTerms(t, nil, terms)
	out, err := c.click(t, "alice@example.com")
	require.NoError(t, err)
	require.Equal(t, channelevents.OutcomeApproved, out.Result)
	c.goal = c.current(t)
	c.f.s.Consent = c.publisher
	return &manageFlow{groupConsentFlow: c, dave: canonOf(t, "dave@example.com"), erin: canonOf(t, "erin@example.com")}
}

// onceTerms asks for a schedule of a single run, within the hour: a schedule
// that has no run after its first.
func onceTerms(x *core.ExecutionTerms) {
	due := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	x.DueAt, x.ExpiresAt = due, due.Add(time.Hour)
	x.Schedule = &sessionschedule.Spec{Kind: "once", Timezone: "UTC", MaxRuns: 1, RunWindowSeconds: 3600}
}

// row is the goal's list row as viewer sees it.
func (m *manageFlow) row(t *testing.T, viewer string) core.GoalSummary {
	t.Helper()
	for _, g := range m.list(t, viewer).Goals {
		if g.Resource == GoalResourceType+":"+m.goal.ID {
			return g
		}
	}
	require.FailNow(t, "the goal is not listed for "+viewer)
	return core.GoalSummary{}
}

// A paused goal is resumed only toward an approval that can be offered: one
// with no run left stays paused rather than ending up active with no approval
// and nothing to ask for.
func TestManageResumeNeedsARunLeftToApprove(t *testing.T) {
	cases := []struct {
		name  string
		terms func(*core.ExecutionTerms)
		// at is when the manager resumes, from the goal's last approved terms.
		at        func(core.ExecutionTerms) time.Time
		status    int
		state     core.State
		canResume bool
	}{
		{name: "a one-time run not yet due: resumed", at: func(x core.ExecutionTerms) time.Time { return x.DueAt.Add(-time.Minute) },
			status: http.StatusOK, state: core.Active, canResume: true},
		{name: "a one-time run whose due time passed: refused, still paused, Resume not offered", at: func(x core.ExecutionTerms) time.Time { return x.DueAt.Add(time.Minute) },
			status: http.StatusBadRequest, state: core.Paused},
		{name: "a schedule with a run left: resumed", terms: dailyTerms, at: func(x core.ExecutionTerms) time.Time { return x.DueAt.Add(36 * time.Hour) },
			status: http.StatusOK, state: core.Active, canResume: true},
		{name: "a schedule with no run left: refused, still paused, Resume not offered", terms: onceTerms, at: func(x core.ExecutionTerms) time.Time { return x.DueAt.Add(time.Minute) },
			status: http.StatusBadRequest, state: core.Paused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newManageFlowTerms(t, tc.terms)
			status, _, body := m.act(t, m.bob, core.ActPause)
			require.Equal(t, http.StatusOK, status, body)
			paused := m.current(t)
			now := tc.at(*paused.LastApprovedTerms)
			m.f.s.Service.Now = func() time.Time { return now }

			row := m.row(t, m.carol)
			assert.True(t, row.CanManage)
			assert.Equal(t, tc.canResume, row.CanResume)

			status, res, body := m.act(t, m.carol, core.ActResume)
			assert.Equal(t, tc.status, status, body)
			g := m.current(t)
			assert.Equal(t, tc.state, g.State)
			if tc.status == http.StatusOK {
				assert.NotNil(t, res.Approval, "the re-approval is offered")
				return
			}
			assert.Contains(t, body, "cannot be offered again")
			assert.Equal(t, paused.Revision, g.Revision, "nothing was committed")
			assert.True(t, m.row(t, m.carol).CanManage, "a manager can still remove it")
		})
	}
}

// planlessStore reads every goal as one approved with manual action approval:
// with no standing plan in any of its terms.
type planlessStore struct{ *sqlstore.Store }

func planless(g core.Goal) core.Goal {
	strip := func(c *core.ExecutionConsent) *core.ExecutionConsent {
		if c == nil {
			return nil
		}
		cp := *c
		cp.Terms.ActionApproval, cp.Terms.StandingPlan = "manual", nil
		return &cp
	}
	g.Execution, g.PendingRenewal, g.PriorExecution = strip(g.Execution), strip(g.PendingRenewal), strip(g.PriorExecution)
	if g.LastApprovedTerms != nil {
		terms := *g.LastApprovedTerms
		terms.ActionApproval, terms.StandingPlan = "manual", nil
		g.LastApprovedTerms = &terms
	}
	return g
}

func (p planlessStore) GetInDomains(ctx context.Context, ids []string, id string) (core.Goal, error) {
	g, err := p.Store.GetInDomains(ctx, ids, id)
	return planless(g), err
}

func (p planlessStore) ListDomains(ctx context.Context, ids []string, r core.ListRequest) (core.Page, error) {
	page, err := p.Store.ListDomains(ctx, ids, r)
	for i := range page.Goals {
		page.Goals[i] = planless(page.Goals[i])
	}
	return page, err
}

// A management surface approves by showing a standing plan. A goal with none
// is offered neither Resume nor Renew, and a plan read for either says so
// rather than reporting the goal as gone.
func TestManageGoalWithNoStandingPlanIsNotOfferedAnApproval(t *testing.T) {
	for _, tc := range []struct {
		name, action string
		paused       bool
	}{
		{name: "active: Renew not offered, its plan read refused as invalid", action: core.ActRenew},
		{name: "paused: Resume not offered, its plan read refused as invalid", action: core.ActResume, paused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newManageFlow(t)
			if tc.paused {
				status, _, body := m.act(t, m.bob, core.ActPause)
				require.Equal(t, http.StatusOK, status, body)
			}
			require.True(t, m.row(t, m.carol).CanRenew || m.row(t, m.carol).CanResume, "offered while the goal has a standing plan")

			m.f.s.Service.Store = planlessStore{m.store}
			row := m.row(t, m.carol)
			assert.True(t, row.CanManage)
			assert.False(t, row.CanRenew)
			assert.False(t, row.CanResume)

			status, _, body := m.plan(t, m.carol, tc.action)
			assert.Equal(t, http.StatusBadRequest, status, body)
			assert.Contains(t, body, "no standing plan to approve")
		})
	}
}
