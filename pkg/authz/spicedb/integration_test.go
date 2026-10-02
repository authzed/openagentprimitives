//go:build integration

// Integration tests against a real SpiceDB. The test binary spins up
// an authzed/spicedb container in serve-testing mode (via dockertest)
// — no external setup needed beyond a running docker daemon. Run with:
//
//	go test -tags=integration ./pkg/authz/spicedb/
//
// Each test gets its own isolated datastore via a unique bearer token
// (serve-testing keys datastores by token).
//
// These tests verify the actual permission semantics SpiceDB enforces
// against the production schema — unit tests with a fake authz can't
// catch shape mismatches between Touch* tuple shapes and Check*
// query shapes, which is exactly the regression that broke the
// multiplayer approve flow in production.
package spicedb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

func newIntegrationClient(t *testing.T) *Client {
	t.Helper()
	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, endpoint, token)
	c, err := NewClient(endpoint, token, true)
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func uniq(t *testing.T, prefix string) string {
	t.Helper()
	var buf [8]byte
	_, err := rand.Read(buf[:])
	require.NoError(t, err, "rand.Read")
	return prefix + hex.EncodeToString(buf[:])
}

// uniqCanon is uniq for the (very common) case where the unique string is
// used as a bare canonical id argument to a typed Check*/Touch* method.
func uniqCanon(t *testing.T, prefix string) identity.CanonicalUserID {
	t.Helper()
	return identity.CanonicalFromTrusted(uniq(t, prefix), "test fixture")
}

// TestIntegration_ApproveFlow_GrantsCheckInteract reproduces the
// production "approve does nothing" bug at the SpiceDB layer.
//
// Production flow on approve:
//  1. HandleDecision computes canonical via identity.Principal.Canonical()
//     (email-keyed) for the requester.
//  2. TouchInteractParticipantUser writes
//     `agentsession#participant @ user:<canonical>`.
//  3. The next inbound from the same user calls CheckInteract with
//     the same canonical.
//
// CheckInteract MUST return true. If it doesn't, the user re-posts
// and gets denied — which is exactly what shipped to production.
// This test locks the write-shape == check-shape contract.
func TestIntegration_ApproveFlow_GrantsCheckInteract(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "approve-flow-")
	canonicalID := uniqCanon(t, "user-canonical-")

	t.Cleanup(func() {
		_ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName)
	})

	require.NoError(t, c.TouchInteractParticipantUser(ctx, ns, sessName, canonicalID), "TouchInteractParticipantUser")

	allowed, err := c.CheckInteract(ctx, ns, sessName, canonicalID, true /*fullyConsistent*/)
	require.NoError(t, err, "CheckInteract")
	assert.True(t, allowed,
		"CheckInteract returned false after approve.\n"+
			"  participant: agentsession:%s/%s #participant user:%s\n"+
			"  query:       agentsession:%s/%s #interact @ user:%s\n"+
			"This is the exact regression that broke the multiplayer approve flow in production — write-shape and check-shape must match.",
		ns, sessName, canonicalID,
		ns, sessName, canonicalID,
	)
}

// TestIntegration_DenyOverridesParticipant verifies the schema's
// `interact = owner + participant - denied` actually subtracts
// the denied set, even when the same user is also a participant.
func TestIntegration_DenyOverridesParticipant(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "deny-flow-")
	canonicalID := uniqCanon(t, "user-canonical-")

	t.Cleanup(func() {
		_ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName)
	})

	// User was first approved as a participant, then later denied.
	require.NoError(t, c.TouchInteractParticipantUser(ctx, ns, sessName, canonicalID), "TouchInteractParticipantUser")
	require.NoError(t, c.TouchDeniedUser(ctx, ns, sessName, canonicalID), "TouchDeniedUser")

	allowed, err := c.CheckInteract(ctx, ns, sessName, canonicalID, true)
	require.NoError(t, err, "CheckInteract")
	assert.False(t, allowed, "CheckInteract returned true despite denied tuple — denied must override participant per `interact = owner + participant - denied`")

	denied, err := c.CheckDenied(ctx, ns, sessName, canonicalID, true)
	require.NoError(t, err, "CheckDenied")
	assert.True(t, denied, "CheckDenied returned false despite TouchDeniedUser — write shape must be compatible with query shape")
}

// TestIntegration_OwnerGrantsCheckInteract verifies the original requester is
// granted CheckInteract through the owner relation. The production path writes
// started_by and owner separately (channelsd at create, the operator's owner
// resolver later); this test locks the owner→interact shape contract
// specifically. That started_by ALSO admits interact on its own — the guard
// against a session locking out its own starter while #owner is unwritten — is
// pinned separately in pkg/authz/spicedb/schema's semantics tests.
func TestIntegration_OwnerGrantsCheckInteract(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "started-by-")
	canonicalID := uniqCanon(t, "user-canonical-")

	t.Cleanup(func() {
		_ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName)
	})

	require.NoError(t, c.TouchStartedBy(ctx, ns, sessName, canonicalID), "TouchStartedBy")
	require.NoError(t, c.TouchOwner(ctx, ns, sessName, "user:"+canonicalID.String()), "TouchOwner")

	allowed, err := c.CheckInteract(ctx, ns, sessName, canonicalID, true)
	require.NoError(t, err, "CheckInteract")
	assert.True(t, allowed, "CheckInteract returned false for owner — interact = owner + participant; the Kube fast-path is gone, every inbound goes through SpiceDB")
}

