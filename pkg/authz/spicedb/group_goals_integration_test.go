//go:build integration

package spicedb

import (
	"context"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// groupGoalFixture is one group-owned goal domain and the people around it:
//
//	alice  member + manager           — the ordinary manager
//	bob    member                     — a plain member
//	carol  manager, NOT a member      — a manager who has left the group
//	dave   platform admin, no tie     — the "no manager left" fallback; must see to step in
//	eve    member + manager + denied  — denied wins over every grant
//	frank  nobody                     — outsider
type groupGoalFixture struct {
	domain, group, class, session string
	alice, bob, carol, dave, eve  identity.CanonicalUserID
	frank                         identity.CanonicalUserID
}

func rel(resType, resID, relation, subjType, subjID, subjRel string) authz.Relation {
	return authz.Relation{ResourceType: resType, ResourceID: resID, Relation: relation, SubjectType: subjType, SubjectID: subjID, SubjectRel: subjRel}
}

func newGroupGoalFixture(t *testing.T, c *Client) groupGoalFixture {
	t.Helper()
	f := groupGoalFixture{
		domain:  uniq(t, "ggdom-"),
		group:   uniq(t, "gggrp-"),
		class:   uniq(t, "ggcls-"),
		session: "goals/" + uniq(t, "ggrun-"),
		alice:   uniqCanon(t, "alice-"),
		bob:     uniqCanon(t, "bob-"),
		carol:   uniqCanon(t, "carol-"),
		dave:    uniqCanon(t, "dave-"),
		eve:     uniqCanon(t, "eve-"),
		frank:   uniqCanon(t, "frank-"),
	}
	const gd = "agent_goal_domain"
	rels := []authz.Relation{
		rel(gd, f.domain, "member_group", "group", f.group, "member"),
		rel(gd, f.domain, "class", "agentclass", f.class, ""),
		rel(gd, f.domain, "manager", "user", f.alice.String(), ""),
		rel(gd, f.domain, "manager", "user", f.carol.String(), ""),
		rel(gd, f.domain, "manager", "user", f.eve.String(), ""),
		rel(gd, f.domain, "denied", "user", f.eve.String(), ""),
		rel("group", f.group, "member", "user", f.alice.String(), ""),
		rel("group", f.group, "member", "user", f.bob.String(), ""),
		rel("group", f.group, "member", "user", f.eve.String(), ""),
		rel("agentclass", f.class, "platform", "platform", "platform", ""),
		rel("platform", "platform", "admin", "user", f.dave.String(), ""),
		// The goal-managed run session: managers own it, the group participates.
		rel("agentsession", f.session, "owner", gd, f.domain, "manage"),
		rel("agentsession", f.session, "participant", "group", f.group, "member"),
	}
	require.NoError(t, c.Relations().WriteRelationships(context.Background(), rels), "write fixture")
	return f
}

func TestGroupGoal_DomainPermissionsBySubject(t *testing.T) {
	c := newIntegrationClient(t)
	f := newGroupGoalFixture(t, c)
	ctx := context.Background()

	type want struct{ view, pause, manage, write bool }
	cases := []struct {
		name string
		who  identity.CanonicalUserID
		want want
	}{
		{"plain member: sees and pauses, cannot manage", f.bob, want{view: true, pause: true}},
		{"manager in the group: everything", f.alice, want{true, true, true, true}},
		{"manager who left the group: nothing", f.carol, want{}},
		{"platform admin with no membership: sees, pauses and manages", f.dave, want{true, true, true, true}},
		{"denied member-manager: nothing", f.eve, want{}},
		{"outsider: nothing", f.frank, want{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := want{}
			for perm, dst := range map[string]*bool{
				"view_memory": &got.view, "pause": &got.pause, "manage": &got.manage, "write_memory": &got.write,
			} {
				ok, err := c.CheckOnResource(ctx, "agent_goal_domain", f.domain, perm, tc.who, true)
				require.NoError(t, err, perm)
				*dst = ok
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// Management is the intersection with live membership: removing a manager
// from the group removes their manage, with no tuple on the domain touched.
func TestGroupGoal_ManagerRemovedFromGroupLosesManage(t *testing.T) {
	c := newIntegrationClient(t)
	f := newGroupGoalFixture(t, c)
	ctx := context.Background()

	ok, err := c.CheckOnResource(ctx, "agent_goal_domain", f.domain, "manage", f.alice, true)
	require.NoError(t, err)
	require.True(t, ok, "precondition: alice manages")

	require.NoError(t, c.Relations().DeleteRelationships(ctx, []authz.Relation{
		rel("group", f.group, "member", "user", f.alice.String(), ""),
	}))
	ok, err = c.CheckOnResource(ctx, "agent_goal_domain", f.domain, "manage", f.alice, true)
	require.NoError(t, err)
	assert.False(t, ok, "alice left the group: no longer manages")

	ok, err = c.CheckOnResource(ctx, "agentsession", f.session, "approve", f.alice, true)
	require.NoError(t, err)
	assert.False(t, ok, "and no longer approves in the run session")
}

// execute_group is satisfied only by the domain itself, through an unexpired
// domain_approved tuple.
func TestGroupGoal_ExecuteGroupNeedsUnexpiredDomainApproval(t *testing.T) {
	c := newIntegrationClient(t)
	f := newGroupGoalFixture(t, c)
	ctx := context.Background()

	checkExec := func(execID string) bool {
		t.Helper()
		resp, err := c.CheckPermission(ctx, &v1.CheckPermissionRequest{
			Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
			Resource:    &v1.ObjectReference{ObjectType: "agent_goal_execution", ObjectId: execID},
			Permission:  "execute_group",
			Subject:     &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "agent_goal_domain", ObjectId: f.domain}},
		})
		require.NoError(t, err)
		return resp.GetPermissionship() == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
	}
	approve := func(execID string, expires time.Time) {
		t.Helper()
		r := rel("agent_goal_execution", execID, "domain_approved", "agent_goal_domain", f.domain, "")
		r.ExpiresAt = expires
		require.NoError(t, c.Relations().WriteRelationships(ctx, []authz.Relation{r}))
	}

	live := uniq(t, "ggexec-live-")
	assert.False(t, checkExec(live), "no approval: refused")
	approve(live, time.Now().Add(time.Hour))
	assert.True(t, checkExec(live), "unexpired approval: allowed")

	stale := uniq(t, "ggexec-stale-")
	approve(stale, time.Now().Add(2*time.Second))
	require.True(t, checkExec(stale), "precondition: allowed before expiry")
	require.Eventually(t, func() bool { return !checkExec(stale) }, 15*time.Second, 250*time.Millisecond,
		"expired approval: refused")
}

// A run session owned by agent_goal_domain:X#manage hands approve to the
// managers and only to them; members talk in it as participants.
func TestGroupGoal_RunSessionApproveFollowsManage(t *testing.T) {
	c := newIntegrationClient(t)
	f := newGroupGoalFixture(t, c)
	ctx := context.Background()

	cases := []struct {
		name                  string
		who                   identity.CanonicalUserID
		wantApprove, wantTalk bool
	}{
		{"manager: approves and talks", f.alice, true, true},
		{"platform admin: approves (manage) and talks via owner", f.dave, true, true},
		{"plain member: talks, cannot approve", f.bob, false, true},
		{"manager outside the group: neither", f.carol, false, false},
		{"denied on the domain: talks as a member, cannot approve", f.eve, false, true},
		{"outsider: neither", f.frank, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := c.CheckOnResource(ctx, "agentsession", f.session, "approve", tc.who, true)
			require.NoError(t, err)
			assert.Equal(t, tc.wantApprove, ok, "approve")
			ok, err = c.CheckOnResource(ctx, "agentsession", f.session, "interact", tc.who, true)
			require.NoError(t, err)
			assert.Equal(t, tc.wantTalk, ok, "interact")
		})
	}

	got, err := c.LookupSubjects(ctx, "agentsession:"+f.session+"#approve")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{f.alice.String(), f.dave.String()}, got,
		"approver set is exactly the live managers plus platform admins")
}
