package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestResolvePermissionTitles(t *testing.T) {
	tk := spiceboxv1alpha1.SpiceboxToolkit{
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{{
					Name: "git_repo",
					Permissions: []spiceboxv1alpha1.SpiceDBPermission{
						{Name: "push", Title: "Push commits to the repository"},
						{Name: "read"}, // undeclared: contributes nothing
					},
				}},
			},
		},
	}

	got := resolvePermissionTitles(nil, []spiceboxv1alpha1.SpiceboxToolkit{tk}, nil)

	assert.Equal(t, []spiceboxv1alpha1.ResolvedPermissionTitle{
		{ResourceType: "git_repo", Permission: "push", Title: "Push commits to the repository"},
	}, got, "only declared titles are published; an absent one falls back at render time")
}

// Two fragments declaring the same pair must resolve deterministically, or
// status churns on every reconcile and violates SSA idempotency.
func TestResolvePermissionTitles_IsDeterministic(t *testing.T) {
	mk := func(title string) spiceboxv1alpha1.SpiceboxToolkit {
		return spiceboxv1alpha1.SpiceboxToolkit{
			Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
				SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
					Resources: []spiceboxv1alpha1.SpiceDBResource{{
						Name:        "git_repo",
						Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "push", Title: title}},
					}},
				},
			},
		}
	}
	first := resolvePermissionTitles(nil, []spiceboxv1alpha1.SpiceboxToolkit{mk("A"), mk("B")}, nil)
	again := resolvePermissionTitles(nil, []spiceboxv1alpha1.SpiceboxToolkit{mk("A"), mk("B")}, nil)
	assert.Equal(t, first, again, "same inputs, byte-identical output")
	assert.Len(t, first, 1, "one row per (resourceType, permission)")
}

// Several DISTINCT (resourceType, permission) pairs, unlike the single-pair
// case above, are what actually exercises map-iteration order: with only one
// key, any order looks "correct" by accident. Go's map range order is
// randomized per iteration, so a naive `for k, v := range seen { out =
// append(out, ...) }` implementation with no explicit sort would very likely
// produce a different element order across some of these repeated calls and
// fail this assertion; sorting by (resourceType, permission) makes the output
// stable regardless.
func TestResolvePermissionTitles_MultipleDistinctPairsSortDeterministically(t *testing.T) {
	mcpServers := []spiceboxv1alpha1.MCPServer{
		{Spec: spiceboxv1alpha1.MCPServerSpec{SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
			Resources: []spiceboxv1alpha1.SpiceDBResource{{
				Name:        "zebra_widget",
				Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "write", Title: "Write to the zebra widget"}},
			}},
		}}},
		{Spec: spiceboxv1alpha1.MCPServerSpec{SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
			Resources: []spiceboxv1alpha1.SpiceDBResource{{
				Name:        "apple_widget",
				Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "delete", Title: "Delete the apple widget"}},
			}},
		}}},
	}
	toolkits := []spiceboxv1alpha1.SpiceboxToolkit{
		{Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
			Resources: []spiceboxv1alpha1.SpiceDBResource{{
				Name:        "mango_widget",
				Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "read", Title: "Read the mango widget"}},
			}},
		}}},
		{Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
			Resources: []spiceboxv1alpha1.SpiceDBResource{{
				Name:        "apple_widget",
				Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "read", Title: "Read the apple widget"}},
			}},
		}}},
	}
	want := []spiceboxv1alpha1.ResolvedPermissionTitle{
		{ResourceType: "apple_widget", Permission: "delete", Title: "Delete the apple widget"},
		{ResourceType: "apple_widget", Permission: "read", Title: "Read the apple widget"},
		{ResourceType: "mango_widget", Permission: "read", Title: "Read the mango widget"},
		{ResourceType: "zebra_widget", Permission: "write", Title: "Write to the zebra widget"},
	}

	for i := 0; i < 25; i++ {
		got := resolvePermissionTitles(mcpServers, toolkits, nil)
		assert.Equal(t, want, got, "output sorted by (resourceType, permission) on every call, iteration %d", i)
	}
}

// A planning note is published on the same entry as the title, and either may
// stand alone.
//
// They share a key — (resourceType, permission) — and differ only in audience:
// a title is read by the human deciding, a note by the agent planning. One walk
// collects both, so a permission declaring only a note still produces an entry
// and a note is never lost for want of a title beside it.
func TestResolvePermissionTitles_PublishesPlanningNotes(t *testing.T) {
	tk := spiceboxv1alpha1.SpiceboxToolkit{
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{{
					Name: "git_repo",
					Permissions: []spiceboxv1alpha1.SpiceDBPermission{
						{Name: "write", Title: "Change the checked-out files",
							PlanningNote: "Declare perm:read:git_repo alongside it."},
						{Name: "fetch", PlanningNote: "Keys on the remote URL."},
						{Name: "push", Title: "Push commits to the repository"},
						{Name: "read"}, // neither: contributes nothing
					},
				}},
			},
		},
	}

	got := resolvePermissionTitles(nil, []spiceboxv1alpha1.SpiceboxToolkit{tk}, nil)

	assert.Equal(t, []spiceboxv1alpha1.ResolvedPermissionTitle{
		{ResourceType: "git_repo", Permission: "fetch",
			PlanningNote: "Keys on the remote URL."},
		{ResourceType: "git_repo", Permission: "push", Title: "Push commits to the repository"},
		{ResourceType: "git_repo", Permission: "write", Title: "Change the checked-out files",
			PlanningNote: "Declare perm:read:git_repo alongside it."},
	}, got, "a note alone is enough to publish an entry; a permission declaring neither is omitted")
}

// The titles half of the same omission: a SidecarToolbox fragment declares
// permissions exactly as an MCPServer's does, and a title left uncollected
// means the approval card detokenizes the handle instead of showing the phrase
// the toolbox author wrote for the person deciding.
func TestResolvePermissionTitles_IncludesSidecarToolboxFragments(t *testing.T) {
	sc := spiceboxv1alpha1.SidecarToolbox{
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{{
					Name: "demo_draft",
					Permissions: []spiceboxv1alpha1.SpiceDBPermission{
						{Name: "change", Title: "Change the draft", PlanningNote: "list this to apply drafts in a phase"},
					},
				}},
			},
		},
	}

	got := resolvePermissionTitles(nil, nil, []spiceboxv1alpha1.SidecarToolbox{sc})

	assert.Equal(t, []spiceboxv1alpha1.ResolvedPermissionTitle{{
		ResourceType: "demo_draft", Permission: "change",
		Title: "Change the draft", PlanningNote: "list this to apply drafts in a phase",
	}}, got, "a permission declared only by a SidecarToolbox must still publish its prose")
}