// TestIntegration_OwnerGrantsCheckManageScope verifies the metaagent
// control-plane gate: the session owner is granted manage_scope and fork,
// and a non-owner is denied both. manage_scope = owner in the schema.
// This is the exact "only the owner may change scope / fork" semantics
// MetaagentReceived and ReconcileRestart enforce.
func TestIntegration_OwnerGrantsCheckManageScope(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "manage-scope-")
	ownerID := uniqCanon(t, "owner-canonical-")
	otherID := uniqCanon(t, "other-canonical-")

	t.Cleanup(func() {
		_ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName)
	})

	require.NoError(t, c.TouchStartedBy(ctx, ns, sessName, ownerID), "TouchStartedBy")
	require.NoError(t, c.TouchOwner(ctx, ns, sessName, "user:"+ownerID.String()), "TouchOwner")

	ownerAllowed, err := c.CheckManageScope(ctx, ns, sessName, ownerID, true /*fullyConsistent*/)
	require.NoError(t, err, "CheckManageScope owner")
	assert.True(t, ownerAllowed, "the session owner must be granted manage_scope")

	otherAllowed, err := c.CheckManageScope(ctx, ns, sessName, otherID, true)
	require.NoError(t, err, "CheckManageScope non-owner")
	assert.False(t, otherAllowed, "a non-owner must be denied manage_scope (manage_scope = owner)")

	// fork = owner mirrors manage_scope: the SessionFork control-plane
	// gate authorizes a restart-from-here only for the session owner. Same
	// fixture (owner@ownerID) covers both permissions.
	ownerFork, err := c.CheckFork(ctx, ns, sessName, ownerID, true /*fullyConsistent*/)
	require.NoError(t, err, "CheckFork owner")
	assert.True(t, ownerFork, "the session owner must be granted fork")

	otherFork, err := c.CheckFork(ctx, ns, sessName, otherID, true)
	require.NoError(t, err, "CheckFork non-owner")
	assert.False(t, otherFork, "a non-owner must be denied fork (fork = owner)")
}

// TestIntegration_ArtifactView_DerivesFromParentSession verifies that
// artifact#view permission is derived entirely from the parent session's
// interact permission: started_by + participants (minus denied) can view
// the artifact; outsiders cannot.
func TestIntegration_ArtifactView_DerivesFromParentSession(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "artifact-sess-")
	artifactID := uniq(t, "artifact-")
	starter := uniqCanon(t, "starter-")
	participant := uniqCanon(t, "participant-")
	outsider := uniqCanon(t, "outsider-")

	t.Cleanup(func() { _ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName) })

	require.NoError(t, c.TouchStartedBy(ctx, ns, sessName, starter), "TouchStartedBy")
	require.NoError(t, c.TouchOwner(ctx, ns, sessName, "user:"+starter.String()), "TouchOwner")
	require.NoError(t, c.TouchInteractParticipantUser(ctx, ns, sessName, participant), "TouchInteractParticipantUser")
	require.NoError(t, c.TouchArtifactParent(ctx, artifactID, ns, sessName), "TouchArtifactParent")

	starterOK, err := c.CheckArtifactView(ctx, artifactID, starter, true)
	require.NoError(t, err)
	assert.True(t, starterOK, "session starter must have artifact view")

	partOK, err := c.CheckArtifactView(ctx, artifactID, participant, true)
	require.NoError(t, err)
	assert.True(t, partOK, "approved participant must have artifact view")

	outOK, err := c.CheckArtifactView(ctx, artifactID, outsider, true)
	require.NoError(t, err)
	assert.False(t, outOK, "an outsider must NOT have artifact view")
}

