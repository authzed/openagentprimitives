package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

func classWithBrowserToolbox() *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			BrowserToolboxes: []spiceboxv1alpha1.AgentClassBrowserToolboxRef{{Ref: "web", Name: "web"}},
		},
	}
}

// A class that references a BrowserToolbox gets the builtin browser schema
// toolkit, so web_host flows into the SAME standing/display resolvers a real
// toolkit uses. A class with no browser toolbox gets nothing — web_host never
// pollutes an unrelated class's status.
func TestBrowserSchemaToolkits_GatedOnReference(t *testing.T) {
	assert.Empty(t, browserSchemaToolkits(&spiceboxv1alpha1.AgentClass{}),
		"a class with no BrowserToolbox must not carry the web_host schema toolkit")

	got := browserSchemaToolkits(classWithBrowserToolbox())
	require.Len(t, got, 1, "a browser-referencing class gets exactly the one builtin toolkit")
	require.NotNil(t, got[0].Spec.SpiceDBSchema)
	require.Len(t, got[0].Spec.SpiceDBSchema.Resources, 1)
	assert.Equal(t, "web_host", got[0].Spec.SpiceDBSchema.Resources[0].Name)
}

// web_host resolves session-only standing through resolveResourceStandings when
// the builtin toolkit is present — the exact path git_repo's standing takes.
func TestResolveResourceStandings_IncludesWebHostForBrowserClass(t *testing.T) {
	tks := browserSchemaToolkits(classWithBrowserToolbox())

	standings, err := resolveResourceStandings(nil, tks, nil, nil)
	require.NoError(t, err, "web_host declares session-only standing, so resolution must not error")

	var found *spiceboxv1alpha1.ResolvedResourceStanding
	for i := range standings {
		if standings[i].ResourceType == "web_host" {
			found = &standings[i]
		}
	}
	require.NotNil(t, found, "web_host standing must be published for a browser class")
	assert.Equal(t, spiceboxv1alpha1.StandingSessionOnly, found.Standing)
	assert.Empty(t, found.ApproverPermission, "session-only names no approver permission")

	// Without the toolkit, web_host is not a resolved type at all.
	none, err := resolveResourceStandings(nil, nil, nil, nil)
	require.NoError(t, err)
	for _, s := range none {
		assert.NotEqual(t, "web_host", s.ResourceType)
	}
}

// web_host's declared display reaches status through resolveResourceDisplays, so
// the approval card shows the host behind a web mark, not the wire type name.
func TestResolveResourceDisplays_IncludesWebHostForBrowserClass(t *testing.T) {
	tks := browserSchemaToolkits(classWithBrowserToolbox())
	displays := resolveResourceDisplays(nil, tks, nil)

	var found *spiceboxv1alpha1.ResolvedResourceDisplay
	for i := range displays {
		if displays[i].ResourceType == "web_host" {
			found = &displays[i]
		}
	}
	require.NotNil(t, found, "web_host display must be published for a browser class")
	assert.Equal(t, browserschema.Resource().Display.Icon, found.Icon)
	assert.NotEmpty(t, found.Label)
}
