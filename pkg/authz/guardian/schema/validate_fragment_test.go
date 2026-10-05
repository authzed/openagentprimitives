package schema_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
)

// TestReservedDefinitionNames_MatchesScaffold pins the parsed reserved
// set against the known base-scaffold definitions (pkg/authz/spicedb/schema/
// schema.zed) so an accidental rename in the scaffold surfaces as a
// normal test failure here rather than silently changing which
// definition names MCPServer fragments are forbidden from redeclaring.
func TestReservedDefinitionNames_MatchesScaffold(t *testing.T) {
	want := map[string]struct{}{
		"user": {},
		// string is the relationship-set hash sentinel's subject type
		// (pkg/authz/spicedb/schema/schema.zed) — a sync source stores a
		// scope's content hash as slack_channel:C3#relhash@string:<sha256>.
		// Reserved so no kind fragment can redeclare it: two fragments (or a
		// fragment and the scaffold) both declaring `string` is a composition
		// conflict the fragment validator rejects, and every kind that wants a
		// hash sentinel needs the SAME `string` to reference.
		"string": {},
		"group":  {},
		// onepassword_group is the synced 1Password directory `group#member`
		// unions. It is in the SCAFFOLD, not in the sync kind's fragment,
		// because group#member names it: SpiceDB resolves every allowed
		// subject type at WriteSchema, so a scaffold naming a type only a
		// fragment supplies fails every bare write (install bootstrap,
		// apply-schema, testspicedb). Being reserved follows from that — a
		// fragment redeclaring it would collide with the scaffold's own copy.
		"onepassword_group": {},
		// github_user binds a GitHub account to a platform user. It moved here
		// from the github channel kind's fragment because three independent
		// fragments now name it (the kind's session links, the gh toolkit's
		// repo roles, the directory sync's writes) and only one may declare it
		// — same reason string and onepassword_group are reserved.
		"github_user":       {},
		"agentsession":      {},
		"memory_entry":      {},
		"artifact":          {},
		"infoleakage_grant": {},
		"platform":          {},
		"cluster":           {},
		"externaltoken":     {},
		"agentidentity":     {},
		// agentclass joined the scaffold with the browser start gate. Being
		// reserved is the point: an MCPServer fragment that redeclared it would
		// silently replace the definition the start gate resolves through, and
		// every agent picker on the cluster would answer differently.
		"agentclass": {},
		// service is the non-human principal a webhook-triggered session acts
		// as. It carries no relations by design, but being reserved still
		// matters: a fragment that redeclared it — with relations — would give
		// a subject the base schema deliberately grants nothing a way to hold
		// permissions, and every service-subject check on the cluster would
		// start answering differently.
		"service": {},
		// pt_tag is the provenance lattice — one datum's derivation tree and
		// the audience allowed to see it. Reservation matters more here than
		// almost anywhere: a fragment that redeclared it could drop the
		// `.all()` on `reader`, turning the confidentiality intersection into
		// a union. Every disclosure check on the cluster would then approve a
		// payload assembled from sources with no reader in common, which is
		// the exact laundering this definition exists to prevent — and the
		// schema would still compile.
		"pt_tag": {},
		// workshop is the lockdown layer 1.3 boundary: a builder session's
		// build space, checked FullyConsistent at every privileged act so that
		// even a misconfigured RBAC layer cannot admit a session the tuple does
		// not name. Reservation matters here for the same reason as everywhere
		// else in this list: an MCPServer fragment that redeclared it could
		// widen `build` past the bound session, silently defeating the one
		// property this definition exists to guarantee.
		"workshop": {},
		// agent is the sentinel bot/agent principal referenced by connector
		// fragments (Slack's slack_user#user and slack_bot#agent). It moved
		// here from the Slack fragment so any connector fragment can reference
		// it without depending on Slack being installed; being reserved is
		// what stops a fragment from redeclaring it once it moved.
		"agent": {},
		// A private goal domain is platform-owned; fragments cannot redefine it.
		"agent_goal_domain":    {},
		"agent_goal_execution": {},
	}
	assert.Equal(t, want, schema.ReservedDefinitionNames(), "ReservedDefinitionNames must track the scaffold's definitions exactly")
}

// TestReservedDefinitionNames_IncludesAgent pins agent's reserved status on
// its own: TestReservedDefinitionNames_MatchesScaffold already covers it as
// part of the exact-set comparison, but a set comparison failure can point
// at the wrong culprit when several names are wrong at once. This assertion
// fails unambiguously if agent stops being reserved — e.g. if the scaffold's
// declaration were ever reverted without updating this file.
func TestReservedDefinitionNames_IncludesAgent(t *testing.T) {
	assert.Contains(t, schema.ReservedDefinitionNames(), "agent")
}

// TestValidateFragment_Nil verifies a nil fragment is valid (nothing to
// check) — mirrors the controller's "MCPServer with no spiceDBSchema"
// case, which must not be treated as a bad fragment.
func TestValidateFragment_Nil(t *testing.T) {
	assert.NoError(t, schema.ValidateFragment(nil))
}

// TestValidateFragment_Valid verifies an ordinary, well-formed fragment
// (a new resource type with no name collision) passes.
func TestValidateFragment_Valid(t *testing.T) {
	frag := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{{
			Name: "github_repo",
			Relations: []spiceboxv1alpha1.SpiceDBRelation{
				{Name: "reader", SubjectType: "user"},
			},
			Permissions: []spiceboxv1alpha1.SpiceDBPermission{
				{Name: "read", Expr: "reader"},
			},
		}},
	}
	assert.NoError(t, schema.ValidateFragment(frag), "well-formed fragment should validate")
}