// TestIntegration_ArtifactView_PlatformAdminSeesAnyArtifact locks the admin
// live-view gate: artifact#view's platform->view_audit arm means a
// platform admin (platform:platform#admin, whose view_audit aliases can_admin)
// may view ANY artifact — while the parent->interact path is preserved and an
// unrelated stranger stays denied. This is enforced PURELY in the schema; webd's
// existing CheckArtifactView passes for admins with no app-layer authz branch.
// The #platform tuple is written by TouchArtifactParent alongside #parent.
func TestIntegration_ArtifactView_PlatformAdminSeesAnyArtifact(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "artifact-admin-sess-")
	artifactID := uniq(t, "artifact-admin-")
	starter := uniqCanon(t, "starter-")      // session interactor (owner/started_by)
	admin := uniqCanon(t, "platform-admin-") // platform admin, NOT a session interactor
	stranger := uniqCanon(t, "stranger-")    // neither interactor nor admin

	t.Cleanup(func() {
		_ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName)
		_ = c.DeletePlatformAdmin(context.Background(), admin)
	})

	// Session interact fixture (started_by + owner) and the artifact links.
	require.NoError(t, c.TouchStartedBy(ctx, ns, sessName, starter), "TouchStartedBy")
	require.NoError(t, c.TouchOwner(ctx, ns, sessName, "user:"+starter.String()), "TouchOwner")
	// TouchArtifactParent writes BOTH artifact#parent and artifact#platform.
	require.NoError(t, c.TouchArtifactParent(ctx, artifactID, ns, sessName), "TouchArtifactParent")
	// Grant the platform admin (platform:platform#admin@user:admin).
	require.NoError(t, c.TouchPlatformAdmin(ctx, admin), "TouchPlatformAdmin")

	// Platform admin views any artifact via platform->view_audit — the whole
	// point of this feature.
	adminOK, err := c.CheckArtifactView(ctx, artifactID, admin, true)
	require.NoError(t, err, "CheckArtifactView admin")
	assert.True(t, adminOK, "a platform admin must have artifact view via platform->view_audit")

	// Session interactor path preserved (parent->interact).
	starterOK, err := c.CheckArtifactView(ctx, artifactID, starter, true)
	require.NoError(t, err, "CheckArtifactView interactor")
	assert.True(t, starterOK, "the session interactor must still have artifact view (parent->interact preserved)")

	// No over-grant: an unrelated non-admin, non-interactor stays denied.
	strangerOK, err := c.CheckArtifactView(ctx, artifactID, stranger, true)
	require.NoError(t, err, "CheckArtifactView stranger")
	assert.False(t, strangerOK, "a non-admin, non-interactor must NOT have artifact view (no over-grant)")
}

// TestIntegration_TouchOwner_SeedsAllGates locks the Plan-2 contract: TouchOwner
// (not TouchStartedBy) seeds agentsession#owner, and every session gate
// (interact/manage_scope/fork/approve) resolves through owner — so the owner
// has all of them and a stranger has none. owner is the ONLY source for
// manage_scope/fork/approve; interact additionally accepts started_by and
// participant (see the schema's interact/approve split).
func TestIntegration_TouchOwner_SeedsAllGates(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "owner-seed-")
	owner := uniqCanon(t, "user-owner-")
	stranger := uniqCanon(t, "user-stranger-")
	t.Cleanup(func() { _ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName) })

	require.NoError(t, c.TouchStartedBy(ctx, ns, sessName, owner), "TouchStartedBy")
	require.NoError(t, c.TouchOwner(ctx, ns, sessName, "user:"+owner.String()), "TouchOwner")

	gates := []struct {
		name  string
		check func(ctx context.Context, ns, name string, canon identity.CanonicalUserID, fc bool) (bool, error)
	}{
		{"interact", c.CheckInteract},
		{"manage_scope", c.CheckManageScope},
		{"fork", c.CheckFork},
		{"approve", c.CheckApprove},
	}
	for _, g := range gates {
		ok, err := g.check(ctx, ns, sessName, owner, true)
		require.NoError(t, err, "%s check (owner)", g.name)
		assert.True(t, ok, "owner must have %s via the owner relation", g.name)

		ok, err = g.check(ctx, ns, sessName, stranger, true)
		require.NoError(t, err, "%s check (stranger)", g.name)
		assert.False(t, ok, "stranger must NOT have %s", g.name)
	}
}

// TestIntegration_GroupMemberGrantsCheckInteract verifies class-wide
// grants: writing `participant @ group:<g>#member` and a separate
// `group:<g>#member @ user:<canonical>` makes CheckInteract return
// true for that canonical user. This is how AgentClass.spec.session
// InteractPermission "group:engineering#member" works in production.
func TestIntegration_GroupMemberGrantsCheckInteract(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "class-grant-")
	groupName := uniq(t, "test-group-")
	canonicalID := uniqCanon(t, "user-canonical-")

	t.Cleanup(func() {
		_ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName)
		// Group cleanup: delete the membership we wrote so reruns don't pile up.
		_ = c.DeleteGroupMember(context.Background(), groupName, canonicalID)
	})

	// Operator-style: populate group membership first.
	require.NoError(t, c.AddGroupMember(ctx, groupName, canonicalID), "AddGroupMember")
	// Class-wide grant: agentsession#participant @ group:<g>#member
	require.NoError(t, c.TouchInteractParticipant(ctx, ns, sessName, "group:"+groupName+"#member"), "TouchInteractParticipant")

	allowed, err := c.CheckInteract(ctx, ns, sessName, canonicalID, true)
	require.NoError(t, err, "CheckInteract")
	assert.True(t, allowed, "CheckInteract returned false for a group member — class-wide grants must work via the group#member subject set walking to user:<canonical>")
}

// TestIntegration_TouchOwner_UserAndGroup verifies owner can be a direct user
// OR a subject-set ref, and that approve resolves through it.
func TestIntegration_TouchOwner_UserAndGroup(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ns, sess := "integration-test", uniq(t, "touchowner-")
	grp, member := uniq(t, "group-"), uniqCanon(t, "user-mem-")
	t.Cleanup(func() { _ = c.DeleteAgentSessionRelationships(context.Background(), ns, sess) })

	require.NoError(t, c.AddGroupMember(ctx, grp, member), "AddGroupMember")
	require.NoError(t, c.TouchOwner(ctx, ns, sess, "group:"+grp+"#member"), "TouchOwner(group ref)")

	ok, err := c.CheckApprove(ctx, ns, sess, member, true)
	require.NoError(t, err)
	assert.True(t, ok, "group member must have approve via owner→group#member")
}

