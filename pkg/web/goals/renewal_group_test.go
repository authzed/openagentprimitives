package goals

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	core "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionschedule"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

var renewalRun = core.RenewalHost{Session: groupNS + "/run-1", SessionUID: "run-uid-1"}

// dailyTerms asks for a daily series of ten runs starting within the hour.
func dailyTerms(x *core.ExecutionTerms) {
	due := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	x.DueAt, x.ExpiresAt = due, due.Add(10*24*time.Hour)
	x.Schedule = &sessionschedule.Spec{Kind: "daily", Timezone: "UTC", MaxRuns: 10, RunWindowSeconds: 3600}
}

// newApprovedSeries is a group consent flow whose daily series alice has
// approved.
func newApprovedSeries(t *testing.T) *groupConsentFlow {
	t.Helper()
	c := newGroupConsentFlowTerms(t, nil, dailyTerms)
	out, err := c.click(t, "alice@example.com")
	require.NoError(t, err)
	require.Equal(t, channelevents.OutcomeApproved, out.Result)
	c.goal = c.current(t)
	return c
}

func (c *groupConsentFlow) current(t *testing.T) core.Goal {
	t.Helper()
	g, err := c.store.Get(context.Background(), c.goal.Domain, c.goal.ID)
	require.NoError(t, err)
	return g
}

// renew requests the series' renewal from run-1 and publishes its card.
func (c *groupConsentFlow) renew(t *testing.T) core.Goal {
	t.Helper()
	ctx := context.Background()
	renewed, err := c.f.s.Service.RequestRenewal(ctx, c.goal.Domain, c.goal.ID, renewalRun, time.Now())
	require.NoError(t, err)
	require.NotNil(t, renewed.PendingRenewal)
	require.NoError(t, c.publisher.Notify(ctx, core.Event{Goal: renewed, Action: "request_renewal"}))
	return renewed
}

func (c *groupConsentFlow) grantFor(digest string) []authz.Relation {
	var out []authz.Relation
	for _, r := range c.f.auth.written {
		if r.ResourceType == ExecutionResourceType && r.ResourceID == digest {
			out = append(out, r)
		}
	}
	return out
}

func TestGroupRenewalCardIsPostedInTheRunSession(t *testing.T) {
	c := newApprovedSeries(t)
	renewed := c.renew(t)
	assert.Equal(t, channelevents.SessionRef{Namespace: groupNS, Name: "run-1"}, c.published.Session, "the renewal card is posted in the run that came due")
	assert.Equal(t, categories.GoalGroupExecutionConsent, c.card.Category, "decided exactly like a first consent")
	assert.Equal(t, "goalconsent-request-"+renewed.PendingRenewal.Digest, c.card.RequestRef)
	assert.Equal(t, []channelevents.InteractionResourceRef{{Type: GoalResourceType, ID: c.goal.ID, Permission: "manage"}}, c.card.Resources)
	assert.Contains(t, c.card.Lead, "Renew")
	require.NoError(t, c.card.Validate())
	require.NotNil(t, c.card.ExpiresAt)
	assert.WithinDuration(t, time.Now().Add(time.Duration(c.goal.Execution.Terms.Bounds.DurationSeconds)*time.Second), *c.card.ExpiresAt, 10*time.Second,
		"the card lasts as long as its run can, not the plan approval timeout")

	require.Len(t, c.notices, 1, "a plain notice is posted beside the card")
	assert.Equal(t, c.published.Session, c.notices[0].Session)
	var n channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(c.notices[0].Payload, &n))
	assert.Equal(t, categories.GoalApprovalExpiring, n.Category)
	assert.Contains(t, n.Lead, c.goal.Title+": approval expires "+c.goal.Execution.Terms.ExpiresAt.UTC().Format("2 Jan 2006"))
	assert.Contains(t, n.NextStep, "asking me to renew it")
	assert.NotContains(t, n.NextStep+n.Lead, "Slack")

	require.NoError(t, c.publisher.Notify(context.Background(), core.Event{Goal: renewed, Action: "request_renewal"}))
	assert.Len(t, c.notices, 1, "a replay finds the card retained and posts nothing again")
}

