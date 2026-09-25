package schema_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	authzschema "github.com/authzed/openagentprimitives/pkg/authz/spicedb/schema"
)

// The base vocabulary belongs to the SCAFFOLD, not to any channel kind: `user`
// and `group` are declared in schema.zed and are admissible with no kind
// registered at all. A derivation that only asked the kinds would silently drop
// them and refuse every ordinary group policy.
func TestSubjectTypesFor_ScaffoldBaseVocabularyWithNoLinks(t *testing.T) {
	types, err := schema.SubjectTypesFor(authzschema.Schema, "participant")
	require.NoError(t, err)

	assert.Contains(t, types, "user", "the scaffold's own participant vocabulary")
	assert.Contains(t, types, "group")
	assert.NotContains(t, types, "slack_channel",
		"a channel-kind type is admissible only once that kind is registered and its link supplied")
}

// A kind's link type joins the admissible set, exactly as the composer unions it
// into the live relation line.
func TestSubjectTypesFor_UnionsChannelKindLinks(t *testing.T) {
	types, err := schema.SubjectTypesFor(authzschema.Schema, "participant",
		"slack_channel#member", "slack_usergroup#member")
	require.NoError(t, err)

	assert.Contains(t, types, "slack_channel")
	assert.Contains(t, types, "slack_usergroup")
	assert.Contains(t, types, "user", "the base vocabulary survives the union")
	assert.Contains(t, types, "group")
}

// The answer must be the composed relation line's, not a parallel opinion about
// it: every type reported has to appear in the text ComposeWithSkipped emits for
// that relation, and every type in that text has to be reported.
func TestSubjectTypesFor_AgreesWithTheComposedRelationLine(t *testing.T) {
	links := []string{"slack_channel#member", "slack_usergroup#member"}
	// The composer refuses a link whose object type is undefined, so the
	// definitions a channel kind contributes via SchemaContributor stand in
	// here. Both calls read the same text, which is the point of the comparison.
	scaffold := authzschema.Schema + `
definition slack_channel {
    relation member: user
}

definition slack_usergroup {
    relation member: user
}
`
	types, err := schema.SubjectTypesFor(scaffold, "participant", links...)
	require.NoError(t, err)

	composed, _, _, cerr := schema.ComposeWithSkipped(scaffold, nil, links...)
	require.NoError(t, cerr)

	line := ""
	for _, l := range strings.Split(composed, "\n") {
		if trimmed := strings.TrimSpace(l); strings.HasPrefix(trimmed, "relation participant:") {
			line = trimmed
			break
		}
	}
	require.NotEmpty(t, line, "composed schema must declare a participant relation")

	for _, typ := range types {
		assert.Containsf(t, line, typ, "reported type %q is absent from the composed line %q", typ, line)
	}
	for _, part := range strings.Split(strings.TrimPrefix(line, "relation participant:"), "|") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		typ, _, _ := strings.Cut(part, "#")
		assert.Containsf(t, types, typ, "composed line admits %q but it was not reported", typ)
	}
}

// Fail-closed on a question the scaffold cannot answer. Returning "just the
// links" for an unknown relation would drop the base vocabulary; returning the
// links for a relation the composer never unions them into would claim an
// admissibility the live schema does not have. Both are refusals, not defaults.
func TestSubjectTypesFor_RefusesRelationsItCannotAnswer(t *testing.T) {
	cases := []struct {
		name     string
		relation string
	}{
		{name: "relation absent from the scaffold: error, not an empty set", relation: "nosuchrelation"},
		{name: "relation that holds no subjects: error, since links are never unioned into it", relation: "started_by"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := schema.SubjectTypesFor(authzschema.Schema, tc.relation, "slack_channel#member")
			require.Error(t, err)
		})
	}
}
