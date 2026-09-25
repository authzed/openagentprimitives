package installcmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// gkeClusterKindNode returns a GKE-providerID node for resolveClusterKind's
// detection/validation cases. Reuses image_mode_test.go's gkeNode helper
// (same package, same shape) rather than duplicating a second node-builder.
func gkeClusterKindNode() runtime.Object {
	return aptest.NodeWithProviderID("gce://p/us-central1-a/n1")
}

func TestResolveClusterKind(t *testing.T) {
	cases := []struct {
		name        string
		kind        string
		nodes       []runtime.Object
		host        string
		contextName string
		wantKey     string
		wantErr     string
	}{
		{
			name:    "no flag on a GKE cluster: detects gke, preserving today's zero-flag experience",
			kind:    "",
			nodes:   []runtime.Object{gkeClusterKindNode()},
			wantKey: cloud.KeyGKE,
		},
		{
			name:        "no flag on an unrecognized cluster: falls back to default",
			kind:        "",
			host:        "https://127.0.0.1:6443",
			contextName: "kind-ap",
			wantKey:     cloud.KeyDefault,
		},
		{
			name:        "--local on a local cluster: selects local",
			kind:        cloud.KeyLocal,
			host:        "https://127.0.0.1:6443",
			contextName: "kind-ap",
			wantKey:     cloud.KeyLocal,
		},
		{
			name:        "--local on a GKE cluster: refused before any apply",
			kind:        cloud.KeyLocal,
			nodes:       []runtime.Object{gkeClusterKindNode()},
			host:        "https://127.0.0.1:6443",
			contextName: "kind-ap",
			wantErr:     "refusing the local cluster kind",
		},
		{
			name:    "--cluster-kind=eks on a GKE cluster: refused",
			kind:    cloud.KeyEKS,
			nodes:   []runtime.Object{gkeClusterKindNode()},
			wantErr: "this cluster's nodes report gke",
		},
		{
			name:    "typo'd kind: errors naming the registered kinds, never a silent default",
			kind:    "gek",
			wantErr: `unknown cluster kind "gek"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveClusterKind(
				context.Background(), tc.kind,
				fake.NewSimpleClientset(tc.nodes...),
				&rest.Config{Host: tc.host}, tc.contextName, false)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Nil(t, got, "a failed resolution must not hand back a usable Strategy")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tc.wantKey, got.Key())
		})
	}
}
