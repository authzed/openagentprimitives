package toolscmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestBuildKubectlEditArgs(t *testing.T) {
	cases := []struct {
		name     string
		g        *apcmd.Globals
		resource string
		ns       string
		want     []string
	}{
		{
			name:     "all flags set: kubeconfig + context + ns appended",
			g:        &apcmd.Globals{Kubeconfig: "/tmp/kc", Context: "kind"},
			resource: "mcpservers.agentprimitives.authzed.com/linear",
			ns:       "ns",
			want: []string{
				"edit", "mcpservers.agentprimitives.authzed.com/linear",
				"--kubeconfig", "/tmp/kc",
				"--context", "kind",
				"-n", "ns",
			},
		},
		{
			name:     "defaults: only edit + resource appended",
			g:        &apcmd.Globals{},
			resource: "res/x",
			ns:       "",
			want:     []string{"edit", "res/x"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildKubectlEditArgs(tc.resource, tc.ns, tc.g)
			assert.Equal(t, tc.want, got, "kubectl args")
		})
	}
}

// TestToolsEdit_DispatchesViaResolve verifies that `oap tools edit` resolves a
// name to its kind and calls the kubectl runner with the right resource arg.
func TestToolsEdit_DispatchesViaResolve(t *testing.T) {
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "ns"},
	}
	c := aptest.ClientBuilder(t).WithObjects(mcp).Build()
	prev := toolsListClientFactory
	defer func() { toolsListClientFactory = prev }()
	toolsListClientFactory = func(_ *apcmd.Globals) (client.Client, string, error) {
		return c, "ns", nil
	}

	prevRunner := editKubectlRunner
	defer func() { editKubectlRunner = prevRunner }()
	var captured []string
	editKubectlRunner = func(args []string) error {
		captured = args
		return nil
	}

	_, err := runTools(t, "edit", "linear")
	require.NoError(t, err, "tools edit")
	require.GreaterOrEqual(t, len(captured), 2, "expected at least 2 args, got %v", captured)
	assert.Equal(t, "edit", captured[0], "first kubectl arg")
	assert.Equal(t, "mcpservers.agentprimitives.authzed.com/linear", captured[1], "resolved resource")
}
