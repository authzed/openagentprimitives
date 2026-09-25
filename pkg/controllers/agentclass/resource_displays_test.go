package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestResolveResourceDisplays(t *testing.T) {
	tk := spiceboxv1alpha1.SpiceboxToolkit{
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{
					{
						Name: "git_repo",
						Display: &spiceboxv1alpha1.SpiceDBResourceDisplay{
							Name: "Git repository", Icon: "repository", Label: "url_path",
						},
					},
					{Name: "tracker_issue"}, // no display declared: contributes nothing
				},
			},
		},
	}

	got := resolveResourceDisplays(nil, []spiceboxv1alpha1.SpiceboxToolkit{tk}, nil)

	assert.Equal(t, []spiceboxv1alpha1.ResolvedResourceDisplay{
		{ResourceType: "git_repo", Name: "Git repository", Icon: "repository", Label: "url_path"},
	}, got, "only declared displays are published; an undeclared type falls back at render time")
}

// Two fragments declaring a display for the same type must resolve
// deterministically, or status churns on every reconcile and violates SSA
// idempotency — the same property TestResolvePermissionTitles_IsDeterministic
// pins for titles.
func TestResolveResourceDisplays_IsDeterministic(t *testing.T) {
	mk := func(name string) spiceboxv1alpha1.SpiceboxToolkit {
		return spiceboxv1alpha1.SpiceboxToolkit{
			Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
				SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
					Resources: []spiceboxv1alpha1.SpiceDBResource{{
						Name:    "git_repo",
						Display: &spiceboxv1alpha1.SpiceDBResourceDisplay{Name: name},
					}},
				},
			},
		}
	}
	first := resolveResourceDisplays(nil, []spiceboxv1alpha1.SpiceboxToolkit{mk("A"), mk("B")}, nil)
	again := resolveResourceDisplays(nil, []spiceboxv1alpha1.SpiceboxToolkit{mk("A"), mk("B")}, nil)
	assert.Equal(t, first, again, "same inputs, byte-identical output")
	assert.Len(t, first, 1, "one row per resource type; first declaration wins")
	assert.Equal(t, "A", first[0].Name, "the FIRST fragment's display wins on a duplicate")
}

// Distinct resource types, sorted deterministically regardless of Go's
// randomized map iteration order — mirrors
// TestResolvePermissionTitles_MultipleDistinctPairsSortDeterministically.
func TestResolveResourceDisplays_MultipleDistinctTypesSortDeterministically(t *testing.T) {
	mcpServers := []spiceboxv1alpha1.MCPServer{
		{Spec: spiceboxv1alpha1.MCPServerSpec{SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
			Resources: []spiceboxv1alpha1.SpiceDBResource{{
				Name:    "zebra_widget",
				Display: &spiceboxv1alpha1.SpiceDBResourceDisplay{Name: "Zebra widget"},
			}},
		}}},
	}
	toolkits := []spiceboxv1alpha1.SpiceboxToolkit{
		{Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
			Resources: []spiceboxv1alpha1.SpiceDBResource{{
				Name:    "apple_widget",
				Display: &spiceboxv1alpha1.SpiceDBResourceDisplay{Name: "Apple widget"},
			}},
		}}},
	}
	want := []spiceboxv1alpha1.ResolvedResourceDisplay{
		{ResourceType: "apple_widget", Name: "Apple widget"},
		{ResourceType: "zebra_widget", Name: "Zebra widget"},
	}
	for i := 0; i < 25; i++ {
		got := resolveResourceDisplays(mcpServers, toolkits, nil)
		assert.Equal(t, want, got, "output sorted by resourceType on every call, iteration %d", i)
	}
}

// A SidecarToolbox carries a SpiceDBSchema fragment exactly as an MCPServer
// does, so the display it declares must reach status like any other. Left out,
// a type whose ONLY declaration is a sidecar's — workshop_draft is one — has no
// row at all, and an approval card falls back to rendering the wire handle at
// the person who has to decide. standingSources already walks sidecar
// fragments for the same reason.
func TestResolveResourceDisplays_IncludesSidecarToolboxFragments(t *testing.T) {
	sc := spiceboxv1alpha1.SidecarToolbox{
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{{
					Name:    "demo_draft",
					Display: &spiceboxv1alpha1.SpiceDBResourceDisplay{Name: "the draft in this workshop"},
				}},
			},
		},
	}

	got := resolveResourceDisplays(nil, nil, []spiceboxv1alpha1.SidecarToolbox{sc})

	assert.Equal(t, []spiceboxv1alpha1.ResolvedResourceDisplay{
		{ResourceType: "demo_draft", Name: "the draft in this workshop"},
	}, got, "a type declared only by a SidecarToolbox must still publish its display")
}
