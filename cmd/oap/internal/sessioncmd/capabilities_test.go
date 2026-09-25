package sessioncmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestRenderPermissionSurface_showsSeverityAndHandle(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, renderPermissionSurface(&buf, "ns", "s", []spiceboxv1alpha1.PermissionSurfaceEntry{
		{Handle: "perm:read:github_repo", StateImpact: "readonly", Tools: []string{"gh_pr_view"}},
		{Handle: "tool:apply_workspace", StateImpact: "external", Tools: []string{"apply_workspace"}},
	}, false))

	out := buf.String()
	assert.Contains(t, out, "readonly")
	assert.Contains(t, out, "perm:read:github_repo")
	assert.Contains(t, out, "external")
	assert.NotContains(t, out, "gh_pr_view",
		"tools are opt-in; the default view is the reach, not the routes to it")
}

func TestRenderPermissionSurface_withToolsShowsWhatReachesEachHandle(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, renderPermissionSurface(&buf, "ns", "s", []spiceboxv1alpha1.PermissionSurfaceEntry{
		{Handle: "perm:write:tracker_issue", StateImpact: "readwrite", Tools: []string{"tracker_close", "tracker_update"}},
	}, true))

	assert.Contains(t, buf.String(), "tracker_close, tracker_update",
		"--tools answers \"why can it do this?\"")
}

// The distinction this exists to preserve: "reaches nothing" and "nobody has
// enumerated it" are different states, and an operator who reads an empty list
// as the former would conclude the agent is harmless when it may simply not
// have started.
func TestRenderPermissionSurface_anAbsentSurfaceSaysSoRatherThanLookingEmpty(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, renderPermissionSurface(&buf, "ns", "my-session", nil, false))

	out := buf.String()
	assert.Contains(t, out, "no permission surface published")
	assert.Contains(t, out, "agentsession:ns/my-session",
		"naming the session is what makes the message actionable")
	assert.Contains(t, out, "session start",
		"the reader must learn WHY it is absent, or they will read it as 'reaches nothing'")
}