func TestGroupRenewalDecision(t *testing.T) {
	for _, tc := range []struct {
		name    string
		email   string
		setup   func(c *groupConsentFlow)
		approve bool
	}{
		{name: "plain member: refused, the current approval unchanged", email: "bob@example.com"},
		{name: "manager the start gate refuses: refused (checked at renewal)", email: "carol@example.com", setup: func(c *groupConsentFlow) {
			c.f.auth.startDenied = map[string]bool{c.carol: true}
		}},
		{name: "manager: the renewal becomes the approval, approved by the clicker", email: "carol@example.com", approve: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newApprovedSeries(t)
			before := c.goal
			renewed := c.renew(t)
			pending := *renewed.PendingRenewal
			if tc.setup != nil {
				tc.setup(c)
			}
			_, err := c.click(t, tc.email)
			g := c.current(t)
			if !tc.approve {
				require.Error(t, err)
				assert.Equal(t, before.Execution, g.Execution, "the approval in force is untouched")
				require.NotNil(t, g.PendingRenewal)
				assert.Nil(t, g.PendingRenewal.Decision, "no decision recorded")
				assert.Empty(t, c.grantFor(pending.Digest), "no grant written")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, g.Execution.Decision)
			assert.Equal(t, pending.Digest, g.Execution.Digest)
			assert.Equal(t, c.carol, g.Execution.Decision.Owner, "the clicking manager is the new approver")
			assert.Nil(t, g.PendingRenewal)
			require.NoError(t, c.f.s.VerifyDecision(context.Background(), g, *g.Execution.Decision))
			domainID := g.Domain.ID()
			assert.ElementsMatch(t, []authz.Relation{
				{ResourceType: ExecutionResourceType, ResourceID: pending.Digest, Relation: "domain", SubjectType: ResourceType, SubjectID: domainID},
				{ResourceType: ExecutionResourceType, ResourceID: pending.Digest, Relation: "domain_approved", SubjectType: ResourceType, SubjectID: domainID, ExpiresAt: pending.Terms.ExpiresAt},
			}, c.grantFor(pending.Digest), "the renewed grant carries the renewal's expiry")
		})
	}
}

func TestGroupResumeOffersTheLastApprovedTermsForApproval(t *testing.T) {
	c := newApprovedSeries(t)
	approved := *c.goal.LastApprovedTerms
	status, r, body := c.f.as(t, "r1", groupUpdate("pause", c.goal, "pause"))
	require.Equal(t, 200, status, body)
	require.Nil(t, r.Goal.Execution)
	status, r, body = c.f.as(t, "r1", groupUpdate("resume", *r.Goal, "resume"))
	require.Equal(t, 200, status, body)

	g := *r.Goal
	assert.Equal(t, core.Active, g.State, "the resume commits")
	require.NotNil(t, g.Execution, "and a re-approval is requested at once")
	assert.Nil(t, g.Execution.Decision)
	assert.Equal(t, groupNS+"/session", g.Execution.Session, "in the resuming manager's own session")
	want := approved
	want.DueAt, want.ExpiresAt, want.ScheduleWindows = g.Execution.Terms.DueAt, g.Execution.Terms.ExpiresAt, g.Execution.Terms.ScheduleWindows
	assert.Equal(t, want, g.Execution.Terms, "the last approved terms, prefilled")
	assert.True(t, g.Execution.Terms.DueAt.After(time.Now()), "with a fresh window")
	assert.Equal(t, approved.ExpiresAt.Sub(approved.DueAt), g.Execution.Terms.ExpiresAt.Sub(g.Execution.Terms.DueAt))

	c.goal = g
	require.NoError(t, c.publisher.Notify(context.Background(), core.Event{Goal: g, Action: "request_execution"}))
	assert.Equal(t, channelevents.SessionRef{Namespace: groupNS, Name: "session"}, c.published.Session)
	_, err := c.click(t, "carol@example.com")
	require.NoError(t, err, "decided like any consent")
	assert.Equal(t, c.carol, c.current(t).Execution.Decision.Owner)
}

func TestGroupApprovalExpiredIsShownOnRead(t *testing.T) {
	c := newApprovedSeries(t)
	expiry := c.goal.Execution.Terms.ExpiresAt
	c.f.s.Service.Now = func() time.Time { return expiry.Add(time.Second) }
	status, r, body := c.f.as(t, "r1", core.Request{Operation: "get", Scope: "group", ID: c.goal.ID})
	require.Equal(t, 200, status, body)
	assert.Equal(t, core.BlockedApprovalExpired, r.Goal.BlockedReason)
	status, r, body = c.f.as(t, "r1", core.Request{Operation: "list", Scope: "group"})
	require.Equal(t, 200, status, body)
	require.Len(t, r.Page.Goals, 1)
	assert.Equal(t, core.BlockedApprovalExpired, r.Page.Goals[0].BlockedReason)
}

