//go:build integration

package pod_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	execfake "github.com/authzed/openagentprimitives/pkg/tools/exec/fake"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/conformance"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod"
)

// The built-in backend runs the same contract as every other. envtest gives it
// a real API server, so Ensure/Status/Teardown exercise genuine object
// lifecycle rather than a fake client's bookkeeping.
func TestConformance_Pod(t *testing.T) {
	// testenv.Start boots a shared envtest with the spicebox CRDs installed and
	// registers its own t.Cleanup; it skips the test when envtest binaries are
	// unavailable. Env exposes Cfg, Client and Scheme.
	env := testenv.Start(t)

	// ExecFor is required: pod.Runtime.Executor refuses to bind without a
	// transport, and pkg/tools/exec/fake is the seam Deps.ExecFor documents tests
	// inject at.
	rt, err := pod.Kind{}.NewRuntime(sandboxkinds.Deps{Client: env.Client, ExecFor: execfake.New().For})
	require.NoError(t, err)

	var n int
	conformance.Run(t, conformance.Subject{
		Name:    "pod",
		Runtime: rt,
		NewRequest: func() sandboxkinds.EnsureRequest {
			// A distinct session per subtest: pods persist in the shared
			// API server, so reusing one name would let cases collide.
			n++
			return sandboxkinds.EnsureRequest{
				Session: &v1alpha1.SpiceboxSession{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("demo-session-%d", n),
						Namespace: "default",
						// podspec.Build stamps this into the pod's owner
						// reference; envtest's real API server rejects an
						// owner reference with an empty UID, unlike a
						// never-created in-memory fixture.
						UID: types.UID(fmt.Sprintf("demo-session-%d-uid", n)),
					},
					Spec: v1alpha1.SpiceboxSessionSpec{Class: "demo-class"},
				},
				Class: v1alpha1.SpiceboxClassSpec{
					Image: "demo.invalid/spicebox-sandbox:test",
					Resources: v1alpha1.SpiceboxResources{
						CPU:              resource.MustParse("1"),
						Memory:           resource.MustParse("256Mi"),
						EphemeralStorage: resource.MustParse("1Gi"),
					},
				},
			}
		},
		// A pod must schedule before it can serve exec; envtest runs no
		// kubelet, so it stays Pending.
		ExpectReadyAfterEnsure: false,
	})

	// Guard against the fixture drifting into a shape envtest silently accepts.
	var pods corev1.PodList
	require.NoError(t, env.Client.List(t.Context(), &pods))
	require.NotEmpty(t, pods.Items, "conformance must have created at least one pod")
}
