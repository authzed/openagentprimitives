//go:build integration

// Semantic tests for the canonical schema's agentsession permissions, run
// against a real SpiceDB via testspicedb. The unit-suite companion
// (schema_test.go) pins the schema TEXT; this file pins what the text MEANS —
// text pins cannot tell you that `- denied` still beats a newly-added term.
//
//	go test -tags=integration -count=1 ./pkg/authz/spicedb/schema/
package schema_test

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/authzed/grpcutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// objRef parses "agentsession:default/s1" into an ObjectReference.
func objRef(t *testing.T, s string) *v1.ObjectReference {
	t.Helper()
	objType, objID, ok := strings.Cut(s, ":")
	require.True(t, ok, "object ref %q must be <type>:<id>", s)
	return &v1.ObjectReference{ObjectType: objType, ObjectId: objID}
}

// subjRef parses "user:alice" or the subject-set form "group:sec#member".
func subjRef(t *testing.T, s string) *v1.SubjectReference {
	t.Helper()
	obj, rel, _ := strings.Cut(s, "#")
	return &v1.SubjectReference{Object: objRef(t, obj), OptionalRelation: rel}
}

// parseRel parses "agentsession:default/s1#owner@group:sec#member".
//
// A subject may carry a trailing "|expires:<duration>" to write an expiring
// tuple: "pt_tag:t#granted_to@agentsession:default/c|expires:1h". A relation
// declared `<type> with expiration` REQUIRES one — SpiceDB rejects a bare
// subject on such a relation with InvalidArgument, which surfaces as a
// WriteRelationships failure rather than as the permission answer under test.
func parseRel(t *testing.T, s string) *v1.Relationship {
	t.Helper()
	resource, rest, ok := strings.Cut(s, "#")
	require.True(t, ok, "relationship %q must contain #", s)
	relation, subject, ok := strings.Cut(rest, "@")
	require.True(t, ok, "relationship %q must contain @", s)
	rel := &v1.Relationship{Resource: objRef(t, resource), Relation: relation}
	if subj, dur, hasExp := strings.Cut(subject, "|expires:"); hasExp {
		d, err := time.ParseDuration(dur)
		require.NoError(t, err, "relationship %q: bad expires duration %q", s, dur)
		rel.OptionalExpiresAt = timestamppb.New(time.Now().Add(d))
		subject = subj
	}
	rel.Subject = subjRef(t, subject)
	return rel
}

// checkOnCanonicalSchema seeds rels into a fresh per-test datastore loaded with
// the canonical schema and evaluates one permission. rels/resource/subject use
// the standard zed text forms ("agentsession:default/s1#owner@user:alice").
func checkOnCanonicalSchema(t *testing.T, rels []string, resource, permission, subject string) v1.CheckPermissionResponse_Permissionship {
	t.Helper()
	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, endpoint, token)

	conn, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpcutil.WithInsecureBearerToken(token),
	)
	require.NoError(t, err, "dial spicedb")
	t.Cleanup(func() { _ = conn.Close() })
	perm := v1.NewPermissionsServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	updates := make([]*v1.RelationshipUpdate, 0, len(rels))
	for _, r := range rels {
		updates = append(updates, &v1.RelationshipUpdate{
			Operation:    v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: parseRel(t, r),
		})
	}
	if len(updates) > 0 {
		_, err = perm.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{Updates: updates})
		require.NoError(t, err, "WriteRelationships")
	}

	res, err := perm.CheckPermission(ctx, &v1.CheckPermissionRequest{
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		Resource:    objRef(t, resource),
		Permission:  permission,
		Subject:     subjRef(t, subject),
	})
	require.NoError(t, err, "CheckPermission %s#%s for %s", resource, permission, subject)
	return res.Permissionship
}

