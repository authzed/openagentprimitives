//go:build integration

package spicedb

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	guardianschema "github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// sessionSubjectFragment is written the way a toolkit author would write it —
// with permissions of their own — and is then run through the REAL composer
// (guardianschema.ComposeSlots) before it reaches SpiceDB. Hand-writing the
// composed line here would test a copy of the expression, not the one a
// cluster runs.
//
// demo_doc declares owner, so its composed line carries the owner leg.
// demo_pool is a memory pool: write_memory is slottable, view_memory is its
// AUDIENCE and must never be composed.
const sessionSubjectFragment = `
definition demo_doc {
    relation owner: user
    permission read = owner
}

definition demo_pool {
    relation owner: user
    permission view_memory = owner
    permission write_memory = owner
}
`

func newSessionSubjectIntegrationClient(t *testing.T) *Client {
	t.Helper()
	composed, changed, skipped, err := guardianschema.ComposeSlots(
		testspicedb.Schema+"\n"+sessionSubjectFragment,
		[]guardianschema.SlotPair{
			{ResourceType: "demo_doc", Permission: "read"},
			{ResourceType: "demo_pool", Permission: "write_memory"},
		})
	require.NoError(t, err, "ComposeSlots")
	require.True(t, changed, "the fixture must actually be composed")
	require.Empty(t, skipped, "no slot may be skipped")

	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchemaText(t, endpoint, token, composed)
	c, err := NewClient(endpoint, token, true)
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestIntegration_SlotGrant_NamedSessionHoldsThePermission locks who a slot
// grant authorizes now that the composed line carries a direct leg:
//
//	permission <p> = slot_grant_<p> + slot_grant_<p>->interact [+ owner]
//
// The direct leg is what lets a session acting as ITSELF (a group goal run, a
// userless session — agentsession:<ns>/<name>) use a grant written for it;
// agentsession#interact never contains the session, so before the leg every
// such grant authorized nobody. Every other row pins a property the leg must
// NOT disturb: expiry still bounds it, another session gets nothing, a
// delegated child gets nothing (slot_grant_<p> admits bare agentsession
// subjects only, so nothing flows across `parent`), the human legs and
// `denied` behave exactly as before, and a memory pool's audience is
// untouched.
func TestIntegration_SlotGrant_NamedSessionHoldsThePermission(t *testing.T) {
	c := newSessionSubjectIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "integration-test"
	sName := uniq(t, "slot-self-")
	tName := uniq(t, "slot-other-")
	childName := uniq(t, "slot-child-")
	docLive := uniq(t, "doc-live-")
	docExpired := uniq(t, "doc-expired-")
	docOwned := uniq(t, "doc-owned-")
	pool := uniq(t, "pool-")

	owner := uniqCanon(t, "s-owner-")
	participant := uniqCanon(t, "s-participant-")
	blocked := uniqCanon(t, "s-blocked-")
	outsider := uniqCanon(t, "outsider-")
	docOwner := uniqCanon(t, "doc-owner-")

	t.Cleanup(func() {
		bg := context.Background()
		for _, n := range []string{sName, tName, childName} {
			_ = c.DeleteAgentSessionRelationships(bg, ns, n)
			_ = c.DeleteSlotGrants(bg, ns, n)
		}
	})

	// S: an owner, a participant, and a denied participant.
	require.NoError(t, c.TouchOwner(ctx, ns, sName, owner.Subject().String()), "TouchOwner")
	require.NoError(t, c.TouchInteractParticipantUser(ctx, ns, sName, participant), "participant")
	require.NoError(t, c.TouchInteractParticipantUser(ctx, ns, sName, blocked), "blocked participant")
	require.NoError(t, c.TouchDeniedUser(ctx, ns, sName, blocked), "TouchDeniedUser")
	// A child delegated from S. Its row asks about the CHILD SESSION as subject:
	// lineage must not carry S's grant down to it.
	require.NoError(t, c.TouchLineage(ctx, ns, childName, ns, sName), "TouchLineage")

	s := authz.SessionRef{Namespace: ns, Name: sName}
	multi := func(typ, id, perm string) authz.SlotBinding {
		return authz.SlotBinding{ResourceType: typ, ResourceID: authz.TrustedObjectID(id), Permission: perm, Occupancy: authz.SlotOccupancyMulti}
	}
	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), s, []authz.SlotBinding{
		multi("demo_doc", docLive, "read"),
		multi("demo_pool", pool, "write_memory"),
	}, time.Now().Add(time.Hour)), "GrantSlots (live)")
	// Positive control for the expired row: the SAME tuple, live, does
	// authorize S. Then it is re-touched with a lapsed expiry, so the expired
	// row's denial is about the expiry and not a tuple that never landed.
	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), s, []authz.SlotBinding{
		multi("demo_doc", docExpired, "read"),
	}, time.Now().Add(time.Hour)), "GrantSlots (expiring doc, live first)")
	live, err := c.CheckSubjectOnResource(ctx, "demo_doc", docExpired, "read", "agentsession", ns+"/"+sName, true)
	require.NoError(t, err, "CheckSubjectOnResource (positive control)")
	require.True(t, live, "positive control: a live grant on the expiring doc must authorize S")
	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), s, []authz.SlotBinding{
		multi("demo_doc", docExpired, "read"),
	}, time.Now().Add(-time.Minute)), "GrantSlots (re-touched, already expired)")
	// Direct standing on docOwned and on the pool, no grant on docOwned.
	require.NoError(t, c.Relations().WriteRelationships(ctx, []authz.Relation{
		{ResourceType: "demo_doc", ResourceID: docOwned, Relation: "owner", SubjectType: "user", SubjectID: docOwner.String()},
		{ResourceType: "demo_pool", ResourceID: pool, Relation: "owner", SubjectType: "user", SubjectID: docOwner.String()},
	}), "owner tuples")

	session := func(name string) (string, string) { return "agentsession", ns + "/" + name }
	user := func(id identity.CanonicalUserID) (string, string) { return "user", id.String() }

	cases := []struct {
		name    string
		subject func() (string, string)
		resType string
		resID   string
		perm    string
		want    bool
		because string
	}{
		{"S holds read via its own grant", func() (string, string) { return session(sName) }, "demo_doc", docLive, "read", true,
			"the session a grant names must hold the permission itself"},
		{"S with an expired grant: denied", func() (string, string) { return session(sName) }, "demo_doc", docExpired, "read", false,
			"expiry still bounds the direct leg"},
		{"T (another session, no grant): denied", func() (string, string) { return session(tName) }, "demo_doc", docLive, "read", false,
			"a grant names exactly one session"},
		{"child delegated from S: denied", func() (string, string) { return session(childName) }, "demo_doc", docLive, "read", false,
			"the direct leg must not flow down the delegation tree"},
		{"S's owner: allowed as before", func() (string, string) { return user(owner) }, "demo_doc", docLive, "read", true,
			"the member leg is unchanged"},
		{"S's participant: allowed as before", func() (string, string) { return user(participant) }, "demo_doc", docLive, "read", true,
			"the member leg is unchanged"},
		{"S's denied participant: denied as before", func() (string, string) { return user(blocked) }, "demo_doc", docLive, "read", false,
			"denied is still honoured on the member leg"},
		{"non-member human: denied", func() (string, string) { return user(outsider) }, "demo_doc", docLive, "read", false,
			"no wildcard anywhere"},
		{"resource owner, no grant: allowed via owner leg", func() (string, string) { return user(docOwner) }, "demo_doc", docOwned, "read", true,
			"the owner leg is unchanged"},
		{"S on a doc it holds no grant for: denied", func() (string, string) { return session(sName) }, "demo_doc", docOwned, "read", false,
			"the direct leg is per instance"},
		{"S holds write_memory via its own grant", func() (string, string) { return session(sName) }, "demo_pool", pool, "write_memory", true,
			"a write_* memory slot gets the same direct leg"},
		{"S does not hold view_memory: audience untouched", func() (string, string) { return session(sName) }, "demo_pool", pool, "view_memory", false,
			"a memory audience permission is never composed, so a grant cannot widen it"},
		{"pool owner holds view_memory: audience untouched", func() (string, string) { return user(docOwner) }, "demo_pool", pool, "view_memory", true,
			"the author's audience line survives composition"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, sid := tc.subject()
			got, err := c.CheckSubjectOnResource(ctx, tc.resType, tc.resID, tc.perm, st, sid, true)
			require.NoError(t, err, "CheckSubjectOnResource")
			assert.Equal(t, tc.want, got, tc.because)
		})
	}
}