// TestIntegration_CheckOwnerOnResource verifies CheckOwnerOnResource checks the
// #owner relation on a resource via user:<canonicalID>. We reuse agentsession as
// the resource type — TouchOwner seeds agentsession#owner@user:<canon>, then
// CheckOwnerOnResource(ctx, "agentsession", "ns/name", canon, true) must return
// true for the owner and false for a stranger. The permission is "owner" which
// is defined as a relation on agentsession; SpiceDB CheckPermission works on
// relations as well as derived permissions.
func TestIntegration_CheckOwnerOnResource(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "owner-resource-")
	owner := uniqCanon(t, "user-owner-")
	stranger := uniqCanon(t, "user-stranger-")
	resID := ns + "/" + sessName

	t.Cleanup(func() { _ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName) })

	require.NoError(t, c.TouchOwner(ctx, ns, sessName, "user:"+owner.String()), "TouchOwner")

	ownerOK, err := c.CheckOwnerOnResource(ctx, "agentsession", resID, owner, true)
	require.NoError(t, err, "CheckOwnerOnResource (owner)")
	assert.True(t, ownerOK, "owner must satisfy agentsession:<resID>#owner")

	strangerOK, err := c.CheckOwnerOnResource(ctx, "agentsession", resID, stranger, true)
	require.NoError(t, err, "CheckOwnerOnResource (stranger)")
	assert.False(t, strangerOK, "stranger must NOT satisfy agentsession:<resID>#owner")
}

// TestIntegration_AuthorizedTokenGrantLifecycle locks the write/list/delete
// shape contract for the externaltoken token-use authorization grant: a
// value-bound (caveated) grant round-trips its authorized_value_hash through
// ListAuthorizedTokens, an identity-only (uncaveated, federated) grant reads
// back with an empty hash, and deleting one grant leaves the other intact.
func TestIntegration_AuthorizedTokenGrantLifecycle(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "authorized-token-")
	credID := uniq(t, "cred-")
	fedCredID := uniq(t, "cred-fed-")

	t.Cleanup(func() {
		_ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName)
	})

	// Write a value-bound grant.
	require.NoError(t, c.TouchAuthorizedToken(ctx, ns, sessName, credID, "hash-v1"), "touch caveated grant")

	// List returns it with the value hash.
	got, err := c.ListAuthorizedTokens(ctx, ns, sessName)
	require.NoError(t, err, "list grants")
	require.Len(t, got, 1)
	assert.Equal(t, credID, got[0].CredID)
	assert.Equal(t, "hash-v1", got[0].AuthorizedValueHash)

	// Identity-only (federated) grant for a second credential.
	require.NoError(t, c.TouchAuthorizedTokenIdentity(ctx, ns, sessName, fedCredID), "touch identity grant")
	got, err = c.ListAuthorizedTokens(ctx, ns, sessName)
	require.NoError(t, err)
	require.Len(t, got, 2)

	// Delete the first.
	require.NoError(t, c.DeleteAuthorizedToken(ctx, ns, sessName, credID), "delete grant")
	got, err = c.ListAuthorizedTokens(ctx, ns, sessName)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, fedCredID, got[0].CredID)
	assert.Empty(t, got[0].AuthorizedValueHash, "identity-only grant has no value hash")
}

// TestIntegration_CheckUseToken verifies that CheckUseToken correctly evaluates
// caveated and identity-only grants against presented token values.
func TestIntegration_CheckUseToken(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "check-use-token-")

	t.Cleanup(func() {
		_ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName)
	})

	// Value-bound grant.
	require.NoError(t, c.TouchAuthorizedToken(ctx, ns, sessName, "cred-a", "good-hash"), "touch caveated grant")
	// Identity-only grant.
	require.NoError(t, c.TouchAuthorizedTokenIdentity(ctx, ns, sessName, "cred-fed"), "touch identity grant")

	cases := []struct {
		name        string
		credID      string
		presented   string
		wantAllowed bool
	}{
		{"caveated match: allow", "cred-a", "good-hash", true},
		{"caveated mismatch (stale value): deny", "cred-a", "bad-hash", false},
		{"identity-only ignores context: allow", "cred-fed", "anything", true},
		{"absent grant (revoked): deny", "cred-missing", "x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := c.CheckUseToken(ctx, ns, sessName, tc.credID, tc.presented, true)
			require.NoError(t, err, "check must not error when SpiceDB is reachable")
			assert.Equal(t, tc.wantAllowed, ok)
		})
	}
}

