package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

func mustPerm(t *testing.T, permission, resourceType string) permsurface.Handle {
	t.Helper()
	h, err := permsurface.NewPermHandle(permission, resourceType)
	require.NoError(t, err)
	return h
}

// The projection is what makes the surface answerable outside the runner. It
// must be a PURE function of the tool envelope: this lands in an applied status
// field, and a value that reordered or re-timestamped itself would rewrite the
// object on every session start.
func TestPermissionSurfaceEntries_isDeterministicAndCarriesProvenance(t *testing.T) {
	surface := []permsurface.Descriptor{
		{
			Handle:      mustPerm(t, "write", "github_repo"),
			StateImpact: authz.Readwrite,
			Via:         []permsurface.Provenance{{Tool: "gh_pr_merge"}, {Tool: "gh_push"}},
		},
		{
			Handle:      mustPerm(t, "read", "github_repo"),
			StateImpact: authz.Readonly,
			Via:         []permsurface.Provenance{{Tool: "gh_pr_view"}},
		},
	}

	got := permissionSurfaceEntries(surface)

	require.Len(t, got, 2)
	assert.Equal(t, "perm:read:github_repo", got[0].Handle,
		"entries sort by handle so two runs of the same envelope produce identical bytes")
	assert.Equal(t, "perm:write:github_repo", got[1].Handle)
	assert.Equal(t, "readonly", got[0].StateImpact)
	assert.Equal(t, []string{"gh_pr_merge", "gh_push"}, got[1].Tools,
		"the tools reaching a handle are the answer to \"why can it do this?\"")

	assert.Equal(t, got, permissionSurfaceEntries(surface),
		"projecting twice must be byte-identical, or every session start rewrites status")
}

// Input order must not leak into the output, since the runner's tool list order
// is not guaranteed stable across restarts.
func TestPermissionSurfaceEntries_isIndependentOfInputOrder(t *testing.T) {
	a := permsurface.Descriptor{Handle: mustPerm(t, "read", "b_type"), StateImpact: authz.Readonly,
		Via: []permsurface.Provenance{{Tool: "t2"}, {Tool: "t1"}}}
	b := permsurface.Descriptor{Handle: mustPerm(t, "read", "a_type"), StateImpact: authz.Readonly,
		Via: []permsurface.Provenance{{Tool: "t1"}}}

	assert.Equal(t,
		permissionSurfaceEntries([]permsurface.Descriptor{a, b}),
		permissionSurfaceEntries([]permsurface.Descriptor{b, a}),
		"a reordered tool list must not produce a different status write")
}

// A tool reaching one handle through both its base permission and a variant
// appears twice in Via. Listing it twice would read as two different routes.
func TestPermissionSurfaceEntries_dedupesToolsReachingAHandleTwice(t *testing.T) {
	got := permissionSurfaceEntries([]permsurface.Descriptor{{
		Handle:      mustPerm(t, "write", "tracker_issue"),
		StateImpact: authz.Readwrite,
		Via: []permsurface.Provenance{
			{Tool: "tracker_update"},
			{Tool: "tracker_update", Condition: `args.op != "comment"`},
		},
	}})

	require.Len(t, got, 1)
	assert.Equal(t, []string{"tracker_update"}, got[0].Tools)
}

func TestPermissionSurfaceEntries_emptySurfaceProjectsToNothing(t *testing.T) {
	assert.Empty(t, permissionSurfaceEntries(nil),
		"nil rather than an empty slice, so the status field is omitted entirely")
}
