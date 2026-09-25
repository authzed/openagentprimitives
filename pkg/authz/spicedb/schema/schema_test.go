package schema_test

import (
	"strings"
	"testing"

	"github.com/authzed/spicedb/pkg/schemadsl/compiler"
	"github.com/authzed/spicedb/pkg/schemadsl/input"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authzschema "github.com/authzed/openagentprimitives/pkg/authz/spicedb/schema"
)

// TestSchemaCompiles is the guard that the single canonical schema is a valid,
// standalone SpiceDB schema. Every consumer (oap install, the operator's
// composer, test fixtures) writes it, so a broken edit here would break all of
// them at once — catching it as a fast unit test is the point of consolidating.
func TestSchemaCompiles(t *testing.T) {
	_, err := compiler.Compile(compiler.InputSchema{
		Source:       input.Source("authz-schema"),
		SchemaString: authzschema.Schema,
	}, compiler.AllowUnprefixedObjectType())
	require.NoError(t, err, "canonical schema must compile standalone")
}

// TestSchemaHasCriticalDefinitions pins the definitions/permissions whose
// absence has caused (artifact) or silently risked (is_denied, memory_entry)
// production bugs via schema drift. If a refactor drops one, fail loudly here.
func TestSchemaHasCriticalDefinitions(t *testing.T) {
	for _, want := range []string{
		"definition user {}",
		"definition group {",
		"definition agentsession {",
		"relation owner: user | group#member",
		"permission interact = owner + started_by + participant - denied",
		"permission is_denied = denied",
		"permission manage_scope = owner",
		"permission fork = owner",
		"permission approve = owner",
		// The info-leakage floor audience for undeclared-tool data. Dropping it
		// makes handleUnmappedTool's floor taint resolve to nobody, silently
		// turning every undeclared tool into an approval prompt (or, worse, a
		// vacuous allow) — the opposite of the "no more restrictive than coarse"
		// invariant it exists to hold.
		"permission unknown_provenance = interact",
		"definition memory_entry {",
		"definition artifact {",
		"relation platform: platform",
		"permission view = parent->interact + parent->artifact_org_view + platform->view_audit",
		// The org-wide arm's deny subtraction. Dropping "- denied" still
		// compiles and still passes every all-arms-open check; what it breaks
		// is the Deny button, which must beat the opt-in wildcard too.
		"permission artifact_org_view = artifact_org_viewer - denied",
		"definition infoleakage_grant {",
		// The provenance lattice. The two arrows are different operators on
		// purpose — `.all()` is the confidentiality INTERSECTION, `->` the
		// integrity union — and swapping either for the other still compiles,
		// still passes every text-free test, and silently turns the
		// disclosure gate into a laundering primitive. Pinned as text because
		// that substitution is the one a well-meaning simplification makes.
		"definition pt_tag {",
		"permission reader = direct_reader + derived_from.all(reader)",
		"permission carries_untrusted = untrusted_origin + derived_from->carries_untrusted",
		"permission install_agent = can_admin",
		// The session-start gate. agentclass#start_session is what the gate
		// actually checks; platform#start_session is the arm it reaches
		// through. Drop EITHER and the picker is empty on every cluster with
		// no sessions yet, silently — the schema still compiles.
		"permission start_session = can_admin",
		"definition agentclass {",
		"permission start_session = starter + platform->start_session",
		"caveat check_hash",
		"caveat infoleakage_resource_match",
		"definition externaltoken {}",
		"caveat token_value_matches",
		// github_user binds a GitHub account to a platform user; sole_user is
		// written only while exactly one platform subject claims that account,
		// and is what repository roles will traverse (agentsession membership
		// keeps #user). Dropping either silently changes which claimants a
		// forge credential link can confer standing through.
		"definition github_user {",
		"relation sole_user: user",
		"relation authorized_token: externaltoken | externaltoken with token_value_matches",
		"permission use_token = authorized_token",
		"definition agentidentity {",
		// The gate for replacing an agent's own shared credential. Its only
		// live arm is platform->can_admin (editor ships unpopulated), which
		// resolves solely through the agentidentity#platform tuple written by
		// the AgentIdentity reconciler — drop either half and every
		// credential-update card is silently unactionable.
		"permission update_credential = editor + platform->can_admin",
		// The workshop boundary, both halves. build is the layer-1.3 gate on
		// acting AS a workshop and must stay the bound session alone. close is
		// the gate on ending SOMEONE ELSE'S workshop from another builder:
		// drop `starter` and the person who opened it can no longer free their
		// own slot, drop `platform->can_admin` and no admin can clear a stuck one.
		// Either loss still compiles and still passes every text-free test.
		"definition workshop {",
		"permission build = session",
		"permission close = starter + platform->can_admin",
	} {
		require.Contains(t, authzschema.Schema, want, "canonical schema missing %q", want)
	}
}

// TestSchema_DeclaresTheAgentSentinel pins the agent sentinel to the
// scaffold. agent is referenced by connector fragments that have nothing to
// do with any one channel kind (Slack's slack_user#user and slack_bot#agent
// today; a future connector's own bot/agent principal tomorrow). Declaring
// it in the scaffold is what lets a connector fragment reference it without
// depending on another kind being installed.
func TestSchema_DeclaresTheAgentSentinel(t *testing.T) {
	assert.Contains(t, authzschema.Schema, "definition agent {}")
}

// TestAgentSessionInteractSubjectsAreClosed pins the exact subject type sets
// of agentsession's started_by and participant relations — the two relations
// that (with owner) feed #interact.
//
// pipeline.go:547 skips the SpiceDB #interact check entirely for
// service-subject inbound (a webhook has no per-message human identity to
// check against). That skip is safe ONLY because no relation feeding
// #interact can ever admit a "service" subject; if one could, the skip would
// silently become a real authorization bypass for every service-subject
// channel (GitHub App installs included).
//
// A plain substring check (require.Contains, as TestSchemaHasCriticalDefinitions
// uses for everything else) would NOT catch the drift this guards against:
// appending "| service" to either relation's type union — the natural way to
// widen it — leaves the original text sitting there as an unbroken prefix, so
// Contains would keep passing. requireExactRelationLine instead matches the
// COMPLETE, trimmed relation declaration line, so any change to the type
// union — append or replace — fails loudly.
func TestAgentSessionInteractSubjectsAreClosed(t *testing.T) {
	requireExactRelationLine(t, authzschema.Schema, "relation started_by: user")
	requireExactRelationLine(t, authzschema.Schema, "relation participant: user | group#member | agentclass#starter")
}

// TestAgentClassInteractorRelationAndPersonalizePermission pins the
// interactor relation and can_personalize permission on agentclass, which are
// the contract for enumerating classes a user has interacted with (Slack App
// Home preferences pane).
func TestAgentClassInteractorRelationAndPersonalizePermission(t *testing.T) {
	assert.Contains(t, authzschema.Schema, "relation interactor: user")
	assert.Contains(t, authzschema.Schema, "permission can_personalize = interactor")
}

// requireExactRelationLine requires that schema contains a line — after
// trimming leading/trailing whitespace — that equals want exactly. Unlike
// require.Contains, this fails on an append (e.g. "user" widened to
// "user | service") as well as a removal or rename, because the comparison
// is against the whole line, not a substring of it.
func requireExactRelationLine(t *testing.T, schema, want string) {
	t.Helper()
	for _, line := range strings.Split(schema, "\n") {
		if strings.TrimSpace(line) == want {
			return
		}
	}
	t.Fatalf("canonical schema has no line exactly matching %q (a substring match would not catch an appended subject type)", want)
}