// TestIntegration_DenyRemovesApproveForkManageScope is the regression test for a
// live defect: `denied` subtracted from `interact` only, so a user an owner
// explicitly Denied lost read access while RETAINING the ability to approve
// plans and tool calls, fork the session, and widen its scope via the metaagent.
//
// Clicking Deny must mean denied. All four permissions subtract the blocklist.
func TestIntegration_DenyRemovesApproveForkManageScope(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "deny-approve-")
	canonicalID := uniqCanon(t, "denied-owner-")

	t.Cleanup(func() {
		_ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName)
	})

	// An owner — the strongest standing there is — who is then denied.
	require.NoError(t, c.TouchOwner(ctx, ns, sessName, canonicalID.Subject().String()), "TouchOwner")
	require.NoError(t, c.TouchDeniedUser(ctx, ns, sessName, canonicalID), "TouchDeniedUser")

	interact, err := c.CheckInteract(ctx, ns, sessName, canonicalID, true)
	require.NoError(t, err, "CheckInteract")
	assert.False(t, interact, "denied must remove interact (already true before this fix)")

	approve, err := c.CheckApprove(ctx, ns, sessName, canonicalID, true)
	require.NoError(t, err, "CheckApprove")
	assert.False(t, approve, "a DENIED user must not be able to approve — `approve = owner - denied`")

	fork, err := c.CheckFork(ctx, ns, sessName, canonicalID, true)
	require.NoError(t, err, "CheckFork")
	assert.False(t, fork, "a DENIED user must not be able to fork — `fork = owner - denied`")

	manageScope, err := c.CheckManageScope(ctx, ns, sessName, canonicalID, true)
	require.NoError(t, err, "CheckManageScope")
	assert.False(t, manageScope, "a DENIED user must not be able to widen scope — `manage_scope = owner - denied`")
}

// TestIntegration_UnDenyRestoresStanding verifies Deny is reversible. Without a
// delete path a misclick is permanent for the life of the session.
func TestIntegration_UnDenyRestoresStanding(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "undeny-")
	canonicalID := uniqCanon(t, "undenied-user-")

	t.Cleanup(func() {
		_ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName)
	})

	require.NoError(t, c.TouchOwner(ctx, ns, sessName, canonicalID.Subject().String()), "TouchOwner")
	require.NoError(t, c.TouchDeniedUser(ctx, ns, sessName, canonicalID), "TouchDeniedUser")

	denied, err := c.CheckDenied(ctx, ns, sessName, canonicalID, true)
	require.NoError(t, err, "CheckDenied after deny")
	require.True(t, denied, "precondition: user is denied")

	require.NoError(t, c.DeleteDeniedUser(ctx, ns, sessName, canonicalID), "DeleteDeniedUser")

	denied, err = c.CheckDenied(ctx, ns, sessName, canonicalID, true)
	require.NoError(t, err, "CheckDenied after un-deny")
	assert.False(t, denied, "un-deny must clear the blocklist entry")

	approve, err := c.CheckApprove(ctx, ns, sessName, canonicalID, true)
	require.NoError(t, err, "CheckApprove after un-deny")
	assert.True(t, approve, "owner standing must return once the deny is lifted")
}

// slotSchemaFragment is the shape the schema composer injects into a slot
// resource's definition (pkg/guardian/schema/slots.go). Slot resources are
// per-AgentClass, so they are not in the canonical schema and a test that
// exercises them has to supply its own.
const slotSchemaFragment = `
definition demo_target {
    relation slot_grant_reachable: agentsession with expiration
    relation slot_grant_mutate: agentsession with expiration
    relation slot_pin: agentsession
    relation owner: user
    permission reachable = slot_grant_reachable->interact + owner
    permission mutate = slot_grant_mutate->interact + owner
}

definition demo_widget {
    relation slot_grant_usable: agentsession with expiration
    relation slot_pin: agentsession
    relation owner: user
    permission usable = slot_grant_usable->interact + owner
}
`

