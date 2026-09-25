package gke

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

func TestParseNEGSelfLink(t *testing.T) {
	cases := []struct {
		name                   string
		in                     string
		project, zone, negName string
		ok                     bool
	}{
		{
			name:    "v1 self-link: parses project/zone/name",
			in:      "https://www.googleapis.com/compute/v1/projects/acme-proj/zones/us-east1-b/networkEndpointGroups/k8s1-abc-agentprimitives-system-spicebox-webd-808-deadbeef",
			project: "acme-proj", zone: "us-east1-b", negName: "k8s1-abc-agentprimitives-system-spicebox-webd-808-deadbeef", ok: true,
		},
		{
			name:    "beta self-link: parses project/zone/name",
			in:      "https://www.googleapis.com/compute/beta/projects/acme-proj/zones/us-east1-c/networkEndpointGroups/k8s1-xyz",
			project: "acme-proj", zone: "us-east1-c", negName: "k8s1-xyz", ok: true,
		},
		{name: "empty string: not ok", in: "", ok: false},
		{name: "missing zones segment: not ok", in: "https://www.googleapis.com/compute/v1/projects/acme-proj/networkEndpointGroups/k8s1-xyz", ok: false},
		{name: "missing name after networkEndpointGroups: not ok", in: "https://www.googleapis.com/compute/v1/projects/acme-proj/zones/us-east1-b/networkEndpointGroups", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, z, n, ok := parseNEGSelfLink(tc.in)
			assert.Equal(t, tc.ok, ok)
			if tc.ok {
				assert.Equal(t, tc.project, p)
				assert.Equal(t, tc.zone, z)
				assert.Equal(t, tc.negName, n)
			}
		})
	}
}

func TestIsNEGInUse(t *testing.T) {
	assert.True(t, isNEGInUse(errors.New("gcloud ...: exit status 1: ERROR: resourceInUseByAnotherResource: The network_endpoint_group resource is already being used")))
	assert.True(t, isNEGInUse(errors.New("Error: The resource is in use by another resource")))
	assert.False(t, isNEGInUse(nil))
	assert.False(t, isNEGInUse(errors.New("some other gcloud failure")))
}

// svcnegGVR mirrors the unexported GVR in unwedge.go for test fixtures.
var testSvcnegGVR = schema.GroupVersionResource{Group: "networking.gke.io", Version: "v1beta1", Resource: "servicenetworkendpointgroups"}

func newStuckSvcNEG(name, namespace string, negSelfLinks ...string) *unstructured.Unstructured {
	negs := make([]any, 0, len(negSelfLinks))
	for _, l := range negSelfLinks {
		negs = append(negs, map[string]any{"selfLink": l})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.gke.io/v1beta1",
		"kind":       "ServiceNetworkEndpointGroup",
		"metadata": map[string]any{
			"name":              name,
			"namespace":         namespace,
			"finalizers":        []any{"networking.gke.io/neg-finalizer"},
			"deletionTimestamp": "2026-06-29T16:00:28Z",
		},
		"status": map[string]any{"networkEndpointGroups": negs},
	}}
}

// withSeams swaps the package-level seams for a test and restores them after.
// Tests using it must NOT run in parallel (package-global mutation).
func withSeams(t *testing.T, present bool, del func(ctx context.Context, rep cloud.Reporter, project, zone, name string) error) {
	t.Helper()
	origDel, origPath := deleteGCPNEG, gcloudOnPath
	deleteGCPNEG = del
	gcloudOnPath = func() bool { return present }
	t.Cleanup(func() { deleteGCPNEG, gcloudOnPath = origDel, origPath })
}

func dynClientWith(objs ...*unstructured.Unstructured) *dynfake.FakeDynamicClient {
	listKinds := map[schema.GroupVersionResource]string{testSvcnegGVR: "ServiceNetworkEndpointGroupList"}
	runtimeObjs := make([]any, 0, len(objs))
	for _, o := range objs {
		runtimeObjs = append(runtimeObjs, o)
	}
	// NewSimpleDynamicClientWithCustomListKinds takes (scheme, listKinds, ...runtime.Object)
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme.Scheme, listKinds, toRuntime(runtimeObjs)...)
}

func svcNEGFinalizers(t *testing.T, dc *dynfake.FakeDynamicClient, name, ns string) []string {
	t.Helper()
	got, err := dc.Resource(testSvcnegGVR).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return got.GetFinalizers()
}

