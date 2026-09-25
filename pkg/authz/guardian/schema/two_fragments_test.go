package schema_test

// TWO MCPServers, each declaring its own resource.
//
// Every fixture in the e2e suite has at most ONE MCPServer, so an AgentClass
// whose second server declares a schema its own readwrite tool checks against
// is an entirely untested shape. A fixture built that way failed with
// "SpiceDB schema has no compose on summary_doc" at any wait length — this
// asks whether the composer is the reason.

import (
	"testing"

	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
)

func fragmentWith(resource, relation, permission string) *spiceboxv1alpha1.SpiceDBSchemaFragment {
	return &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{{
			Name:        resource,
			Relations:   []spiceboxv1alpha1.SpiceDBRelation{{Name: relation, SubjectType: "user"}},
			Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: permission, Expr: relation}},
		}},
	}
}

// TestComposeAll_TwoServersEachDeclaringOneResource.
//
// Both must land. If the composer dropped the second, an AgentClass could
// never reference two MCPServers where the second one's tools check against
// its own schema — and that would be a product gap, not a fixture problem.
func TestComposeAll_TwoServersEachDeclaringOneResource(t *testing.T) {
	notes := fragmentWith("flagged_note", "viewer", "view")
	writer := fragmentWith("summary_doc", "author", "compose")

	got, err := schema.ComposeAll(
		identify(notes, writer), nil, nil)
	require.NoError(t, err, "ComposeAll over two fragments")

	assertContainsAll(t, got, []string{
		"definition flagged_note {",
		"permission view = viewer",
		"definition summary_doc {",
		"permission compose = author",
	}, "both servers' resources")
}

// TestComposeAll_OrderDoesNotDecideWhichSurvives — the same two fragments the
// other way round. A composer that kept only the first would pass the test
// above and fail this one.
func TestComposeAll_OrderDoesNotDecideWhichSurvives(t *testing.T) {
	notes := fragmentWith("flagged_note", "viewer", "view")
	writer := fragmentWith("summary_doc", "author", "compose")

	got, err := schema.ComposeAll(
		identify(writer, notes), nil, nil)
	require.NoError(t, err)

	assertContainsAll(t, got, []string{
		"definition flagged_note {",
		"definition summary_doc {",
	}, "both, whichever order they arrive in")
}

// TestComposeAll_TheSameResourceDeclaredTwiceIdentically.
//
// The shape a fixture reaches for when a second server must declare a resource
// the first already declares: the two bodies are byte-identical. The
// controller's N-way isolation rejects fragments that CONFLICT, and this asks
// whether identical is treated as conflicting — which would make "declare it
// on both" (the obvious workaround) quietly drop one server's whole fragment.
func TestComposeAll_TheSameResourceDeclaredTwiceIdentically(t *testing.T) {
	a := fragmentWith("summary_doc", "author", "compose")
	b := fragmentWith("summary_doc", "author", "compose")

	_, err := schema.ComposeAll(identify(a, b), nil, nil)

	// Whichever way this goes, it is worth pinning: a duplicate that composes
	// makes the workaround sound, and one that errors makes it a trap the
	// controller has to catch before RunAll.
	if err != nil {
		t.Logf("identical duplicate resource declarations FAIL to compose: %v", err)
		return
	}
	t.Log("identical duplicate resource declarations compose cleanly")
}