// newSlotIntegrationClient is newIntegrationClient with the slot fragment
// appended to the schema.
func newSlotIntegrationClient(t *testing.T) *Client {
	t.Helper()
	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchemaWithExtra(t, endpoint, token, slotSchemaFragment)
	c, err := NewClient(endpoint, token, true)
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestIntegration_SlotGrant_ResolvesThroughSessionMembership locks the
// instance-axis grant shape against real SpiceDB: the tuple points from the
// RESOURCE at the SESSION, so the resource's permission resolves the session's
// member set and the Check is per-requester with no wildcard leaf.
//
// The mirror image — session#grant_<perm>_<type>@<type>:<id> — is a real shape
// in this codebase, and it can only evaluate through a wildcard, which is
// exactly what reversing the direction removes. A test that only asserted "the
// owner passes" would pass under both shapes; the outsider and denied cases are
// what tell them apart.
func TestIntegration_SlotGrant_ResolvesThroughSessionMembership(t *testing.T) {
	c := newSlotIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "slot-sess-")
	targetID := uniq(t, "target-")
	owner := uniqCanon(t, "slot-owner-")
	participant := uniqCanon(t, "slot-participant-")
	blocked := uniqCanon(t, "slot-blocked-")
	outsider := uniqCanon(t, "slot-outsider-")

	t.Cleanup(func() {
		_ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName)
		_ = c.DeleteSlotGrants(context.Background(), ns, sessName)
	})

	require.NoError(t, c.TouchOwner(ctx, ns, sessName, owner.Subject().String()), "TouchOwner")
	require.NoError(t, c.TouchInteractParticipantUser(ctx, ns, sessName, participant), "TouchInteractParticipantUser")
	require.NoError(t, c.TouchInteractParticipantUser(ctx, ns, sessName, blocked), "TouchInteractParticipantUser (blocked)")
	require.NoError(t, c.TouchDeniedUser(ctx, ns, sessName, blocked), "TouchDeniedUser")

	sess := authz.SessionRef{Namespace: ns, Name: sessName}
	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), sess,
		[]authz.SlotBinding{{ResourceType: "demo_target", ResourceID: authz.TrustedObjectID(targetID), Permission: "reachable"}}, time.Now().Add(time.Hour)), "GrantSlots")

	check := func(who identity.CanonicalUserID) bool {
		t.Helper()
		ok, err := c.CheckOnResource(ctx, "demo_target", targetID, "reachable", who, true)
		require.NoError(t, err, "CheckOnResource")
		return ok
	}

	assert.True(t, check(owner), "the session owner must reach a granted instance")
	assert.True(t, check(participant), "a session participant must reach a granted instance")
	assert.False(t, check(blocked), "a denied participant must NOT reach a granted instance (denied is honored for free)")
	assert.False(t, check(outsider), "an outsider must NOT reach a granted instance — no wildcard leaf")

	require.NoError(t, authz.RevokeSlots(ctx, c.Relations(), sess,
		[]authz.SlotBinding{{ResourceType: "demo_target", ResourceID: authz.TrustedObjectID(targetID), Permission: "reachable"}}), "RevokeSlots")
	assert.False(t, check(participant), "revoking the grant must deny the next fully-consistent Check")
}

// TestIntegration_ListAndDeleteSlotGrants_AreSessionScopedAcrossTypes covers
// the two operations session teardown depends on. Slot grants live on the
// RESOURCE with the session as SUBJECT, so DeleteAgentSessionRelationships —
// which filters agentsession as the resource — never collects them; without a
// reciprocal delete every session leaves live authority behind on external
// resources.
//
// The resource type is deliberately absent from both filters: SpiceDB accepts a
// subject-only relationship filter, so one RPC covers every slot type a session
// touched, including types whose AgentClass no longer declares them.
func TestIntegration_ListAndDeleteSlotGrants_AreSessionScopedAcrossTypes(t *testing.T) {
	c := newSlotIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ns := "integration-test"
	mine := uniq(t, "slot-mine-")
	theirs := uniq(t, "slot-theirs-")
	targetID := uniq(t, "target-")
	widgetID := uniq(t, "widget-")
	otherTargetID := uniq(t, "other-target-")

	t.Cleanup(func() {
		_ = c.DeleteSlotGrants(context.Background(), ns, mine)
		_ = c.DeleteSlotGrants(context.Background(), ns, theirs)
	})

	mineRef := authz.SessionRef{Namespace: ns, Name: mine}
	theirsRef := authz.SessionRef{Namespace: ns, Name: theirs}

	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), mineRef, []authz.SlotBinding{
		{ResourceType: "demo_target", ResourceID: authz.TrustedObjectID(targetID), Permission: "reachable"},
		{ResourceType: "demo_widget", ResourceID: authz.TrustedObjectID(widgetID), Permission: "usable"},
	}, time.Now().Add(time.Hour)), "GrantSlots (mine, two types)")
	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), theirsRef, []authz.SlotBinding{
		{ResourceType: "demo_target", ResourceID: authz.TrustedObjectID(otherTargetID), Permission: "reachable"},
	}, time.Now().Add(time.Hour)), "GrantSlots (theirs)")

	got, err := c.ListSlotGrants(ctx, ns, mine)
	require.NoError(t, err, "ListSlotGrants")
	assert.ElementsMatch(t, []authz.SlotBinding{
		{ResourceType: "demo_target", ResourceID: authz.TrustedObjectID(targetID), Permission: "reachable"},
		{ResourceType: "demo_widget", ResourceID: authz.TrustedObjectID(widgetID), Permission: "usable"},
	}, got, "one call must return every type this session holds, and only this session's")

	require.NoError(t, c.DeleteSlotGrants(ctx, ns, mine), "DeleteSlotGrants")

	got, err = c.ListSlotGrants(ctx, ns, mine)
	require.NoError(t, err, "ListSlotGrants after teardown")
	assert.Empty(t, got, "teardown must collect grants across every resource type")

	survived, err := c.ListSlotGrants(ctx, ns, theirs)
	require.NoError(t, err, "ListSlotGrants (other session)")
	assert.Equal(t, []authz.SlotBinding{{ResourceType: "demo_target", ResourceID: authz.TrustedObjectID(otherTargetID), Permission: "reachable"}}, survived,
		"tearing one session down must not touch another session's grants")
}