func TestGKEUnwedge(t *testing.T) {
	const ns = "agentprimitives-system"
	const negName = "k8s1-abc-agentprimitives-system-spicebox-webd-808-deadbeef"
	link := func(zone string) string {
		return fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/acme-proj/zones/%s/networkEndpointGroups/%s", zone, negName)
	}

	t.Run("remediate=false: inspects only, surfaces commands, mutates nothing", func(t *testing.T) {
		dc := dynClientWith(newStuckSvcNEG("neg-obj", ns, link("us-east1-b")))
		var calls int
		withSeams(t, true, func(context.Context, cloud.Reporter, string, string, string) error { calls++; return nil })

		rep, err := Strategy{}.UnwedgeTerminatingNamespace(context.Background(), cloud.Clients{Dynamic: dc}, cloud.NopReporter{}, ns, false)
		require.NoError(t, err)
		assert.Equal(t, 0, calls, "remediate=false must not delete any NEG")
		assert.Contains(t, rep.StuckFinalizers, "networking.gke.io/neg-finalizer")
		assert.NotEmpty(t, rep.ManualCommands, "must surface the manual fix commands")
		assert.Equal(t, []string{"networking.gke.io/neg-finalizer"}, svcNEGFinalizers(t, dc, "neg-obj", ns), "finalizer must be untouched")
	})

	t.Run("remediate=true happy path: deletes NEG then clears finalizer", func(t *testing.T) {
		dc := dynClientWith(newStuckSvcNEG("neg-obj", ns, link("us-east1-b"), link("us-east1-c")))
		var deleted []string
		withSeams(t, true, func(_ context.Context, _ cloud.Reporter, project, zone, name string) error {
			deleted = append(deleted, project+"/"+zone+"/"+name)
			return nil
		})

		rep, err := Strategy{}.UnwedgeTerminatingNamespace(context.Background(), cloud.Clients{Dynamic: dc}, cloud.NopReporter{}, ns, true)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"acme-proj/us-east1-b/" + negName, "acme-proj/us-east1-c/" + negName}, deleted)
		assert.Len(t, rep.CloudResourcesDeleted, 2)
		assert.Contains(t, rep.FinalizersCleared, "neg-obj")
		assert.Empty(t, svcNEGFinalizers(t, dc, "neg-obj", ns), "finalizer must be cleared")
	})

	t.Run("remediate=true, NEG already gone: idempotent, clears finalizer", func(t *testing.T) {
		dc := dynClientWith(newStuckSvcNEG("neg-obj", ns, link("us-east1-b")))
		withSeams(t, true, func(context.Context, cloud.Reporter, string, string, string) error {
			return cloudNotFoundErr()
		})
		rep, err := Strategy{}.UnwedgeTerminatingNamespace(context.Background(), cloud.Clients{Dynamic: dc}, cloud.NopReporter{}, ns, true)
		require.NoError(t, err)
		assert.Contains(t, rep.FinalizersCleared, "neg-obj")
		assert.Empty(t, svcNEGFinalizers(t, dc, "neg-obj", ns))
	})

	t.Run("remediate=true, NEG still referenced: no clear, Blocked populated", func(t *testing.T) {
		dc := dynClientWith(newStuckSvcNEG("neg-obj", ns, link("us-east1-b")))
		withSeams(t, true, func(context.Context, cloud.Reporter, string, string, string) error {
			return inUseErr()
		})
		rep, err := Strategy{}.UnwedgeTerminatingNamespace(context.Background(), cloud.Clients{Dynamic: dc}, cloud.NopReporter{}, ns, true)
		require.NoError(t, err)
		assert.NotEmpty(t, rep.Blocked, "a referenced NEG must be reported as Blocked")
		assert.NotContains(t, rep.FinalizersCleared, "neg-obj")
		assert.Equal(t, []string{"networking.gke.io/neg-finalizer"}, svcNEGFinalizers(t, dc, "neg-obj", ns), "finalizer must NOT be cleared while NEG is referenced")
	})

	t.Run("remediate=true, gcloud absent: degrades to manual commands, no mutation", func(t *testing.T) {
		dc := dynClientWith(newStuckSvcNEG("neg-obj", ns, link("us-east1-b")))
		var calls int
		withSeams(t, false, func(context.Context, cloud.Reporter, string, string, string) error { calls++; return nil })
		rep, err := Strategy{}.UnwedgeTerminatingNamespace(context.Background(), cloud.Clients{Dynamic: dc}, cloud.NopReporter{}, ns, true)
		require.NoError(t, err)
		assert.Equal(t, 0, calls, "gcloud absent: must not attempt deletes")
		assert.NotEmpty(t, rep.ManualCommands)
		assert.Equal(t, []string{"networking.gke.io/neg-finalizer"}, svcNEGFinalizers(t, dc, "neg-obj", ns))
	})

	t.Run("remediate=true, neg-finalizer present but empty status: finalizer cleared, nothing deleted", func(t *testing.T) {
		// SvcNEG has the neg-finalizer but zero entries in status.networkEndpointGroups.
		// The finalizer must be cleared, but no GCP NEG deletion should occur.
		dc := dynClientWith(newStuckSvcNEG("neg-obj", ns)) // no selfLink args → empty NEG list
		var calls int
		withSeams(t, true, func(context.Context, cloud.Reporter, string, string, string) error { calls++; return nil })
		rep, err := Strategy{}.UnwedgeTerminatingNamespace(context.Background(), cloud.Clients{Dynamic: dc}, cloud.NopReporter{}, ns, true)
		require.NoError(t, err)
		assert.Equal(t, 0, calls, "no NEGs to delete — deleteGCPNEG must not be called")
		assert.Contains(t, rep.FinalizersCleared, "neg-obj", "finalizer must be cleared")
		assert.Empty(t, rep.CloudResourcesDeleted, "no cloud resources were deleted")
		assert.Empty(t, svcNEGFinalizers(t, dc, "neg-obj", ns), "neg-finalizer must be absent after clear")
	})

	t.Run("no stuck SvcNEGs: empty report", func(t *testing.T) {
		dc := dynClientWith()
		withSeams(t, true, func(context.Context, cloud.Reporter, string, string, string) error { return nil })
		rep, err := Strategy{}.UnwedgeTerminatingNamespace(context.Background(), cloud.Clients{Dynamic: dc}, cloud.NopReporter{}, ns, true)
		require.NoError(t, err)
		assert.Empty(t, rep.StuckFinalizers)
		assert.Empty(t, rep.FinalizersCleared)
	})

	t.Run("remediate=true, unparseable self-link on one NEG: no clear, Blocked populated", func(t *testing.T) {
		// One valid self-link and one malformed; allGone must stay false → finalizer NOT cleared.
		dc := dynClientWith(newStuckSvcNEG("neg-obj", ns, link("us-east1-b"), "not-a-valid-selflink"))
		var calls int
		withSeams(t, true, func(context.Context, cloud.Reporter, string, string, string) error { calls++; return nil })
		rep, err := Strategy{}.UnwedgeTerminatingNamespace(context.Background(), cloud.Clients{Dynamic: dc}, cloud.NopReporter{}, ns, true)
		require.NoError(t, err)
		assert.NotEmpty(t, rep.Blocked, "unparseable self-link must be recorded in Blocked")
		assert.NotContains(t, rep.FinalizersCleared, "neg-obj")
		assert.Equal(t, []string{"networking.gke.io/neg-finalizer"}, svcNEGFinalizers(t, dc, "neg-obj", ns), "finalizer must NOT be cleared while a NEG self-link is unparseable")
	})

	t.Run("remediate=true, malformed status.networkEndpointGroups: no clear, Blocked populated", func(t *testing.T) {
		// status.networkEndpointGroups is a string instead of a slice → NestedSlice returns an error.
		malformed := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "networking.gke.io/v1beta1",
			"kind":       "ServiceNetworkEndpointGroup",
			"metadata": map[string]any{
				"name":              "neg-obj",
				"namespace":         ns,
				"finalizers":        []any{"networking.gke.io/neg-finalizer"},
				"deletionTimestamp": "2026-06-29T16:00:28Z",
			},
			"status": map[string]any{"networkEndpointGroups": "not-a-slice"},
		}}
		dc := dynClientWith(malformed)
		var calls int
		withSeams(t, true, func(context.Context, cloud.Reporter, string, string, string) error { calls++; return nil })
		rep, err := Strategy{}.UnwedgeTerminatingNamespace(context.Background(), cloud.Clients{Dynamic: dc}, cloud.NopReporter{}, ns, true)
		require.NoError(t, err)
		assert.Equal(t, 0, calls, "deleteGCPNEG must not be called when status is unreadable")
		assert.NotEmpty(t, rep.Blocked, "malformed status must be recorded in Blocked")
		assert.NotContains(t, rep.FinalizersCleared, "neg-obj")
		assert.Equal(t, []string{"networking.gke.io/neg-finalizer"}, svcNEGFinalizers(t, dc, "neg-obj", ns), "finalizer must NOT be cleared when NEG status is unreadable")
	})
}

func toRuntime(in []any) []runtime.Object {
	out := make([]runtime.Object, 0, len(in))
	for _, o := range in {
		out = append(out, o.(runtime.Object))
	}
	return out
}

func cloudNotFoundErr() error {
	return fmt.Errorf("gcloud compute network-endpoint-groups delete: exit status 1: ERROR: The resource 'x' was not found")
}

func inUseErr() error {
	return fmt.Errorf("gcloud compute network-endpoint-groups delete: exit status 1: ERROR: resourceInUseByAnotherResource")
}