// TestIntegration_SlotGrant_GroupRunMembersReadTheBoundResource pins how far
// a standing-bound grant reaches. A group goal's run keeps participant = the group's member set, so
// while a standing-bound slot grant on the run lives, every member of the
// group holds the composed permission on the bound resource through the
// grant's interact leg — which is what the info-leakage audience check reads
// when it asks whether a post into the group would reach anyone who cannot
// see the resource. That is accepted: a manager approved the standing plan,
// and with it posting about those resources into this group. The rows pin
// that the reach is exactly the grant's: it ends when the grant expires or
// is removed, and never extends past the group.
func TestIntegration_SlotGrant_GroupRunMembersReadTheBoundResource(t *testing.T) {
	c := newSessionSubjectIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "integration-test"
	run := uniq(t, "group-run-")
	group := uniq(t, "run-group-")
	docLive := uniq(t, "doc-live-")
	docExpired := uniq(t, "doc-expired-")
	member := uniqCanon(t, "member-")
	outsider := uniqCanon(t, "outsider-")

	t.Cleanup(func() {
		bg := context.Background()
		_ = c.DeleteAgentSessionRelationships(bg, ns, run)
		_ = c.DeleteSlotGrants(bg, ns, run)
	})

	require.NoError(t, c.Relations().WriteRelationships(ctx, []authz.Relation{
		{ResourceType: "group", ResourceID: group, Relation: "member", SubjectType: "user", SubjectID: member.String()},
	}), "group membership")
	require.NoError(t, c.TouchInteractParticipant(ctx, ns, run, "group:"+group+"#member"), "run participant = member group")

	s := authz.SessionRef{Namespace: ns, Name: run}
	bind := func(doc string) []authz.SlotBinding {
		return []authz.SlotBinding{{ResourceType: "demo_doc", ResourceID: authz.TrustedObjectID(doc), Permission: "read", Occupancy: authz.SlotOccupancyMulti}}
	}
	reads := func(id identity.CanonicalUserID, doc string) bool {
		t.Helper()
		ok, err := c.CheckSubjectOnResource(ctx, "demo_doc", doc, "read", "user", id.String(), true)
		require.NoError(t, err, "CheckSubjectOnResource")
		return ok
	}

	assert.False(t, reads(member, docLive), "before any grant, a member holds nothing on the resource")

	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), s, bind(docLive), time.Now().Add(time.Hour)), "GrantSlots (live)")
	assert.True(t, reads(member, docLive), "while the grant lives, a group member reads the bound resource")
	assert.False(t, reads(outsider, docLive), "someone outside the group does not")

	// Expiry: live first (positive control), then re-touched already expired.
	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), s, bind(docExpired), time.Now().Add(time.Hour)), "GrantSlots (expiring, live first)")
	require.True(t, reads(member, docExpired), "positive control: the live grant reaches the member")
	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), s, bind(docExpired), time.Now().Add(-time.Minute)), "GrantSlots (re-touched, expired)")
	assert.False(t, reads(member, docExpired), "once the grant expires, the member no longer reads it")

	// Removal: the run's grants deleted, as at session end.
	require.NoError(t, c.DeleteSlotGrants(ctx, ns, run), "DeleteSlotGrants")
	assert.False(t, reads(member, docLive), "once the grant is removed, the member no longer reads it")
}
