package goals_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	goalinmem "github.com/authzed/openagentprimitives/pkg/memory/goals/inmem"
)

// groupAuth records which kind of check each Service call asked for. Writes
// and reads go through Authorize; the group-only accesses (member, pause) and
// manager validation go through the GroupAuthorizer half.
type groupAuth struct {
	calls       []string
	denyManager error
	sourcesErr  error
}

func (g *groupAuth) Authorize(_ context.Context, _ goals.Actor, write bool) error {
	if write {
		g.calls = append(g.calls, "manage")
	} else {
		g.calls = append(g.calls, "view_memory")
	}
	return nil
}
func (g *groupAuth) Sources(context.Context, goals.Actor) ([]goals.Source, error) {
	return nil, g.sourcesErr
}
func (g *groupAuth) AuthorizeGoalManager(_ context.Context, a goals.Actor, goal goals.Goal) error {
	g.calls = append(g.calls, "listed-manager")
	if !slices.Contains(goal.Managers, a.User) {
		return goals.ErrDenied
	}
	return nil
}
func (g *groupAuth) ReadGoal(context.Context, goals.Actor, goals.Goal) error { return nil }
func (g *groupAuth) AuthorizeGroup(_ context.Context, _ goals.Actor, access goals.GroupAccess) error {
	g.calls = append(g.calls, string(access))
	return nil
}
func (g *groupAuth) CheckManagers(context.Context, goals.Actor, goals.Goal, goals.Change) error {
	g.calls = append(g.calls, "managers")
	return g.denyManager
}

func groupActor(user string) goals.Actor {
	return goals.Actor{Session: "ns/s", Proof: "p", User: user, Domain: goals.Domain{Namespace: "ns", Owner: "group:g1#member", Class: "c", ClassUID: "u"}}
}

func groupService(auth *groupAuth) *goals.Service {
	return &goals.Service{Store: goalinmem.New(), Auth: auth, GroupLinks: func() []string { return []string{"group#member"} }}
}

func createGroupGoal(t *testing.T, s *goals.Service, a goals.Actor) goals.Goal {
	t.Helper()
	g, err := s.Create(t.Context(), a, goals.CreateRequest{RequestID: "create", Title: "Standup", Outcome: "Summarize"})
	require.NoError(t, err, "group create")
	return g
}

func TestGroupCreateNeedsMembershipAndMakesTheCreatorManager(t *testing.T) {
	auth := &groupAuth{}
	s := groupService(auth)
	g := createGroupGoal(t, s, groupActor("alice"))
	assert.Equal(t, []string{"alice"}, g.Managers)
	assert.Equal(t, goals.Draft, g.State)
	assert.Equal(t, []string{string(goals.GroupMember)}, auth.calls, "a group create asks for membership, not manage: the creator is not a manager yet")
}

func TestGroupCreateWithoutActingMemberIsDenied(t *testing.T) {
	_, err := groupService(&groupAuth{}).Create(t.Context(), groupActor(""), goals.CreateRequest{RequestID: "create", Title: "Standup", Outcome: "Summarize"})
	require.ErrorIs(t, err, goals.ErrDenied)
}

func TestGroupUpdateAccess(t *testing.T) {
	cases := []struct {
		name   string
		action string
		want   []string
	}{
		{name: "pause: any member's pause permission, no manager-list check", action: "pause", want: []string{string(goals.GroupPause)}},
		{name: "resume: manage and listed as a manager", action: "resume", want: []string{"manage", "listed-manager"}},
		{name: "cancel: manage without source checks, listed as a manager", action: "cancel", want: []string{string(goals.GroupRelinquish), "listed-manager"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := &groupAuth{}
			s := groupService(auth)
			a := groupActor("alice")
			g := createGroupGoal(t, s, a)
			g, err := s.Update(t.Context(), a, goals.Change{RequestID: "activate", ID: g.ID, Revision: g.Revision, Action: "activate"})
			require.NoError(t, err)
			if tc.action == "resume" {
				g, err = s.Update(t.Context(), a, goals.Change{RequestID: "pause", ID: g.ID, Revision: g.Revision, Action: "pause"})
				require.NoError(t, err)
			}
			auth.calls = nil
			_, err = s.Update(t.Context(), a, goals.Change{RequestID: tc.action, ID: g.ID, Revision: g.Revision, Action: tc.action})
			require.NoError(t, err)
			assert.Equal(t, tc.want, auth.calls)
		})
	}
}

func TestPrivatePauseStillChecksManage(t *testing.T) {
	auth := &groupAuth{}
	s := &goals.Service{Store: goalinmem.New(), Auth: auth}
	a := goals.Actor{Session: "ns/s", Proof: "p", Domain: goals.Domain{Namespace: "ns", Owner: "alice", Class: "c", ClassUID: "u"}}
	g, err := s.Create(t.Context(), a, goals.CreateRequest{RequestID: "create", Title: "Standup", Outcome: "Summarize"})
	require.NoError(t, err)
	assert.Empty(t, g.Managers, "a private goal has no managers")
	g, err = s.Update(t.Context(), a, goals.Change{RequestID: "activate", ID: g.ID, Revision: g.Revision, Action: "activate"})
	require.NoError(t, err)
	auth.calls = nil
	_, err = s.Update(t.Context(), a, goals.Change{RequestID: "pause", ID: g.ID, Revision: g.Revision, Action: "pause"})
	require.NoError(t, err)
	assert.Equal(t, []string{"manage"}, auth.calls)
}

