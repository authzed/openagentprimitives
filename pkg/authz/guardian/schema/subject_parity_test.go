package schema_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	authzschema "github.com/authzed/openagentprimitives/pkg/authz/spicedb/schema"
)

// baseSchemaWithDenied mirrors the production agentsession block closely enough
// to exercise subject-type parity: two granting relations and one subtracting
// one, all subject-bearing.
const baseSchemaWithDenied = `
use expiration

caveat check_hash(arguments_hash string, allowed_arguments_hash string) {
    arguments_hash == allowed_arguments_hash
}

definition user {}

definition group {
    relation member: user
    permission membership = member
}

definition slack_channel {
    relation member: user
}

definition slack_usergroup {
    relation member: user
}

definition agentsession {
    relation started_by: user
    relation owner: user | group#member
    relation participant: user | group#member
    relation denied: user | group#member
    permission interact = owner + participant - denied
}
`

// subjectTypesOf extracts the subject-type entries from a `relation X: A | B`
// line in the composed agentsession block.
func subjectTypesOf(t *testing.T, schemaText, relation string) []string {
	t.Helper()
	for _, line := range strings.Split(schemaText, "\n") {
		trimmed := strings.TrimSpace(line)
		prefix := "relation " + relation + ":"
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		var out []string
		for _, part := range strings.Split(strings.TrimPrefix(trimmed, prefix), "|") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	t.Fatalf("relation %q not found in composed schema", relation)
	return nil
}

// A registered channel subject type flows into the GRANTING relations
// automatically. It must flow into the SUBTRACTING one too.
//
// The subtrahend has to be able to name anything the minuend can name, or the
// subtraction is structurally incapable of undoing some grants. Concretely:
// slack_channel#member lands in owner and participant, one tuple can therefore
// mean an entire channel, and if denied cannot hold that same subject type
// there is no way to rescind it — you can only deny members one at a time,
// which does not terminate for a channel that keeps growing.
//
// This is asserted as PARITY rather than as a literal expected string so it
// keeps holding for the next channel kind someone registers.
func TestCompose_DeniedAcceptsEverySubjectTypeOwnerAndParticipantDo(t *testing.T) {
	links := []string{"slack_channel#member", "slack_usergroup#member"}
	out, _, _, err := schema.ComposeWithSkipped(baseSchemaWithDenied, nil, links...)
	require.NoError(t, err)

	denied := subjectTypesOf(t, out, "denied")
	for _, rel := range []string{"owner", "participant"} {
		for _, subj := range subjectTypesOf(t, out, rel) {
			assert.Containsf(t, denied, subj,
				"subject type %q is grantable via %q but not deniable; "+
					"a grant that cannot be rescinded is a one-way door", subj, rel)
		}
	}
}

// The parity has to hold for the schema we actually ship, not only for a
// hand-written fixture. This reads the embedded production schema so a future
// edit that adds a subject type to owner or participant alone fails here.
func TestProductionSchema_DeniedHasSubjectParity(t *testing.T) {
	denied := subjectTypesOf(t, authzschema.Schema, "denied")
	for _, rel := range []string{"owner", "participant"} {
		for _, subj := range subjectTypesOf(t, authzschema.Schema, rel) {
			assert.Containsf(t, denied, subj,
				"pkg/authz/schema/schema.zed: %q is accepted by %q but not by denied", subj, rel)
		}
	}
}
