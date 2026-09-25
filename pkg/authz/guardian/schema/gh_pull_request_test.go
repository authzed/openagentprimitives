package schema_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/toolkits"
)

// composeToolkitFragment composes the gh toolkit's OWN rawZed fragment
// through the same base-scaffold compose pass a real cluster runs
// (ComposeBase), scoped to just this one toolkit rather than
// toolkits.All() (which shippedSchema, in gh_slot_composition_test.go, uses)
// — a defect this brief cares about is a property of gh.yaml meeting the
// scaffold, not of gh.yaml meeting every other built-in toolkit at once.
func composeToolkitFragment(t *testing.T) string {
	t.Helper()

	var gh toolkit.Toolkit
	found := false
	for _, tk := range toolkits.All() {
		if tk.Name == "gh" {
			gh = tk
			found = true
			break
		}
	}
	require.True(t, found, "the gh toolkit must be registered as a built-in")

	out, err := schema.ComposeBase(schema.ToolkitFragments([]toolkit.Toolkit{gh}))
	require.NoError(t, err, "the gh toolkit's fragment must compose against the base scaffold on its own")
	return out
}

// ghPullRequestFragment returns github_pull_request's definition text as a
// standalone fragment — exactly the shape ComposeSlots' composeOneSlot
// operates on (definitionBlockBounds slices one definition block out of a
// larger schema; a lone block compiles fine on its own even though it
// references external subject types, because compiler.Compile here is a
// parse, not a type-check — see definitionDeclaresName's doc in
// references.go). Kept as a Go string rather than read out of gh.yaml so
// this test does not depend on the toolkit loader to exercise ComposeSlots.
func ghPullRequestFragment(t *testing.T) string {
	t.Helper()
	return `definition github_pull_request {
    relation repo: github_repo
    relation author: github_user

    permission view_memory = author->sole_user + repo->can_admin
}`
}

// The audience expression survives composition. A slot may not be declared
// on view_memory, and this is what proves the refusal protects a REAL
// definition rather than only a fixture.
func TestGhToolkit_PullRequestAudienceSurvivesComposition(t *testing.T) {
	composed := composeToolkitFragment(t) // the gh toolkit's rawZed through the composer

	assert.Contains(t, composed, "definition github_pull_request")
	assert.Contains(t, composed, "permission view_memory = author->sole_user + repo->can_admin",
		"the composer must not own this line; a slot on it is refused precisely so it cannot")
}

// A slot on write_memory composes, creating the permission the fragment
// deliberately does not declare.
func TestGhToolkit_PullRequestWriteMemoryIsCreatedBySlotComposition(t *testing.T) {
	composed, changed, skipped, err := schema.ComposeSlots(ghPullRequestFragment(t),
		[]schema.SlotPair{{ResourceType: "github_pull_request", Permission: "write_memory"}})

	require.NoError(t, err)
	assert.True(t, changed)
	assert.Empty(t, skipped)
	assert.Contains(t, composed, "relation slot_grant_write_memory")
	assert.Contains(t, composed, "permission write_memory = slot_grant_write_memory->interact")
}

// A slot on the AUDIENCE permission is refused with an error, not skipped.
func TestGhToolkit_ASlotOnViewMemoryIsRefused(t *testing.T) {
	_, _, _, err := schema.ComposeSlots(ghPullRequestFragment(t),
		[]schema.SlotPair{{ResourceType: "github_pull_request", Permission: "view_memory"}})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "memory audience permission")
}
