package projectors

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestToolsProjector(t *testing.T) {
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "ns1"},
		Status: spiceboxv1alpha1.MCPServerStatus{
			ObservedTools: []string{"list", "get", "create"},
			// Health is the runtime Reachable condition, not spec-Valid.
			Conditions: []metav1.Condition{cond(spiceboxv1alpha1.MCPServerConditionReachable, metav1.ConditionTrue, "Reachable")},
		},
	}
	box := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "ns1"},
		// No conditions stamped → Unknown.
	}
	kit := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "github"},
		Spec:       spiceboxv1alpha1.SpiceboxToolkitSpec{Subcommands: []spiceboxv1alpha1.ToolkitSubcommand{{}, {}}},
		Status: spiceboxv1alpha1.SpiceboxToolkitStatus{
			Conditions: []metav1.Condition{cond(spiceboxv1alpha1.SpiceboxToolkitConditionValid, metav1.ConditionFalse, "BuiltinCollision")},
		},
	}
	spec := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "github-issues"},
		Spec:       spiceboxv1alpha1.SpiceboxToolspecSpec{AllowSubcommands: []string{"issue list"}},
		Status: spiceboxv1alpha1.SpiceboxToolspecStatus{
			Conditions: []metav1.Condition{cond(spiceboxv1alpha1.SpiceboxToolspecConditionValid, metav1.ConditionTrue, "Valid")},
		},
	}

	c := newClient(t, mcp, box, kit, spec)
	rows, err := toolsProjector{}.List(context.Background(), c)
	require.NoError(t, err)
	require.Len(t, rows, 4)

	m := rowByName(t, rows, "linear", "mcpserver")
	assert.Equal(t, "namespaced", m.Scope)
	assert.Equal(t, "Reachable", m.Status)
	assert.Equal(t, 3, countVal(m, "observedTools"))
	assert.Equal(t, "kubectl edit mcpserver linear -n ns1", m.ManageCmd)

	b := rowByName(t, rows, "echo", "sidecartoolbox")
	assert.Equal(t, "namespaced", b.Scope)
	assert.Equal(t, "Unknown", b.Status)

	k := rowByName(t, rows, "github", "toolkit")
	assert.Equal(t, "cluster", k.Scope)
	assert.Equal(t, "Degraded", k.Status)
	assert.Equal(t, "BuiltinCollision", k.StatusReason)
	assert.Equal(t, 2, countVal(k, "subcommands"))
	assert.Equal(t, "kubectl edit spiceboxtoolkit github", k.ManageCmd)

	s := rowByName(t, rows, "github-issues", "toolspec")
	assert.Equal(t, "cluster", s.Scope)
	assert.Equal(t, "Valid", s.Status)
	assert.Equal(t, 1, countVal(s, "allowedSubcommands"))
}
