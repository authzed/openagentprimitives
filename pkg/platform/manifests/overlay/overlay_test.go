package overlay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

const componentHeader = "apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\n"

// writeOverlay materializes an overlay directory holding kustomization (the
// full kustomization.yaml text) and each named file.
func writeOverlay(t *testing.T, kustomization string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(kustomization), 0o644))
	for name, body := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
	}
	return dir
}

// operatorMemoryOverlay raises the operator container's memory limit to limit.
func operatorMemoryOverlay(t *testing.T, limit string) string {
	t.Helper()
	return writeOverlay(t, componentHeader+"patches:\n- path: memory.yaml\n", map[string]string{
		"memory.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: spicebox-operator\n  namespace: agentprimitives-system\n" +
			"spec:\n  template:\n    spec:\n      containers:\n      - name: operator\n        resources:\n          limits:\n            memory: " + limit + "\n",
	})
}

func webdMemoryOverlay(t *testing.T, limit string) string {
	t.Helper()
	return writeOverlay(t, componentHeader+"patches:\n- path: memory.yaml\n", map[string]string{
		"memory.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: spicebox-webd\n  namespace: agentprimitives-system\n" +
			"spec:\n  template:\n    spec:\n      containers:\n      - name: webd\n        resources:\n          limits:\n            memory: " + limit + "\n",
	})
}

func testBase(t *testing.T) [][]byte {
	t.Helper()
	base, err := Base()
	require.NoError(t, err)
	return base
}

// bundledDoc returns the named document from the embedded install bundle, as
// install would hold it just before applying.
func bundledDoc(t *testing.T, kind, name string) *unstructured.Unstructured {
	t.Helper()
	docs, err := manifests.Split(manifests.Install)
	require.NoError(t, err)
	for _, d := range docs {
		if d.GetKind() == kind && d.GetName() == name {
			return d
		}
	}
	require.FailNowf(t, "missing bundled doc", "%s %s", kind, name)
	return nil
}

func container(t *testing.T, d *unstructured.Unstructured, name string) map[string]any {
	t.Helper()
	cs, _, err := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	require.NoError(t, err)
	for _, c := range cs {
		if m := c.(map[string]any); m["name"] == name {
			return m
		}
	}
	require.FailNowf(t, "missing container", "%s", name)
	return nil
}

func memory(t *testing.T, c map[string]any, field string) string {
	t.Helper()
	v, _, err := unstructured.NestedString(c, "resources", field, "memory")
	require.NoError(t, err)
	return v
}

func TestLoad_NoOverlaysIsNilAndApplyIsNoOp(t *testing.T) {
	s, err := Load(testBase(t), nil)
	require.NoError(t, err)
	assert.Nil(t, s)

	d := bundledDoc(t, "Deployment", "spicebox-operator")
	before := d.DeepCopy()
	require.NoError(t, s.Apply(d))
	assert.Equal(t, before, d)
}

func TestApply_RaisesOperatorMemoryAndKeepsInstallRewrites(t *testing.T) {
	s, err := Load(testBase(t), []string{operatorMemoryOverlay(t, "2Gi")})
	require.NoError(t, err)
	assert.Equal(t, []string{"Deployment.apps agentprimitives-system/spicebox-operator"}, s.Patched())

	// Install rewrites the image before applying; the overlay must not undo it.
	d := bundledDoc(t, "Deployment", "spicebox-operator")
	c := container(t, d, "operator")
	c["image"] = "registry.example/spicebox-operator@sha256:abc"
	cs, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	for i := range cs {
		if cs[i].(map[string]any)["name"] == "operator" {
			cs[i] = c
		}
	}
	require.NoError(t, unstructured.SetNestedSlice(d.Object, cs, "spec", "template", "spec", "containers"))

	require.NoError(t, s.Apply(d))
	got := container(t, d, "operator")
	assert.Equal(t, "2Gi", memory(t, got, "limits"))
	assert.Equal(t, "256Mi", memory(t, got, "requests"), "a field the overlay did not name keeps its bundled value")
	assert.Equal(t, "registry.example/spicebox-operator@sha256:abc", got["image"])
}

