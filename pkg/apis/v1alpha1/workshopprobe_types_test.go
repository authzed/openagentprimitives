package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestWorkshopProbe_DeepCopyRoundTrip(t *testing.T) {
	in := &WorkshopProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ws-abc123def456"},
		Spec:       WorkshopProbeSpec{Image: "ghcr.io/demo/mcp:v1", TimeoutSeconds: 90},
		Status: WorkshopProbeStatus{
			Phase: WorkshopProbePhaseSucceeded,
			Tools: []WorkshopProbedTool{{Name: "list_things", Description: "d", InputSchema: `{"type":"object"}`}},
		},
	}
	out := in.DeepCopy()
	require.Equal(t, in.Spec, out.Spec)
	require.Equal(t, in.Status.Tools, out.Status.Tools)
	out.Status.Tools[0].Name = "mutated"
	assert.Equal(t, "list_things", in.Status.Tools[0].Name, "deepcopy must not alias the slice")
	var _ client.Object = in // compile-time: implements client.Object (DeepCopyObject generated)
}
