package installcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
)

func TestMirrorImageRefs(t *testing.T) {
	mm := apimage.MirrorMap("myreg.io/ap")
	d := &unstructured.Unstructured{Object: map[string]any{
		"kind": "Deployment",
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers":     []any{map[string]any{"name": "db", "image": "ghcr.io/authzed/spicedb:v1.56.2"}},
			"initContainers": []any{map[string]any{"name": "wait", "image": "busybox"}},
		}}},
	}}
	require.NoError(t, mirrorImageRefs(d, mm))
	cs, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	assert.Equal(t, "myreg.io/ap/ghcr.io-authzed-spicedb:v1.56.2", cs[0].(map[string]any)["image"])
	ics, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "initContainers")
	assert.Equal(t, "myreg.io/ap/busybox", ics[0].(map[string]any)["image"])

	// non-dependency image untouched
	d2 := &unstructured.Unstructured{Object: map[string]any{
		"kind": "Deployment",
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": "c", "image": "myreg.io/ap/spicebox-operator:dev"}},
		}}},
	}}
	require.NoError(t, mirrorImageRefs(d2, mm))
	cs2, _, _ := unstructured.NestedSlice(d2.Object, "spec", "template", "spec", "containers")
	assert.Equal(t, "myreg.io/ap/spicebox-operator:dev", cs2[0].(map[string]any)["image"])

	// nil map is a no-op
	d3 := &unstructured.Unstructured{Object: map[string]any{"kind": "Deployment", "spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "c", "image": "authzed/spicedb:latest"}}}}}}}
	require.NoError(t, mirrorImageRefs(d3, nil))
	cs3, _, _ := unstructured.NestedSlice(d3.Object, "spec", "template", "spec", "containers")
	assert.Equal(t, "authzed/spicedb:latest", cs3[0].(map[string]any)["image"])
}

func TestMirrorWorkspaceHelperImage(t *testing.T) {
	mm := apimage.MirrorMap("myreg.io/ap") // busybox → myreg.io/ap/busybox

	// Deployment with a --helper-image=busybox provisioner arg → rewritten.
	dep := &unstructured.Unstructured{Object: map[string]any{
		"kind": "Deployment",
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": "p", "args": []any{"--provisioner=x", "--helper-image=busybox"}}},
		}}},
	}}
	require.NoError(t, mirrorWorkspaceHelperImage(dep, mm))
	cs, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
	args := cs[0].(map[string]any)["args"].([]any)
	assert.Contains(t, anyToStrings(args), "--helper-image=myreg.io/ap/busybox")
	assert.NotContains(t, anyToStrings(args), "--helper-image=busybox")

	// ConfigMap with `image: busybox` inside the helperPod.yaml data → rewritten.
	cm := &unstructured.Unstructured{Object: map[string]any{
		"kind": "ConfigMap",
		"data": map[string]any{"helperPod.yaml": "spec:\n  containers:\n    - name: helper\n      image: busybox"},
	}}
	require.NoError(t, mirrorWorkspaceHelperImage(cm, mm))
	data, _, _ := unstructured.NestedMap(cm.Object, "data")
	assert.Contains(t, data["helperPod.yaml"].(string), "image: myreg.io/ap/busybox")
	assert.NotContains(t, data["helperPod.yaml"].(string), "image: busybox\n")

	// nil map → no-op (helper image unchanged).
	dep2 := &unstructured.Unstructured{Object: map[string]any{
		"kind": "Deployment",
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": "p", "args": []any{"--helper-image=busybox"}}},
		}}},
	}}
	require.NoError(t, mirrorWorkspaceHelperImage(dep2, nil))
	cs2, _, _ := unstructured.NestedSlice(dep2.Object, "spec", "template", "spec", "containers")
	assert.Contains(t, anyToStrings(cs2[0].(map[string]any)["args"].([]any)), "--helper-image=busybox")
}

func TestMirrorOperatorSnapshotImage(t *testing.T) {
	mm := apimage.MirrorMap("myreg.io/ap")
	operatorName := apimage.Operator.Name // "spicebox-operator"

	// Operator Deployment with snapshot image in mirrorMap → --snapshot-image arg appended.
	dep := &unstructured.Unstructured{Object: map[string]any{
		"kind": "Deployment",
		"metadata": map[string]any{
			"name": operatorName,
		},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": "operator", "args": []any{"--some-flag=x"}}},
		}}},
	}}
	require.NoError(t, mirrorOperatorSnapshotImage(dep, mm, operatorName))
	cs, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
	args := anyToStrings(cs[0].(map[string]any)["args"].([]any))
	assert.Contains(t, args, "--snapshot-image=myreg.io/ap/busybox:1.36")
	assert.Contains(t, args, "--some-flag=x")

	// Non-operator Deployment → untouched.
	dep2 := &unstructured.Unstructured{Object: map[string]any{
		"kind": "Deployment",
		"metadata": map[string]any{
			"name": "some-other-deployment",
		},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": "c", "args": []any{"--flag=y"}}},
		}}},
	}}
	require.NoError(t, mirrorOperatorSnapshotImage(dep2, mm, operatorName))
	cs2, _, _ := unstructured.NestedSlice(dep2.Object, "spec", "template", "spec", "containers")
	args2 := anyToStrings(cs2[0].(map[string]any)["args"].([]any))
	assert.NotContains(t, args2, "--snapshot-image=myreg.io/ap/busybox:1.36")

	// nil map → no-op.
	dep3 := &unstructured.Unstructured{Object: map[string]any{
		"kind": "Deployment",
		"metadata": map[string]any{
			"name": operatorName,
		},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": "operator", "args": []any{"--some-flag=z"}}},
		}}},
	}}
	require.NoError(t, mirrorOperatorSnapshotImage(dep3, nil, operatorName))
	cs3, _, _ := unstructured.NestedSlice(dep3.Object, "spec", "template", "spec", "containers")
	args3 := anyToStrings(cs3[0].(map[string]any)["args"].([]any))
	assert.NotContains(t, args3, "--snapshot-image=myreg.io/ap/busybox:1.36")
}