// TestValidateFragment_ReservedResourceName verifies a fragment whose
// structured Resources[].Name collides with a reserved scaffold
// definition (agentsession) is rejected with an error naming the
// offending definition.
func TestValidateFragment_ReservedResourceName(t *testing.T) {
	frag := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{{
			Name: "agentsession",
			Permissions: []spiceboxv1alpha1.SpiceDBPermission{
				{Name: "evil", Expr: "nil"},
			},
		}},
	}
	err := schema.ValidateFragment(frag)
	require.Error(t, err, "reserved-name collision must error")
	assert.Contains(t, err.Error(), "agentsession", "error names the offending definition")
}

// TestValidateFragment_UserResource verifies the historical "cannot
// redeclare implicit user" rule is now subsumed by the general
// reserved-name check.
func TestValidateFragment_UserResource(t *testing.T) {
	frag := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{{Name: "user"}},
	}
	err := schema.ValidateFragment(frag)
	require.Error(t, err, "resource named \"user\" must error")
	assert.Contains(t, err.Error(), "user", "error names the offending definition")
}

// TestValidateFragment_ReservedNameViaRawZed verifies a RawZed fragment
// that redefines a reserved scaffold definition (agentsession, this
// time expressed as raw DSL rather than a structured Resource) is also
// rejected — the regex scan over RawZed must catch it, not just the
// structured Resources path.
func TestValidateFragment_ReservedNameViaRawZed(t *testing.T) {
	frag := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: "definition agentsession {\n  permission evil = nil\n}\n",
	}
	err := schema.ValidateFragment(frag)
	require.Error(t, err, "rawZed reserved-name collision must error")
	assert.Contains(t, err.Error(), "agentsession", "error names the offending definition")
}

// TestValidateFragment_MalformedRawZed verifies a RawZed fragment with
// invalid SpiceDB schema DSL syntax fails at the compose+compile step —
// the same class of error a live WriteSchema call would reject, caught
// here before the fragment ever reaches RunAll.
func TestValidateFragment_MalformedRawZed(t *testing.T) {
	frag := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: "definition broken_thing {\n  relation foo user\n}\n", // missing ':' — invalid syntax
	}
	err := schema.ValidateFragment(frag)
	require.Error(t, err, "malformed RawZed must error")
}

// A fragment whose permission names a relation the definition does not declare
// must be REJECTED here — not accepted and left to be refused by SpiceDB at
// WriteSchema, once, cluster-wide, after every fragment has been merged.
//
// compiler.Compile is a parse: it builds `read = session->interact + owner`
// into an expression tree and never asks whether `owner` is declared. So a
// tenant fragment with a self-contained dangling reference passed
// ValidateFragment, passed the partition, and reached RunAll, where the check
// was report-only — then the real WriteSchema rejected the merged schema and
// froze every AgentSessionGrants in the cluster. Isolating it here costs the
// one offending CR's validity instead.
//
// The `session->interact` arrow in the same expression resolves cleanly against
// the scaffold's agentsession#interact, so this pins that only the genuinely
// dangling LOCAL name is what trips it.
func TestValidateFragment_RejectsADanglingReference(t *testing.T) {
	dangling := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: `
definition wiring_probe {
    relation session: agentsession
    permission read = session->interact + owner
}
`,
	}
	err := schema.ValidateFragment(dangling)
	require.Error(t, err, "a self-contained dangling reference must fail validation, not reach WriteSchema")
	assert.Contains(t, err.Error(), "owner", "the error names the reference that resolves to nothing")
}

// The clean twin — same fragment minus the dangling `+ owner` — must still
// validate, or the check is an outage. Its cross-scaffold arrow
// (session->interact) resolves, and a fragment that arrows onto a type NOT
// present at validation time is skipped by the resolver rather than flagged, so
// this is not a wall against legitimate cross-fragment references.
func TestValidateFragment_AcceptsAResolvableReference(t *testing.T) {
	clean := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: `
definition wiring_probe {
    relation session: agentsession
    permission read = session->interact
}
`,
	}
	assert.NoError(t, schema.ValidateFragment(clean), "a fragment whose references all resolve must validate")
}

// ValidateFragment composes ONE fragment over the scaffold — no channel-kind
// baseline, no sibling fragment — so a relation typed onto a channel kind's
// definition looks undeclared here and is declared at the real compose. The
// resolver reports it (that half is what caught `group#member` naming an
// undeclared onepassword_group in the scaffold); this caller drops it by its
// SubjectType marker, because refusing would cost a valid MCPServer its
// availability over a type github's own fragment declares.
//
// `github_user` is the live instance: pkg/channels/channelkinds/github/
// schema_fragment.go declares it, and pkg/authz/guardian/schema/
// github_user_inert_test.go builds exactly this fragment shape.
func TestValidateFragment_AcceptsASubjectTypeASiblingFragmentDeclares(t *testing.T) {
	crossFragment := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: `
definition wiring_probe {
    relation direct_member: github_user#user
    permission members = direct_member
}
`,
	}
	assert.NoError(t, schema.ValidateFragment(crossFragment),
		"a subject type a channel-kind fragment declares must not be refused from the scaffold-only view")
}
