package installcmd

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"
)

// memDeployment builds a Deployment named "op" whose single container "c"
// carries the given memory request/limit ("" leaves the field out).
func memDeployment(t *testing.T, request, limit string) *unstructured.Unstructured {
	t.Helper()
	res := map[string]any{}
	if request != "" {
		res["requests"] = map[string]any{"memory": request, "cpu": "100m"}
	}
	if limit != "" {
		res["limits"] = map[string]any{"memory": limit, "cpu": "500m"}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "op", "namespace": "ns"},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": "c", "image": "x", "resources": res}},
		}}},
	}}
}

func containerMemory(t *testing.T, d *unstructured.Unstructured, field string) string {
	t.Helper()
	cs, _, err := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	require.NoError(t, err)
	require.Len(t, cs, 1)
	v, _, _ := unstructured.NestedString(cs[0].(map[string]any), "resources", field, "memory")
	return v
}

func TestPreserveLiveMemoryLimits(t *testing.T) {
	cases := []struct {
		name        string
		live        *unstructured.Unstructured // nil: nothing on the cluster yet
		wantLimit   string
		wantRequest string
		wantNote    bool
	}{
		{name: "fresh install: bundled limit applied", live: nil, wantLimit: "512Mi", wantRequest: "256Mi"},
		{name: "live limit raised: kept, request untouched, narrated", live: memDeployment(t, "1Gi", "4Gi"), wantLimit: "4Gi", wantRequest: "256Mi", wantNote: true},
		{name: "live limit below bundled request: kept, request lowered to it", live: memDeployment(t, "", "128Mi"), wantLimit: "128Mi", wantRequest: "128Mi", wantNote: true},
		{name: "live limit equals bundle: unchanged, not narrated", live: memDeployment(t, "256Mi", "512Mi"), wantLimit: "512Mi", wantRequest: "256Mi"},
		{name: "live has no memory limit: bundled limit applied", live: memDeployment(t, "256Mi", ""), wantLimit: "512Mi", wantRequest: "256Mi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []runtime.Object
			if tc.live != nil {
				objs = append(objs, tc.live)
			}
			dyn := dynfake.NewSimpleDynamicClient(scheme.Scheme, objs...)
			doc := memDeployment(t, "256Mi", "512Mi")

			var notes []string
			err := preserveLiveMemoryLimits(context.Background(), dyn, doc, func(f string, a ...any) {
				notes = append(notes, fmt.Sprintf(f, a...))
			})
			require.NoError(t, err)

			assert.Equal(t, tc.wantLimit, containerMemory(t, doc, "limits"))
			assert.Equal(t, tc.wantRequest, containerMemory(t, doc, "requests"))
			assert.Equal(t, tc.wantNote, len(notes) == 1, "notes: %v", notes)
		})
	}
}

func TestPreserveLiveMemoryLimits_NonWorkloadKindIsUntouched(t *testing.T) {
	doc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "op", "namespace": "ns"},
	}}
	before := doc.DeepCopy()
	require.NoError(t, preserveLiveMemoryLimits(context.Background(), dynfake.NewSimpleDynamicClient(scheme.Scheme), doc, nil))
	assert.Equal(t, before, doc)
}
