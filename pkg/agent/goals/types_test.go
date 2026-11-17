package goals

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLifecycleDoesNotAuthorizeExecution(t *testing.T) {
	now := time.Now().UTC()
	g := Goal{ID: "goal-1", Domain: Domain{Namespace: "ns", Owner: "alice", Class: "assistant", ClassUID: "uid"}, Title: "Agenda", Outcome: "Prepare meeting", State: Draft, Revision: 1}
	for _, action := range []string{"activate", "pause", "resume", "revise", "complete"} {
		change := Change{Revision: g.Revision, Action: action}
		if action == "revise" {
			title := "Updated agenda"
			change.Title = &title
		}
		if action == "complete" {
			change.Result = &Result{Summary: "Prepared", Evidence: []string{"artifact:agenda"}}
		}
		next, err := Revise(g, change, now)
		require.NoError(t, err)
		assert.Equal(t, g.Revision+1, next.Revision)
		g = next
	}
	assert.Equal(t, Completed, g.State)
	_, err := Revise(g, Change{Revision: g.Revision, Action: "resume"}, now)
	require.ErrorIs(t, err, ErrInvalid)
}
func TestInvalidLifecycleDoesNotMutateGoal(t *testing.T) {
	g := Goal{Domain: Domain{Namespace: "ns", Owner: "alice", Class: "assistant", ClassUID: "uid"}, Title: "Agenda", Outcome: "Prepare", State: Draft, Revision: 1}
	for _, c := range []Change{{Revision: 1, Action: "pause"}, {Revision: 1, Action: "resume"}, {Revision: 1, Action: "complete"}, {Revision: 1, Action: "revise", ClearDue: true, DueAt: new(time.Time)}, {Revision: 1, Action: "activate", Outcome: new(string)}} {
		_, err := Revise(g, c, time.Now())
		require.ErrorIs(t, err, ErrInvalid)
		assert.Equal(t, Draft, g.State)
		assert.Equal(t, int64(1), g.Revision)
	}
}

func TestValidateDomainGroupOwner(t *testing.T) {
	links := func() []string { return []string{"group#member"} }
	cases := []struct {
		name       string
		owner      string
		links      func() []string
		groupOwned bool
		wantErr    bool
	}{
		{name: "user owner: not group-owned, valid", owner: "user-1", links: links},
		{name: "registered group link: group-owned, valid", owner: "group:g1#member", links: links, groupOwned: true},
		{name: "group owner with nil GroupLinks: ErrInvalid", owner: "group:g1#member", groupOwned: true, wantErr: true},
		{name: "unregistered link type: ErrInvalid", owner: "slack_channel:C#member", links: links, groupOwned: true, wantErr: true},
		{name: "unregistered relation: ErrInvalid", owner: "group:g1#admin", links: links, groupOwned: true, wantErr: true},
		{name: "missing id: ErrInvalid", owner: "group:#member", links: links, groupOwned: true, wantErr: true},
		{name: "two hashes: ErrInvalid", owner: "group:g1#member#x", links: links, groupOwned: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Domain{Namespace: "ns", Owner: tc.owner, Class: "c", ClassUID: "uid"}
			assert.Equal(t, tc.groupOwned, d.GroupOwned())
			err := (&Service{GroupLinks: tc.links}).validateDomain(d)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrInvalid)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestOwnerSubjectSet(t *testing.T) {
	typ, id, rel, ok := Domain{Owner: "group:g1#member"}.OwnerSubjectSet()
	require.True(t, ok)
	assert.Equal(t, []string{"group", "g1", "member"}, []string{typ, id, rel})
	_, _, _, ok = Domain{Owner: "user-1"}.OwnerSubjectSet()
	assert.False(t, ok)
}

// Guards the user-owned hash: group support must not change existing goal ids.
func TestDomainIDStableForUserOwner(t *testing.T) {
	d := Domain{Namespace: "ns", Owner: "user-1", Class: "c", ClassUID: "uid"}
	assert.Equal(t, "ea1a1d97f7e6e22c2ced59c82048aac9ce1b7e363e362a83a0a181f25a73008b", d.ID())
}

type allowAuth struct{}

func (allowAuth) Authorize(context.Context, Actor, bool) error     { return nil }
func (allowAuth) Sources(context.Context, Actor) ([]Source, error) { return nil, nil }
func (allowAuth) ReadGoal(context.Context, Actor, Goal) error      { return nil }

func TestAuthorizeRejectsUnregisteredGroupOwner(t *testing.T) {
	a := Actor{Session: "s", Proof: "p", Domain: Domain{Namespace: "ns", Owner: "group:g#member", Class: "c", ClassUID: "u"}}
	// The Authorizer allows everything, so only validateDomain can deny.
	require.ErrorIs(t, (&Service{Auth: allowAuth{}}).authorize(t.Context(), a, true), ErrDenied)
	require.NoError(t, (&Service{Auth: allowAuth{}, GroupLinks: func() []string { return []string{"group#member"} }}).authorize(t.Context(), a, true))
}