// TestAgentSessionStarterStandingIsInteractOnly pins the split that keeps an
// ownerless session usable without making its starter an approver.
//
// The production failure this encodes: channelsd writes agentsession#started_by
// at session-create time, but agentsession#owner is written later, by the
// OPERATOR's owner resolver (it alone knows the per-channel owner policy). A
// session whose reconcile has not reached that write yet — or never will —
// carried started_by and nothing else, and since every gate resolved through
// #owner the session denied its OWN starter, who was then asked to approve
// their own join request (and could not: `approve = owner` was false too).
// Including started_by in #interact makes that state impossible.
//
// started_by must stay OUT of approve/manage_scope/fork: channel.spec.owner.
// explicit exists precisely so a channel can declare "sessions here are owned
// by this team, not by whoever typed first". Granting the starter approve would
// let them self-approve their own tool calls on such a channel, silently
// voiding that policy.
func TestAgentSessionStarterStandingIsInteractOnly(t *testing.T) {
	const (
		sess    = "agentsession:default/s1"
		starter = "user:starter"
	)
	// The exact production state observed on the wedged session: started_by
	// present, owner absent.
	ownerless := []string{sess + "#started_by@" + starter}

	cases := []struct {
		name  string
		rels  []string
		perm  string
		who   string
		grant bool
	}{
		{
			name:  "ownerless session: starter may interact (its own thread, before #owner lands)",
			rels:  ownerless,
			perm:  "interact",
			who:   starter,
			grant: true,
		},
		{
			name:  "ownerless session: starter may NOT approve (no self-approval on an explicit-owner channel)",
			rels:  ownerless,
			perm:  "approve",
			who:   starter,
			grant: false,
		},
		{
			name:  "ownerless session: starter may NOT manage_scope",
			rels:  ownerless,
			perm:  "manage_scope",
			who:   starter,
			grant: false,
		},
		{
			name:  "ownerless session: starter may NOT fork",
			rels:  ownerless,
			perm:  "fork",
			who:   starter,
			grant: false,
		},
		{
			name:  "blocklisted starter: denied still wins over started_by",
			rels:  append(append([]string{}, ownerless...), sess+"#denied@"+starter),
			perm:  "interact",
			who:   starter,
			grant: false,
		},
		{
			name:  "stranger on an ownerless session: no interact",
			rels:  ownerless,
			perm:  "interact",
			who:   "user:stranger",
			grant: false,
		},
		{
			name: "explicit-owner channel: the group owner keeps approve",
			rels: []string{
				sess + "#started_by@" + starter,
				sess + "#owner@group:sec#member",
				"group:sec#member@user:secmember",
			},
			perm:  "approve",
			who:   "user:secmember",
			grant: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkOnCanonicalSchema(t, tc.rels, sess, tc.perm, tc.who)
			want := v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION
			if tc.grant {
				want = v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
			}
			assert.Equal(t, want, got, "%s#%s for %s", sess, tc.perm, tc.who)
		})
	}
}

// TestMemoryAndArtifactReadFollowStarterInteract pins the two permissions that
// derive from agentsession#interact. Adding started_by to interact widens them
// too — intended (the starter authored the prompt the transcript opens with),
// and pinned here so a future edit to interact cannot widen transcript/artifact
// read as an unnoticed side effect.
func TestMemoryAndArtifactReadFollowStarterInteract(t *testing.T) {
	rels := []string{
		"agentsession:default/s1#started_by@user:starter",
		"memory_entry:default/s1/e1#session@agentsession:default/s1",
		"artifact:artifact-abc#parent@agentsession:default/s1",
	}
	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		checkOnCanonicalSchema(t, rels, "memory_entry:default/s1/e1", "read", "user:starter"),
		"the starter can read their own session's transcript")
	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		checkOnCanonicalSchema(t, rels, "artifact:artifact-abc", "view", "user:starter"),
		"the starter can view their own session's artifacts")
	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
		checkOnCanonicalSchema(t, rels, "memory_entry:default/s1/e1", "read", "user:stranger"),
		"a stranger still cannot read the transcript")
}

// TestDelegationBlockedResolvesFromTheWildcardRevoke pins the operator-side
// kill switch's SpiceDB half — the piece the SubagentRequest controller's unit
// tests (which use a fake Authz) cannot reach. delegation_blocked is what
// CheckDelegationBlocked queries with a concrete probe subject, and it must:
//
//   - deny by default (no delegation_revoked tuple), so delegation is permitted
//     unless an operator has explicitly armed the switch; and
//   - resolve HAS_PERMISSION for ANY concrete user once a wildcard
//     agentsession:X#delegation_revoked@user:* is written, because the switch is
//     session-wide — who asked to delegate is immaterial once it is armed, which
//     is exactly why the client probes with a fixed throwaway subject.
//
// A schema-text pin cannot catch a wildcard that stopped matching a concrete
// subject, or a permission renamed out from under the client; this can.
func TestDelegationBlockedResolvesFromTheWildcardRevoke(t *testing.T) {
	const sess = "agentsession:default/s1"

	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
		checkOnCanonicalSchema(t, nil, sess, "delegation_blocked", "user:delegation-revocation-probe"),
		"with no revoke tuple, delegation is not blocked (default-off kill switch)")

	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		checkOnCanonicalSchema(t, []string{sess + "#delegation_revoked@user:*"},
			sess, "delegation_blocked", "user:delegation-revocation-probe"),
		"a wildcard revoke blocks delegation for the probe subject the client uses")

	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		checkOnCanonicalSchema(t, []string{sess + "#delegation_revoked@user:*"},
			sess, "delegation_blocked", "user:someone-else"),
		"the switch is session-wide: it blocks for any concrete subject, not just the probe")

	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
		checkOnCanonicalSchema(t, []string{sess + "#delegation_revoked@user:*"},
			"agentsession:default/other", "delegation_blocked", "user:delegation-revocation-probe"),
		"a revoke on one session does not block a different session")
}

