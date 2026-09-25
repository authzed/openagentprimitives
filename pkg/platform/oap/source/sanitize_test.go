package source_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/pkg/platform/oap/source"
)

func TestSanitize(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]any
		wantAnnos   bool // whether metadata.annotations should survive at all
	}{
		{
			name: "last-applied alongside a real annotation survives",
			annotations: map[string]any{
				"kubectl.kubernetes.io/last-applied-configuration": `{"apiVersion":"v1"}`,
				"widget.example.com/note":                          "keep me",
			},
			wantAnnos: true,
		},
		{
			name: "last-applied is the only annotation removes the map",
			annotations: map[string]any{
				"kubectl.kubernetes.io/last-applied-configuration": `{"apiVersion":"v1"}`,
			},
			wantAnnos: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "widgets.example.com/v1",
					"kind":       "Widget",
					"metadata": map[string]any{
						"name":              "gizmo",
						"namespace":         "default",
						"resourceVersion":   "12345",
						"uid":               "fake-uid-0000",
						"generation":        int64(3),
						"creationTimestamp": "2026-01-01T00:00:00Z",
						"selfLink":          "/apis/widgets.example.com/v1/namespaces/default/widgets/gizmo",
						"managedFields": []any{
							map[string]any{"manager": "kubectl"},
						},
						"ownerReferences": []any{
							map[string]any{"kind": "Owner", "name": "owner-1"},
						},
						"finalizers": []any{
							"spicebox.authzed.com/example-finalizer",
						},
						"labels": map[string]any{
							"app": "gizmo",
						},
						"annotations": tc.annotations,
					},
					"spec": map[string]any{
						"replicas": int64(2),
					},
					"status": map[string]any{
						"phase": "Running",
					},
				},
			}

			source.Sanitize(u)

			// Removed: top-level status.
			_, statusFound, err := unstructured.NestedFieldNoCopy(u.Object, "status")
			require.NoError(t, err)
			assert.False(t, statusFound, "status should be removed")

			// Removed: server-managed metadata fields.
			for _, field := range []string{
				"resourceVersion", "uid", "generation", "creationTimestamp", "managedFields", "ownerReferences", "selfLink", "finalizers",
			} {
				_, found, err := unstructured.NestedFieldNoCopy(u.Object, "metadata", field)
				require.NoError(t, err)
				assert.Falsef(t, found, "metadata.%s should be removed", field)
			}

			// Removed: last-applied-configuration annotation specifically.
			annos, annosFound, err := unstructured.NestedStringMap(u.Object, "metadata", "annotations")
			require.NoError(t, err)
			if tc.wantAnnos {
				require.True(t, annosFound, "metadata.annotations should survive")
				_, hasLastApplied := annos["kubectl.kubernetes.io/last-applied-configuration"]
				assert.False(t, hasLastApplied, "last-applied-configuration annotation should be removed")
				assert.Equal(t, "keep me", annos["widget.example.com/note"], "other annotations should survive")
			} else {
				assert.False(t, annosFound, "metadata.annotations should be removed entirely once empty")
			}

			// Survives: apiVersion, kind, spec, metadata.name/namespace/labels.
			assert.Equal(t, "widgets.example.com/v1", u.Object["apiVersion"])
			assert.Equal(t, "Widget", u.Object["kind"])

			spec, specFound, err := unstructured.NestedMap(u.Object, "spec")
			require.NoError(t, err)
			require.True(t, specFound)
			assert.Equal(t, int64(2), spec["replicas"])

			assert.Equal(t, "gizmo", u.GetName())
			assert.Equal(t, "default", u.GetNamespace())

			labels, labelsFound, err := unstructured.NestedStringMap(u.Object, "metadata", "labels")
			require.NoError(t, err)
			require.True(t, labelsFound)
			assert.Equal(t, "gizmo", labels["app"])
		})
	}
}
