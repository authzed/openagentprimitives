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
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/memory"
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

func (m *manageFlow) act(t *testing.T, viewer, action string) (int, core.ManageResponse, string) {
	t.Helper()
	return m.manage(t, "channelsd", core.ManageRequest{Operation: core.ManageAct, Viewer: viewer, Namespace: groupNS, Goal: GoalResourceType + ":" + m.goal.ID, Action: action})
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
	assert.Equal(t, firstPartyAttestation{Publisher: "system:channelsd", Surface: "management", Viewer: m.bob, Goal: m.goal.ID, Action: core.ActPause, At: att.At}, att)
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