// R18c: after the approval runs out, a manager re-requests it without
// restating the terms.
func TestGroupRequestExecutionWithoutTermsReusesTheLastApproved(t *testing.T) {
	c := newApprovedSeries(t)
	approved := *c.goal.LastApprovedTerms
	now := approved.ExpiresAt.Add(time.Hour)
	c.f.s.Service.Now = func() time.Time { return now }
	g := c.current(t)
	status, r, body := c.f.as(t, "r1", core.Request{Operation: "request_execution", Scope: "group", Resource: groupResource(),
		Execution: core.ExecutionRequest{RequestID: "again", ID: g.ID, Revision: g.Revision}})
	require.Equal(t, 200, status, body)
	e := r.Goal.Execution
	require.NotNil(t, e)
	assert.Nil(t, e.Decision, "a fresh request for the managers to decide")
	want := approved
	want.DueAt, want.ExpiresAt, want.ScheduleWindows = e.Terms.DueAt, e.Terms.ExpiresAt, e.Terms.ScheduleWindows
	assert.Equal(t, want, e.Terms, "the last approved terms")
	assert.True(t, e.Terms.DueAt.After(now), "with a fresh window")
	assert.Equal(t, approved.ExpiresAt.Sub(approved.DueAt), e.Terms.ExpiresAt.Sub(e.Terms.DueAt))
}

func TestGroupRequestExecutionWithoutTermsNeedsAPriorApproval(t *testing.T) {
	c := newGroupConsentFlowTerms(t, nil, dailyTerms) // requested, never approved
	g := c.current(t)
	status, _, body := c.f.as(t, "r1", core.Request{Operation: "request_execution", Scope: "group", Resource: groupResource(),
		Execution: core.ExecutionRequest{RequestID: "again", ID: g.ID, Revision: g.Revision}})
	assert.NotEqual(t, 200, status)
	assert.Contains(t, body, "supply terms")
}

// With an approval in force, a request naming no terms renews it in
// place — in the asking manager's conversation — instead of replacing it.
func TestGroupRequestExecutionWithoutTermsRenewsAnApprovalInForce(t *testing.T) {
	c := newApprovedSeries(t)
	before := c.goal
	status, r, body := c.f.as(t, "r1", core.Request{Operation: "request_execution", Scope: "group", Resource: groupResource(),
		Execution: core.ExecutionRequest{RequestID: "renew-please", ID: before.ID, Revision: before.Revision}})
	require.Equal(t, 200, status, body)
	g := *r.Goal
	assert.Equal(t, before.Revision, g.Revision, "nothing is replaced")
	assert.Equal(t, before.Execution, g.Execution, "the approval in force is untouched")
	p := g.PendingRenewal
	require.NotNil(t, p)
	assert.Equal(t, groupNS+"/session", p.Session, "hosted in the asking manager's own conversation")
	assert.Equal(t, before.Execution.Digest, p.Renews)
	assert.False(t, p.SystemRenewal())

	require.NoError(t, c.publisher.Notify(context.Background(), core.Event{Goal: g, Action: "request_renewal"}))
	assert.Equal(t, channelevents.SessionRef{Namespace: groupNS, Name: "session"}, c.published.Session)
	assert.Equal(t, "goalconsent-request-"+p.Digest, c.card.RequestRef)
	assert.Empty(t, c.notices, "no 'ask me' notice beside a card the manager asked for")
	require.NotNil(t, c.card.ExpiresAt)
	assert.True(t, c.card.ExpiresAt.Equal(before.Execution.Terms.ExpiresAt), "it lasts until the approval it renews runs out")

	_, err := c.click(t, "carol@example.com")
	require.NoError(t, err, "decided like any consent")
	after := c.current(t)
	assert.Equal(t, before.Revision, after.Revision, "the approval swaps in place")
	assert.Equal(t, p.Digest, after.Execution.Digest)
	assert.Equal(t, c.carol, after.Execution.Decision.Owner)
	assert.Equal(t, before.Execution.Digest, after.PriorExecution.Digest, "the renewed approval keeps its runs")
}
