package goals_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
)

// firstParty is the session-less management actor the goal server builds
// after verifying channelsd's token: no session, a viewer, an attestation.
func firstParty(user string) goals.Actor {
	a := groupActor(user)
	a.Session, a.FirstParty, a.Proof, a.Attestation = "", true, "first-party", `{"viewer":"`+user+`"}`
	return a
}

// TestSessionlessActorIsAdmittedOnlyAsFirstParty: Session == "" is the
// management surface's actor, and only with the first-party attestation the
// goal server sets; it never stands in for a session-scoped actor.
func TestSessionlessActorIsAdmittedOnlyAsFirstParty(t *testing.T) {
	auth := &groupAuth{}
	s := groupService(auth)
	g := createGroupGoal(t, s, groupActor("alice"))

	cases := []struct {
		name  string
		actor func() goals.Actor
		ok    bool
	}{
		{name: "session-less without the first-party attestation: denied", actor: func() goals.Actor {
			a := groupActor("alice")
			a.Session = ""
			return a
		}},
		{name: "first-party that also names a session: denied", actor: func() goals.Actor {
			a := firstParty("alice")
			a.Session = "ns/s"
			return a
		}},
		{name: "first-party with no viewer: denied", actor: func() goals.Actor { return firstParty("") }},
		{name: "first-party on a private domain: denied", actor: func() goals.Actor {
			a := firstParty("alice")
			a.Domain.Owner = "alice"
			return a
		}},
		{name: "first-party on a group domain with a viewer: admitted", actor: func() goals.Actor { return firstParty("alice") }, ok: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Get(t.Context(), tc.actor(), g.ID)
			if tc.ok {
				assert.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, goals.ErrDenied)
		})
	}
}

// TestFirstPartyAuditKeepsTheAttestation: a first-party write is audited with
// the attestation as its actor proof, since there is no session to name.
func TestFirstPartyAuditKeepsTheAttestation(t *testing.T) {
	auth := &groupAuth{}
	s := groupService(auth)
	g := createGroupGoal(t, s, groupActor("alice"))
	g, err := s.Update(t.Context(), groupActor("alice"), goals.Change{RequestID: "activate", ID: g.ID, Revision: g.Revision, Action: "activate"})
	require.NoError(t, err)

	a := firstParty("alice")
	paused, err := s.Update(t.Context(), a, goals.Change{RequestID: "pause", ID: g.ID, Revision: g.Revision, Action: "pause"})
	require.NoError(t, err)
	assert.Equal(t, goals.Paused, paused.State)
	events, err := s.Store.Pending(t.Context(), 100)
	require.NoError(t, err)
	last := events[len(events)-1]
	assert.Equal(t, "pause", last.Action)
	assert.Empty(t, last.Session, "no session acted")
	assert.JSONEq(t, a.Attestation, string(last.ActorProof), "the first-party attestation is the authorship evidence")
}

// TestManagementHostIsNeverASession: a consent requested from a management
// surface is hosted at a name no AgentSession can have, and is recognised as
// such.
func TestManagementHostIsNeverASession(t *testing.T) {
	g := goals.Goal{ID: "goal-abc", Domain: goals.Domain{Namespace: "ns", Owner: "group:g1#member", Class: "c", ClassUID: "u"}}
	h := goals.ManagementHost(g)
	assert.Equal(t, "ns/_goal-manage.goal-abc", h.Session)
	assert.NotEmpty(t, h.SessionUID)
	assert.True(t, (&goals.ExecutionConsent{Session: h.Session}).ManagementHosted())
	assert.False(t, (&goals.ExecutionConsent{Session: "ns/session"}).ManagementHosted())
	assert.False(t, (*goals.ExecutionConsent)(nil).ManagementHosted())
}