func TestApply_UntouchedResourceIsUnchanged(t *testing.T) {
	s, err := Load(testBase(t), []string{operatorMemoryOverlay(t, "2Gi")})
	require.NoError(t, err)
	d := bundledDoc(t, "Deployment", "spicebox-webd")
	before := d.DeepCopy()
	require.NoError(t, s.Apply(d))
	assert.Equal(t, before, d)
}

func TestLoad_Validation(t *testing.T) {
	cases := []struct {
		name    string
		dirs    func(t *testing.T) []string
		wantErr []string // substrings; empty means Load succeeds
	}{
		{
			name: "two overlays on different resources: accepted",
			dirs: func(t *testing.T) []string {
				return []string{operatorMemoryOverlay(t, "2Gi"), webdMemoryOverlay(t, "1Gi")}
			},
		},
		{
			name: "two overlays agreeing on a field: accepted",
			dirs: func(t *testing.T) []string {
				return []string{operatorMemoryOverlay(t, "2Gi"), operatorMemoryOverlay(t, "2Gi")}
			},
		},
		{
			name: "two overlays setting one field differently: conflict naming both and the field",
			dirs: func(t *testing.T) []string {
				return []string{operatorMemoryOverlay(t, "2Gi"), operatorMemoryOverlay(t, "4Gi")}
			},
			wantErr: []string{"overlays conflict", "spicebox-operator", "/[operator]/resources/limits/memory", `"2Gi"`, `"4Gi"`},
		},
		{
			name: "patch naming a resource oap does not ship: refused",
			dirs: func(t *testing.T) []string {
				return []string{writeOverlay(t, componentHeader+"patches:\n- path: p.yaml\n", map[string]string{
					"p.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: spicebox-operatr\n  namespace: agentprimitives-system\nspec:\n  replicas: 2\n",
				})}
			},
			wantErr: []string{"spicebox-operatr"},
		},
		{
			name: "overlay adding a resource: refused",
			dirs: func(t *testing.T) []string {
				return []string{writeOverlay(t, componentHeader+"resources:\n- cm.yaml\n", map[string]string{
					"cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: extra\n  namespace: agentprimitives-system\n",
				})}
			},
			wantErr: []string{"adds ConfigMap agentprimitives-system/extra", "may only modify"},
		},
		{
			name: "classic overlay (kind: Kustomization): refused with how to fix",
			dirs: func(t *testing.T) []string {
				return []string{writeOverlay(t, "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- ../base\n", nil)}
			},
			wantErr: []string{"must be a kustomize Component"},
		},
		{
			name: "directory without a kustomization: refused",
			dirs: func(t *testing.T) []string {
				return []string{t.TempDir()}
			},
			wantErr: []string{"no kustomization.yaml found"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(testBase(t), tc.dirs(t))
			if len(tc.wantErr) == 0 {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, want := range tc.wantErr {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestChangedLeaves_KeysNamedListsByName(t *testing.T) {
	a := map[string]any{"cs": []any{
		map[string]any{"name": "x", "v": "1"},
		map[string]any{"name": "y", "v": "1"},
	}}
	b := map[string]any{"cs": []any{
		map[string]any{"name": "x", "v": "1"},
		map[string]any{"name": "y", "v": "2"},
		map[string]any{"name": "z", "v": "3"},
	}}
	got := changedLeaves(a, b)
	assert.Equal(t, map[string]any{"/cs/[y]/v": "2", "/cs/[z]/name": "z", "/cs/[z]/v": "3"}, got)
}

// An overlay that changes a list with no merge key (container args) must not
// carry the bundle's copy of that list along with it: install rewrites the
// runner image ref inside those args before applying, and the overlay is laid
// over that rewritten document, not over the bundle.
func TestApply_ListWithoutMergeKeyKeepsInstallRewrites(t *testing.T) {
	dir := writeOverlay(t, componentHeader+"patches:\n- target:\n    kind: Deployment\n    name: spicebox-operator\n  patch: |-\n    - op: add\n      path: /spec/template/spec/containers/0/args/-\n      value: --extra-flag\n", nil)
	s, err := Load(testBase(t), []string{dir})
	require.NoError(t, err)

	d := bundledDoc(t, "Deployment", "spicebox-operator")
	raw, err := json.Marshal(d.Object)
	require.NoError(t, err)
	rewritten := strings.ReplaceAll(string(raw), "agentprimitives-runner:dev", "registry.example/agentprimitives-runner@sha256:abc")
	require.NoError(t, json.Unmarshal([]byte(rewritten), &d.Object))

	require.NoError(t, s.Apply(d))
	args, _, err := unstructured.NestedStringSlice(container(t, d, "operator"), "args")
	require.NoError(t, err)
	assert.Contains(t, args, "--extra-flag")
	assert.Contains(t, args, "--runner-image=registry.example/agentprimitives-runner@sha256:abc")
	assert.NotContains(t, args, "--runner-image=agentprimitives-runner:dev")
}

// A kind outside the client-go scheme (oap's own CRs) goes through the same
// rebuild: the overlay's change lands and install's substitution of an image
// ref elsewhere in the resource survives.
func TestApply_CustomResourceKeepsInstallRewrites(t *testing.T) {
	dir := writeOverlay(t, componentHeader+"patches:\n- target:\n    kind: SpiceboxToolchain\n    name: claude\n  patch: |-\n    - op: add\n      path: /spec/provides/-\n      value: claude-extra\n", nil)
	s, err := Load(testBase(t), []string{dir})
	require.NoError(t, err)
	assert.Equal(t, []string{"SpiceboxToolchain.agentprimitives.authzed.com claude"}, s.Patched())

	d := bundledDoc(t, "SpiceboxToolchain", "claude")
	require.NoError(t, unstructured.SetNestedField(d.Object, "registry.example/ap-toolchain-claude@sha256:abc", "spec", "source", "image"))
	require.NoError(t, s.Apply(d))

	provides, _, err := unstructured.NestedStringSlice(d.Object, "spec", "provides")
	require.NoError(t, err)
	assert.Equal(t, []string{"claude", "claude-code", "claude-extra"}, provides)
	image, _, err := unstructured.NestedString(d.Object, "spec", "source", "image")
	require.NoError(t, err)
	assert.Equal(t, "registry.example/ap-toolchain-claude@sha256:abc", image)
}

func TestLoad_Refusals(t *testing.T) {
	t.Run("symlink inside an overlay: refused as not a regular file", func(t *testing.T) {
		dir := operatorMemoryOverlay(t, "2Gi")
		require.NoError(t, os.Symlink(filepath.Join(dir, "memory.yaml"), filepath.Join(dir, "link.yaml")))
		_, err := Load(testBase(t), []string{dir})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "link.yaml is not a regular file")
	})

	t.Run("one overlay removes a field another sets: conflict shows the removal", func(t *testing.T) {
		remove := writeOverlay(t, componentHeader+"patches:\n- target:\n    kind: Deployment\n    name: spicebox-operator\n  patch: |-\n    - op: remove\n      path: /spec/template/spec/containers/0/resources/limits/memory\n", nil)
		_, err := Load(testBase(t), []string{remove, operatorMemoryOverlay(t, "4Gi")})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "/[operator]/resources/limits/memory")
		assert.Contains(t, err.Error(), "(removed)")
		assert.Contains(t, err.Error(), `"4Gi"`)
	})
}

func TestChangedLeaves_KeysListsByFirstFittingMergeKey(t *testing.T) {
	a := map[string]any{"mounts": []any{
		map[string]any{"mountPath": "/a", "readOnly": true},
		map[string]any{"mountPath": "/b", "readOnly": true},
	}}
	b := map[string]any{"mounts": []any{
		map[string]any{"mountPath": "/a", "readOnly": true},
		map[string]any{"mountPath": "/b", "readOnly": false},
	}}
	assert.Equal(t, map[string]any{"/mounts/[/b]/readOnly": false}, changedLeaves(a, b))
}