// TestIntegration_SlotGrant_ExpiryIsTheLeakBackstop proves the property that
// justifies rejecting standing grants at all.
//
// Teardown cannot be the only bound on a slot grant: the tuple lives on somebody
// else's RESOURCE, the AgentSession CR is retained after completion, and
// slot_grant->interact keeps resolving — so a teardown that fails once would
// leak live authority indefinitely. The expiry is what makes that recoverable
// WITHOUT any cleanup running, so this test never calls DeleteSlotGrants.
func TestIntegration_SlotGrant_ExpiryIsTheLeakBackstop(t *testing.T) {
	c := newSlotIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "slot-expiry-")
	targetID := uniq(t, "target-")
	owner := uniqCanon(t, "expiry-owner-")

	t.Cleanup(func() {
		_ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName)
		_ = c.DeleteSlotGrants(context.Background(), ns, sessName)
	})

	require.NoError(t, c.TouchOwner(ctx, ns, sessName, owner.Subject().String()), "TouchOwner")
	sess := authz.SessionRef{Namespace: ns, Name: sessName}

	// Already lapsed: SpiceDB must treat it as absent even though the tuple was
	// accepted and no teardown has run.
	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), sess,
		[]authz.SlotBinding{{ResourceType: "demo_target", ResourceID: authz.TrustedObjectID(targetID), Permission: "reachable"}},
		time.Now().Add(-time.Minute)), "GrantSlots (already expired)")

	// The owner still passes — through the `owner` leg, which has nothing to do
	// with the grant. Asserting on a NON-owner is what isolates the expiry.
	participant := uniqCanon(t, "expiry-participant-")
	require.NoError(t, c.TouchInteractParticipantUser(ctx, ns, sessName, participant), "TouchInteractParticipantUser")

	ok, err := c.CheckOnResource(ctx, "demo_target", targetID, "reachable", participant, true)
	require.NoError(t, err, "CheckOnResource")
	assert.False(t, ok,
		"an expired slot grant must not authorize, with no teardown having run — this is the leak backstop")

	// And a live grant for the same instance does authorize, so the assertion
	// above is about the EXPIRY and not about the tuple never having landed.
	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), sess,
		[]authz.SlotBinding{{ResourceType: "demo_target", ResourceID: authz.TrustedObjectID(targetID), Permission: "reachable"}},
		time.Now().Add(time.Hour)), "GrantSlots (live)")
	ok, err = c.CheckOnResource(ctx, "demo_target", targetID, "reachable", participant, true)
	require.NoError(t, err, "CheckOnResource (live)")
	assert.True(t, ok, "a live grant must authorize; otherwise the expiry test proves nothing")
}

// TestIntegration_RevokeSlot_DeniesTheNextCheck is the property revocation
// rests on: a slot grant is a tuple, so removing it is visible to the very next
// fully-consistent check with no cache to wait out and no session restart.
//
// It revokes ONE instance of two held, because "revocation works" is not the
// interesting claim — "revocation is surgical" is. An operator forced to choose
// between killing the whole session and leaving it holding something it should
// not will usually pick neither.
func TestIntegration_RevokeSlot_IsSurgicalAndImmediate(t *testing.T) {
	c := newSlotIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "slot-revoke-")
	keepID := uniq(t, "keep-")
	dropID := uniq(t, "drop-")
	member := uniqCanon(t, "revoke-member-")

	t.Cleanup(func() {
		_ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName)
		_ = c.DeleteSlotGrants(context.Background(), ns, sessName)
	})

	require.NoError(t, c.TouchInteractParticipantUser(ctx, ns, sessName, member), "TouchInteractParticipantUser")
	sess := authz.SessionRef{Namespace: ns, Name: sessName}
	// Occupancy multi: surgical revocation is only interesting when the session
	// holds MORE THAN ONE instance of a type, which single-occupancy pinning
	// forbids by construction. This is the explicit multi-set case.
	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), sess, []authz.SlotBinding{
		{ResourceType: "demo_target", ResourceID: authz.TrustedObjectID(keepID), Permission: "reachable", Occupancy: "multi"},
		{ResourceType: "demo_target", ResourceID: authz.TrustedObjectID(dropID), Permission: "reachable", Occupancy: "multi"},
	}, time.Now().Add(time.Hour)), "GrantSlots")

	reach := func(id string) bool {
		t.Helper()
		ok, err := c.CheckOnResource(ctx, "demo_target", id, "reachable", member, true)
		require.NoError(t, err, "CheckOnResource")
		return ok
	}
	require.True(t, reach(keepID), "precondition: both instances reachable")
	require.True(t, reach(dropID), "precondition: both instances reachable")

	require.NoError(t, authz.RevokeSlots(ctx, c.Relations(), sess,
		[]authz.SlotBinding{{ResourceType: "demo_target", ResourceID: authz.TrustedObjectID(dropID), Permission: "reachable"}}), "RevokeSlots")

	assert.False(t, reach(dropID), "the revoked instance must be denied by the very next check")
	assert.True(t, reach(keepID), "revocation must be surgical — the session keeps everything else")

	// And the listing reflects it, since that is how an operator confirms.
	held, err := c.ListSlotGrants(ctx, ns, sessName)
	require.NoError(t, err)
	assert.Equal(t, []authz.SlotBinding{{ResourceType: "demo_target", ResourceID: authz.TrustedObjectID(keepID), Permission: "reachable"}}, held)
}