// TestAgentIdentityUpdateCredentialNeedsThePlatformLink pins the semantics of
// agentidentity#update_credential — the gate that decides who may replace an
// agent's own SHARED credential when the platform has independently confirmed
// the current value is dead.
//
// The permission is `editor + platform->can_admin` and `editor` ships
// DELIBERATELY UNPOPULATED, so platform->can_admin is the only live arm. That
// arm resolves ONLY through the agentidentity#platform tuple. Drop that one
// relationship and the permission becomes unsatisfiable by anyone — silently:
// the schema still compiles, the credential-update card still publishes, the
// button still renders, and every click is refused. The "linked" vs "unlinked"
// pair below is what makes that tuple provably load-bearing rather than
// incidental; the schema-level half of the same guard the reconciler test
// enforces at the wiring level.
func TestAgentIdentityUpdateCredentialNeedsThePlatformLink(t *testing.T) {
	const (
		identity      = "agentidentity:default/support-bot"
		admin         = "user:platform-admin"
		stranger      = "user:stranger"
		adminGrant    = "platform:platform#admin@" + admin
		platformLink  = identity + "#platform@platform:platform"
		adminViaGroup = "platform:platform#admin@group:ops#member"
	)

	cases := []struct {
		name  string
		rels  []string
		who   string
		grant bool
	}{
		{
			name:  "linked identity + platform admin: admin may replace the dead credential",
			rels:  []string{platformLink, adminGrant},
			who:   admin,
			grant: true,
		},
		{
			// THE load-bearing case. Same admin, same schema; the ONLY
			// difference is the missing agentidentity#platform tuple.
			name:  "admin but NO platform link: refused (the tuple is what makes the permission satisfiable)",
			rels:  []string{adminGrant},
			who:   admin,
			grant: false,
		},
		{
			name:  "linked identity, unrelated user: refused (no over-grant from the link itself)",
			rels:  []string{platformLink, adminGrant},
			who:   stranger,
			grant: false,
		},
		{
			name:  "linked identity, nobody is admin: refused (editor ships unpopulated, so no arm resolves)",
			rels:  []string{platformLink},
			who:   admin,
			grant: false,
		},
		{
			name:  "linked identity + group-based platform admin: a member of the admin group may replace",
			rels:  []string{platformLink, adminViaGroup, "group:ops#member@user:opsmember"},
			who:   "user:opsmember",
			grant: true,
		},
		{
			// editor is unpopulated by any production writer today, but the
			// relation must still WORK — that is the whole reason it ships
			// early (a future per-identity delegation becomes a relationship
			// write, not a schema migration on every live cluster).
			name:  "editor arm works when written directly: no platform link needed",
			rels:  []string{identity + "#editor@user:delegate"},
			who:   "user:delegate",
			grant: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkOnCanonicalSchema(t, tc.rels, identity, "update_credential", tc.who)
			want := v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION
			if tc.grant {
				want = v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
			}
			assert.Equal(t, want, got, "%s#update_credential for %s", identity, tc.who)
		})
	}
}

// TestAgentClassStartExplicitIsStartersOnly pins the two start arms apart. A
// platform admin holds start_session through platform->start_session and
// NOTHING through start_explicit; a listed starter holds both. A class that
// sets platformAdminsMayStart: false checks start_explicit, and this is what
// makes "admins list themselves" true rather than aspirational.
func TestAgentClassStartExplicitIsStartersOnly(t *testing.T) {
	const cls = "agentclass:default/gatebot"
	rels := []string{
		cls + "#platform@platform:platform",
		"platform:platform#admin@user:admin",
		cls + "#starter@user:listed",
		cls + "#starter@group:eng#member",
		"group:eng#member@user:engineer",
	}
	cases := []struct {
		perm, who string
		grant     bool
	}{
		{"start_session", "user:admin", true},
		{"start_explicit", "user:admin", false},
		{"start_session", "user:listed", true},
		{"start_explicit", "user:listed", true},
		{"start_explicit", "user:engineer", true},
		{"start_explicit", "user:stranger", false},
		{"start_session", "user:stranger", false},
	}
	for _, tc := range cases {
		t.Run(tc.perm+" for "+tc.who, func(t *testing.T) {
			got := checkOnCanonicalSchema(t, rels, cls, tc.perm, tc.who)
			want := v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION
			if tc.grant {
				want = v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
			}
			assert.Equal(t, want, got, "%s#%s for %s", cls, tc.perm, tc.who)
		})
	}
}