func TestGroupManagerChanges(t *testing.T) {
	auth := &groupAuth{}
	s := groupService(auth)
	a := groupActor("alice")
	g := createGroupGoal(t, s, a)
	bob := "bob"

	next, err := s.Update(t.Context(), a, goals.Change{RequestID: "add", ID: g.ID, Revision: g.Revision, Action: "add_manager", Manager: &bob})
	require.NoError(t, err)
	assert.Equal(t, []string{"alice", "bob"}, next.Managers)
	assert.Contains(t, auth.calls, "managers", "the server validates every manager change")

	auth.denyManager = goals.ErrDenied
	_, err = s.Update(t.Context(), a, goals.Change{RequestID: "remove", ID: g.ID, Revision: next.Revision, Action: "remove_manager", Manager: &bob})
	require.ErrorIs(t, err, goals.ErrDenied, "a refused manager check refuses the change")
}

func TestReviseManagers(t *testing.T) {
	now := time.Now().UTC()
	group := goals.Domain{Namespace: "ns", Owner: "group:g1#member", Class: "c", ClassUID: "u"}
	base := goals.Goal{ID: "goal-1", Domain: group, Title: "T", Outcome: "O", State: goals.Active, Revision: 3, Managers: []string{"alice"}, Execution: &goals.ExecutionConsent{RequestID: "r", Digest: "d"}}
	str := func(s string) *string { return &s }
	cases := []struct {
		name    string
		goal    func() goals.Goal
		change  goals.Change
		want    []string
		wantErr string
	}{
		{name: "add: appended, execution consent kept", goal: func() goals.Goal { return base }, change: goals.Change{Revision: 3, Action: "add_manager", Manager: str("bob")}, want: []string{"alice", "bob"}},
		{name: "add an existing manager: ErrInvalid", goal: func() goals.Goal { return base }, change: goals.Change{Revision: 3, Action: "add_manager", Manager: str("alice")}, wantErr: "already a manager"},
		{name: "add with no manager named: ErrInvalid", goal: func() goals.Goal { return base }, change: goals.Change{Revision: 3, Action: "add_manager"}, wantErr: "names no manager"},
		{name: "remove: dropped, execution consent kept", goal: func() goals.Goal { g := base; g.Managers = []string{"alice", "bob"}; return g }, change: goals.Change{Revision: 3, Action: "remove_manager", Manager: str("alice")}, want: []string{"bob"}},
		{name: "remove the last manager: refused", goal: func() goals.Goal { return base }, change: goals.Change{Revision: 3, Action: "remove_manager", Manager: str("alice")}, wantErr: "a group goal needs a manager in the group"},
		{name: "remove a non-manager: ErrInvalid", goal: func() goals.Goal { return base }, change: goals.Change{Revision: 3, Action: "remove_manager", Manager: str("carol")}, wantErr: "not a manager"},
		{name: "manager change on a private goal: ErrInvalid", goal: func() goals.Goal {
			g := base
			g.Domain.Owner = "alice"
			g.Managers = nil
			return g
		}, change: goals.Change{Revision: 3, Action: "add_manager", Manager: str("bob")}, wantErr: "group goals"},
		{name: "manager with edits: ErrInvalid", goal: func() goals.Goal { return base }, change: goals.Change{Revision: 3, Action: "add_manager", Manager: str("bob"), Title: str("x")}, wantErr: "edits require revise"},
		{name: "manager named on a non-manager action: ErrInvalid", goal: func() goals.Goal { return base }, change: goals.Change{Revision: 3, Action: "pause", Manager: str("bob")}, wantErr: "manager requires"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := tc.goal()
			next, err := goals.Revise(g, tc.change, now)
			if tc.wantErr != "" {
				require.ErrorIs(t, err, goals.ErrInvalid)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, next.Managers)
			assert.Equal(t, g.Execution, next.Execution, "manager changes do not clear execution consent")
			assert.Equal(t, g.Revision, next.Revision, "nor the revision the consent and its runs are bound to")
		})
	}
}

func TestGroupWriteNeedsTheGoalsOwnManagerList(t *testing.T) {
	auth := &groupAuth{}
	s := groupService(auth)
	g := createGroupGoal(t, s, groupActor("alice"))
	// The domain's manage check (Authorize) passes for bob, as a stale
	// manager tuple would make it; the goal's own list still refuses him.
	_, err := s.Update(t.Context(), groupActor("bob"), goals.Change{RequestID: "activate", ID: g.ID, Revision: g.Revision, Action: "activate"})
	require.ErrorIs(t, err, goals.ErrDenied)
}

func TestGroupRelinquishingChangesSkipSources(t *testing.T) {
	cases := []struct {
		action  string
		wantErr bool
	}{
		{action: "pause"},
		{action: "cancel"},
		{action: "remove_manager"},
		{action: "revise", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			auth := &groupAuth{}
			s := groupService(auth)
			a := groupActor("alice")
			g := createGroupGoal(t, s, a)
			bob := "bob"
			g, err := s.Update(t.Context(), a, goals.Change{RequestID: "add", ID: g.ID, Revision: g.Revision, Action: "add_manager", Manager: &bob})
			require.NoError(t, err)
			g, err = s.Update(t.Context(), a, goals.Change{RequestID: "activate", ID: g.ID, Revision: g.Revision, Action: "activate"})
			require.NoError(t, err)
			auth.sourcesErr = errors.New("session source unreadable by the audience")
			c := goals.Change{RequestID: tc.action, ID: g.ID, Revision: g.Revision, Action: tc.action}
			switch tc.action {
			case "remove_manager":
				c.Manager = &bob
			case "revise":
				title := "x"
				c.Title = &title
			}
			_, err = s.Update(t.Context(), a, c)
			if tc.wantErr {
				require.ErrorIs(t, err, auth.sourcesErr, "a change that adds to the goal still checks the session's sources")
				return
			}
			require.NoError(t, err, "a change that only removes authority is never blocked by sources")
		})
	}
}