// TestIntegration_SlotGrant_OnePermissionDoesNotSatisfyAnother is the property
// that makes it safe for a slot grant to carry no args binding at all.
//
// The session-grant mechanism this replaces bound an approval to the exact
// arguments hash, so it could not be reused for a different call. A slot grant
// deliberately does not: it authorizes an INSTANCE, and the class axis is what
// bounds what may be done to it. That trade is only sound while a grant for one
// permission cannot exercise a DIFFERENT permission on the same instance —
// otherwise a read-scoped slot on a repository would authorize a write, because
// the same tool (a git CLI) reaches the same resource id with different args.
//
// A single shared relation name broke this outright, which is what this test
// exists to prevent returning.
func TestIntegration_SlotGrant_OnePermissionDoesNotSatisfyAnother(t *testing.T) {
	c := newSlotIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "slot-perm-iso-")
	targetID := uniq(t, "target-")
	member := uniqCanon(t, "perm-iso-member-")

	t.Cleanup(func() {
		_ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName)
		_ = c.DeleteSlotGrants(context.Background(), ns, sessName)
	})

	require.NoError(t, c.TouchInteractParticipantUser(ctx, ns, sessName, member), "TouchInteractParticipantUser")
	sess := authz.SessionRef{Namespace: ns, Name: sessName}

	// Granted READ on the instance, and nothing else.
	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), sess,
		[]authz.SlotBinding{{ResourceType: "demo_target", ResourceID: authz.TrustedObjectID(targetID), Permission: "reachable"}},
		time.Now().Add(time.Hour)), "GrantSlots(reachable)")

	readOK, err := c.CheckOnResource(ctx, "demo_target", targetID, "reachable", member, true)
	require.NoError(t, err)
	assert.True(t, readOK, "the granted permission must work; otherwise the negative below proves nothing")

	writeOK, err := c.CheckOnResource(ctx, "demo_target", targetID, "mutate", member, true)
	require.NoError(t, err)
	assert.False(t, writeOK,
		"a grant for one permission must NOT satisfy another on the same instance — "+
			"this is what makes dropping the args-hash binding safe")
}

// TestIntegration_NewMetaagentPermissionsResolve verifies that manage_budget
// and manage_model exist in the written schema and resolve through owner.
//
// The schema change that added them is the kind whose failure is invisible
// until something reaches for it: a typo in schema.zed compiles fine, passes
// every unit test, and only surfaces when a capability first calls the gate —
// at which point SpiceDB reports an unknown permission and the capability is
// simply unavailable. Checking them against a real datastore is what turns that
// into a build-time answer.
//
// Both resolve through owner today, so the assertions mirror manage_scope
// exactly. That is the point: they grant nobody anything new, and the split
// exists so they can diverge later without breaking call sites.
func TestIntegration_NewMetaagentPermissionsResolve(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "meta-perms-")
	ownerID := uniqCanon(t, "owner-canonical-")
	otherID := uniqCanon(t, "other-canonical-")

	t.Cleanup(func() {
		_ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName)
	})
	require.NoError(t, c.TouchOwner(ctx, ns, sessName, "user:"+ownerID.String()), "TouchOwner")

	for _, perm := range []string{"manage_budget", "manage_model"} {
		t.Run(perm+": owner allowed, non-owner denied", func(t *testing.T) {
			ownerAllowed, err := c.CheckSessionPermission(ctx, ns, sessName, perm, ownerID, true)
			require.NoError(t, err,
				"%s must exist in the written schema; an error here means schema.zed and the "+
					"code disagree about what permissions exist", perm)
			assert.True(t, ownerAllowed, "the session owner must hold %s", perm)

			otherAllowed, err := c.CheckSessionPermission(ctx, ns, sessName, perm, otherID, true)
			require.NoError(t, err)
			assert.False(t, otherAllowed, "a non-owner must be denied %s", perm)
		})
	}
}

// TestIntegration_DeniedLosesTheNewPermissions pins the `- denied` half.
//
// Every gate on agentsession subtracts denied, and these two were written to
// match. Without it, an owner who explicitly Denied a user would find that user
// could still widen a budget — which is the "lost read access" misreading of the
// Deny button the schema comment warns about.
func TestIntegration_DeniedLosesTheNewPermissions(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "meta-denied-")
	ownerID := uniqCanon(t, "owner-canonical-")

	t.Cleanup(func() {
		_ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName)
	})
	require.NoError(t, c.TouchOwner(ctx, ns, sessName, "user:"+ownerID.String()), "TouchOwner")
	require.NoError(t, c.TouchDeniedUser(ctx, ns, sessName, ownerID), "TouchDeniedUser")

	for _, perm := range []string{"manage_budget", "manage_model"} {
		allowed, err := c.CheckSessionPermission(ctx, ns, sessName, perm, ownerID, true)
		require.NoError(t, err)
		assert.False(t, allowed,
			"a denied user must lose %s, whatever else grants it — denied wins over every grant", perm)
	}
}
