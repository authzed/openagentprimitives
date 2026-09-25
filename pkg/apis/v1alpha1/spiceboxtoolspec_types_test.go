package v1alpha1_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	. "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestToSpec_SecretOutput verifies that a SpiceboxToolspecSpec with a
// secretOutput declaration produces, via ToSpec(), a spec.Spec with
// SecretOutput fully populated. This exercises the JSON round-trip path:
// the CRD field must carry the same json tag ("secretOutput") and matching
// sub-field tags as spec.SecretOutputSpec for the round-trip to preserve it.
func TestToSpec_SecretOutput(t *testing.T) {
	crdSpec := SpiceboxToolspecSpec{
		Name:    "get-kubeconfig",
		Version: "1.0.0",
		Toolkit: ToolspecToolkitRef{Name: "kubectl", Revision: "1.29"},
		SecretOutput: &ToolspecSecretOutput{
			Name:        "kubeconfig",
			Source:      "file:/p",
			Description: "d",
		},
	}

	sp, err := crdSpec.ToSpec()
	require.NoError(t, err, "ToSpec must succeed")
	require.NotNil(t, sp.SecretOutput, "SecretOutput must be non-nil after ToSpec round-trip")
	assert.Equal(t, "kubeconfig", sp.SecretOutput.Name, "SecretOutput.Name")
	assert.Equal(t, "file:/p", sp.SecretOutput.Source, "SecretOutput.Source")
	assert.Equal(t, "d", sp.SecretOutput.Description, "SecretOutput.Description")
}

// TestToSpec_SecretOutput_Absent verifies that a spec with no secretOutput
// field round-trips cleanly: SecretOutput is nil and ToSpec does not error.
func TestToSpec_SecretOutput_Absent(t *testing.T) {
	crdSpec := SpiceboxToolspecSpec{
		Name:    "list-pods",
		Version: "1.0.0",
		Toolkit: ToolspecToolkitRef{Name: "kubectl", Revision: "1.29"},
	}

	sp, err := crdSpec.ToSpec()
	require.NoError(t, err, "ToSpec must succeed when secretOutput is absent")
	assert.Nil(t, sp.SecretOutput, "SecretOutput must be nil when not declared")
}

// TestToSpec_WritesRelationships verifies the CRD→spec JSON round-trip
// carries writesRelationships, like TestToSpec_SecretOutput does for
// secretOutput. Field-for-field json-tag parity is the contract.
func TestToSpec_WritesRelationships(t *testing.T) {
	in := SpiceboxToolspecSpec{
		Name:             "sre-fetch-kubeconfig",
		Version:          "1.0.0",
		Toolkit:          ToolspecToolkitRef{Name: "kubectl", Revision: "1.29"},
		AllowSubcommands: []string{"get-kubeconfig"},
		WritesRelationships: []MCPServerRelationshipWrite{{
			When:             "result.success",
			Exclusive:        true,
			RequireSlotBound: true,
			Tuple: MCPServerRelationshipTuple{
				Resource: `"cluster:" + args.argv[1]`,
				Relation: `"debug_target"`,
				Subject:  `"agentsession:" + session`,
			},
		}},
	}
	sp, err := in.ToSpec()
	require.NoError(t, err)
	require.Len(t, sp.WritesRelationships, 1)
	assert.Equal(t, "result.success", sp.WritesRelationships[0].When)
	assert.Equal(t, `"debug_target"`, sp.WritesRelationships[0].Tuple.Relation)
	assert.True(t, sp.WritesRelationships[0].Exclusive, "exclusive must round-trip through ToSpec (byte-identical json tag)")
	// requireSlotBound is the sandbox path's ONLY route from the CRD to
	// relwrites.Block: the toolspec dispatcher reads spec.Spec, never the CR.
	// A missing or mistyped json tag on spec.RelationshipWriteSpec would drop
	// the gate silently here and the block would write unchecked.
	assert.True(t, sp.WritesRelationships[0].RequireSlotBound,
		"requireSlotBound must round-trip through ToSpec (byte-identical json tag)")
}

// TestToSpec_Observes verifies the CRD→spec JSON round-trip carries
// observes, like TestToSpec_WritesRelationships does for
// writesRelationships. Without spec.ObservesSpec / spec.ObserveSubjectSpec
// carrying matching json tags, ToSpec would silently drop the field and the
// toolspec controller's compileObserves phase would validate a field the
// runner's dispatcher never actually sees.
func TestToSpec_Observes(t *testing.T) {
	in := SpiceboxToolspecSpec{
		Name:             "sre-open-pr",
		Version:          "1.0.0",
		Toolkit:          ToolspecToolkitRef{Name: "gh", Revision: "1.0"},
		AllowSubcommands: []string{"pr create"},
		Observes: []ObservesBlock{{
			When:    "result.success",
			ForEach: "result.results",
			Subjects: []ObserveSubject{
				{ResourceType: `"github_pr"`, ResourceID: "item.id"},
			},
			Facts: map[string]string{"is_cross_repository": "item.isCrossRepository"},
		}},
	}
	sp, err := in.ToSpec()
	require.NoError(t, err)
	require.Len(t, sp.Observes, 1)
	assert.Equal(t, "result.success", sp.Observes[0].When)
	assert.Equal(t, "result.results", sp.Observes[0].ForEach)
	require.Len(t, sp.Observes[0].Subjects, 1)
	assert.Equal(t, `"github_pr"`, sp.Observes[0].Subjects[0].ResourceType)
	assert.Equal(t, "item.id", sp.Observes[0].Subjects[0].ResourceID)
	assert.Equal(t, "item.isCrossRepository", sp.Observes[0].Facts["is_cross_repository"])
}
