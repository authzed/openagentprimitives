package relwrites_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
)

// The only structural check on a resolved tuple was that both strings contain
// a colon. Everything else — which TYPE is written, which relation, which
// subject — came from CEL over `args` (model-authored) and `result` (the
// upstream MCP server's response), and the writer touched whatever it was
// handed using the preshared token mounted into every runner pod.
//
// Every escalation target is schema-valid, so SpiceDB refuses none of them:
// agentsession#owner, agentsession#parent, pt_tag#direct_reader, an uncaveated
// externaltoken grant, and platform#admin.
//
// The guardian already refuses a tenant FRAGMENT that redeclares a scaffold
// definition; nothing applied the same rule to the tuples a tool writes. This
// is that rule, at the point the tuple is resolved.
func TestReservedResourceTypesAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tuple relwrites.ResolvedTuple
	}{
		{name: "platform admin — the whole cluster",
			tuple: relwrites.ResolvedTuple{Resource: "platform:cluster", Relation: "admin", Subject: "user:attacker"}},
		{name: "session owner — approve, hold, manage_scope, fork",
			tuple: relwrites.ResolvedTuple{Resource: "agentsession:victim", Relation: "owner", Subject: "user:attacker"}},
		{name: "delegation lineage",
			tuple: relwrites.ResolvedTuple{Resource: "agentsession:child", Relation: "parent", Subject: "agentsession:attacker"}},
		{name: "memory entry",
			tuple: relwrites.ResolvedTuple{Resource: "memory_entry:e1", Relation: "reader", Subject: "user:attacker"}},
		{name: "artifact",
			tuple: relwrites.ResolvedTuple{Resource: "artifact:a1", Relation: "viewer", Subject: "user:attacker"}},
		{name: "reserved type as the SUBJECT, not just the resource",
			tuple: relwrites.ResolvedTuple{Resource: "crm_company:c1", Relation: "owner", Subject: "platform:cluster"}},
		{name: "granting a HUMAN debug on a cluster — the resource rule alone would allow this",
			tuple: relwrites.ResolvedTuple{Resource: "cluster:prod", Relation: "debugger", Subject: "user:attacker"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := relwrites.ValidateResolvedTuple(tc.tuple)
			require.Error(t, err, "a tool must not write a tuple naming a code-owned scaffold type")
			assert.Contains(t, err.Error(), "reserved")
		})
	}
}

// The feature still has to work: a tenant's OWN resource types are exactly
// what writesRelationships exists to maintain.
func TestTenantOwnedResourceTypesAreAllowed(t *testing.T) {
	for _, tuple := range []relwrites.ResolvedTuple{
		{Resource: "crm_company:acme", Relation: "hubspot_owner_id_ref", Subject: "hubspot_owner:42"},
		{Resource: "hubspot_owner:42", Relation: "user", Subject: "user:sales@corp"},
		{Resource: "pde_board:board-team", Relation: "viewer", Subject: "user:teammate"},
		// The shipped cluster-pinning flow: a sandbox toolspec binds the
		// session to the cluster it is debugging. The subject is the session,
		// not a human, so no authority is handed to anyone.
		{Resource: "cluster:prod-example-1", Relation: "debug_target", Subject: "agentsession:default/sre-1"},
	} {
		assert.NoError(t, relwrites.ValidateResolvedTuple(tuple),
			"a tenant's own type must stay writable: %s", tuple.Resource)
	}
}

// A malformed tuple is still refused, and the reserved-type rule must not
// replace that check.
func TestMalformedTuplesAreStillRefused(t *testing.T) {
	assert.Error(t, relwrites.ValidateResolvedTuple(
		relwrites.ResolvedTuple{Resource: "no-colon", Relation: "owner", Subject: "user:a"}))
	assert.Error(t, relwrites.ValidateResolvedTuple(
		relwrites.ResolvedTuple{Resource: "crm_company:acme", Relation: "owner", Subject: "no-colon"}))
}

// The resource rule is an ALLOWLIST over a scaffold set DERIVED from
// schema.zed rather than transcribed. That combination is the point: a
// definition added to the scaffold later is refused by default instead of
// silently becoming writable by every tool.
//
// `service` is a scaffold definition with no entry in
// writableScaffoldResourceTypes, which is exactly the shape a newly-added
// definition would have — so this pins the DEFAULT, not any one type.
func TestUnlistedScaffoldTypesAreRefusedByDefault(t *testing.T) {
	err := relwrites.ValidateResolvedTuple(relwrites.ResolvedTuple{
		Resource: "service:some-service", Relation: "member", Subject: "user:a",
	})
	require.Error(t, err,
		"a scaffold type nobody added to the writable allowlist must be refused, so a new authorization object is protected the day it is declared")
}

// And the allowlist is real: the one type on it stays writable, or the rule
// would have broken the shipped cluster-pinning flow.
func TestTheWritableScaffoldTypeStaysWritable(t *testing.T) {
	require.NoError(t, relwrites.ValidateResolvedTuple(relwrites.ResolvedTuple{
		Resource: "cluster:prod-example-1", Relation: "debug_target", Subject: "agentsession:default/sre-1",
	}))
}

// github_user is a scaffold definition (attested_identity.go's
// TouchAttestedIdentity writes github_user:<id>#user@user:<canonical>), but
// the PR-identity write — the one flow that names this type as a subject —
// needs to name the account itself as a tuple's subject on a tenant-owned
// resource: recording who opened a pull request. That is safe to allow: a
// github_user object confers read only through #sole_user, which the
// useridentity controller writes for an exclusive attested claim, never a
// tool.
func TestAttestedForgeIdentityAllowedAsTupleSubject(t *testing.T) {
	assert.NoError(t, relwrites.ValidateResolvedTuple(relwrites.ResolvedTuple{
		Resource: "github_pull_request:PR_kwABC", Relation: "author", Subject: "github_user:123456",
	}), "an attested forge identity must be nameable as a tuple subject on a tenant-owned resource")
}

// github_user denotes a person TRANSITIVELY — naming one as a subject hands
// authority to whoever holds #sole_user on it. The principal rule must treat
// it exactly like user/group: a writable scaffold RESOURCE still may not hand
// it a relation.
func TestAttestedForgeIdentityRefusedAsPrincipalOnScaffoldResource(t *testing.T) {
	err := relwrites.ValidateResolvedTuple(relwrites.ResolvedTuple{
		Resource: "cluster:prod-example-1", Relation: "debugger", Subject: "github_user:123456",
	})
	require.Error(t, err, "an attested forge identity must not be handed authority over a platform object")
	assert.Contains(t, err.Error(), "a tool may not hand a principal authority over a platform object")
}

// slot_pin and slot_grant_* are platform slot mechanisms that must not be
// writable by tools. slot_pin is the single-occupancy commitment the pinning
// gate writes with an atomic precondition; slot_grant_* are the session's slot
// grants. A toolspec writing either would bypass the gate that makes them mean
// anything.
func TestValidateResolvedTuple_RefusesPlatformSlotRelations(t *testing.T) {
	cases := []struct {
		name     string
		relation string
	}{
		{"slot_pin is the pinning mechanism's own relation", "slot_pin"},
		{"slot_grant_push is a grant relation", "slot_grant_push"},
		{"slot_grant_read is a grant relation", "slot_grant_read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := relwrites.ValidateResolvedTuple(relwrites.ResolvedTuple{
				Resource: "git_repo:acme/widgets", Relation: tc.relation,
				Subject: "agentsession:ns/s",
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.relation)
		})
	}
}