// TestAgentSessionParticipantAdmitsClassStarters is the onlyStartersInteract
// mechanism: one participant tuple naming the class's starter set makes exactly
// the listed starters interact-holders, and denied still wins.
func TestAgentSessionParticipantAdmitsClassStarters(t *testing.T) {
	const (
		sess = "agentsession:default/s1"
		cls  = "agentclass:default/gatebot"
	)
	rels := []string{
		sess + "#started_by@user:opener",
		sess + "#participant@" + cls + "#starter",
		cls + "#starter@user:listed",
		cls + "#starter@group:eng#member",
		"group:eng#member@user:engineer",
	}
	cases := []struct {
		name  string
		who   string
		extra []string
		grant bool
	}{
		{name: "listed starter: interact granted", who: "user:listed", grant: true},
		{name: "group member: interact granted", who: "user:engineer", grant: true},
		{name: "opener via started_by: interact granted", who: "user:opener", grant: true},
		{name: "stranger: no interact", who: "user:stranger", grant: false},
		{name: "listed starter under denied@class#starter: denied wins", who: "user:listed", extra: []string{sess + "#denied@" + cls + "#starter"}, grant: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkOnCanonicalSchema(t, append(append([]string{}, rels...), tc.extra...), sess, "interact", tc.who)
			want := v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION
			if tc.grant {
				want = v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
			}
			assert.Equal(t, want, got, "%s#interact for %s", sess, tc.who)
		})
	}
}

// TestWorkshopBuildIsTheSessionAlone pins layer 1.3's shape: build resolves
// through the session relation and nothing else — no platform arm, no
// admin arm, nothing transitive.
func TestWorkshopBuildIsTheSessionAlone(t *testing.T) {
	rels := []string{"workshop:ws-a1b2c3d4e5f6#session@agentsession:default/builder-s1"}
	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		checkOnCanonicalSchema(t, rels, "workshop:ws-a1b2c3d4e5f6", "build", "agentsession:default/builder-s1"))
	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
		checkOnCanonicalSchema(t, rels, "workshop:ws-a1b2c3d4e5f6", "build", "agentsession:default/builder-s2"))
}

// TestWorkshopCloseIsStarterOrPlatformAdmin pins the OTHER workshop gate: who
// may end this workshop from another builder. Both arms are live — the person
// who opened it, and a platform admin asked to clear one — and nobody else,
// including a session holding build on the very same workshop. Closing is not
// something a workshop may do to itself sideways.
func TestWorkshopCloseIsStarterOrPlatformAdmin(t *testing.T) {
	const ws = "workshop:ws-a1b2c3d4e5f6"
	rels := []string{
		ws + "#session@agentsession:default/builder-s1",
		ws + "#starter@user:opener",
		ws + "#platform@platform:platform",
		"platform:platform#admin@user:admin",
	}
	cases := []struct {
		name  string
		who   string
		grant bool
	}{
		{name: "the starter: close granted", who: "user:opener", grant: true},
		{name: "a platform admin: close granted", who: "user:admin", grant: true},
		{name: "another person: refused", who: "user:stranger", grant: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION
			if tc.grant {
				want = v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
			}
			assert.Equal(t, want, checkOnCanonicalSchema(t, rels, ws, "close", tc.who), "%s#close for %s", ws, tc.who)
		})
	}
}

// TestWorkshopCloseNeedsThePlatformLink pins that the platform arm resolves
// ONLY through the workshop's own #platform tuple. Omit that one write and an
// admin is refused with the schema still compiling and the admin relation
// still populated — the silent shape agentidentity#platform documents.
func TestWorkshopCloseNeedsThePlatformLink(t *testing.T) {
	const ws = "workshop:ws-b2c3d4e5f6a1"
	rels := []string{
		ws + "#session@agentsession:default/builder-s1",
		ws + "#starter@user:opener",
		"platform:platform#admin@user:admin",
	}
	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
		checkOnCanonicalSchema(t, rels, ws, "close", "user:admin"),
		"without workshop#platform the admin arm is unreachable")
}
