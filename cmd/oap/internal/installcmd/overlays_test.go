package installcmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests/overlay"
)

// memoryOverlayDir writes a kustomize Component raising container's memory
// limit on the named Deployment.
func memoryOverlayDir(t *testing.T, deployment, container, limit string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(
		"apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\npatches:\n- path: memory.yaml\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.yaml"), []byte(
		"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: "+deployment+"\n  namespace: agentprimitives-system\n"+
			"spec:\n  template:\n    spec:\n      containers:\n      - name: "+container+"\n        resources:\n          limits:\n            memory: "+limit+"\n"), 0o644))
	return dir
}

func containerMemoryLimit(t *testing.T, obj map[string]any, container string) string {
	t.Helper()
	cs, _, err := unstructured.NestedSlice(obj, "spec", "template", "spec", "containers")
	require.NoError(t, err)
	for _, c := range cs {
		m := c.(map[string]any)
		if m["name"] == container {
			v, _, err := unstructured.NestedString(m, "resources", "limits", "memory")
			require.NoError(t, err)
			return v
		}
	}
	require.FailNowf(t, "missing container", "%s", container)
	return ""
}

func TestLoadOverlays_NoDirsIsNilAndSilent(t *testing.T) {
	var out bytes.Buffer
	s, err := loadOverlays(&out, overlay.Base, nil)
	require.NoError(t, err)
	assert.Nil(t, s)
	assert.Empty(t, out.String())
}

func TestLoadOverlays_NarratesPatchedResources(t *testing.T) {
	var out bytes.Buffer
	s, err := loadOverlays(&out, overlay.Base, []string{memoryOverlayDir(t, "spicebox-operator", "operator", "2Gi")})
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Contains(t, out.String(), "overlay patches Deployment.apps agentprimitives-system/spicebox-operator")
}

func TestApplyManifestGroups_OverlayAppliedBeforeInstallRewrites(t *testing.T) {
	ov, err := loadOverlays(io.Discard, overlay.Base, []string{memoryOverlayDir(t, "spicebox-channelsd", "channelsd", "1Gi")})
	require.NoError(t, err)
	groups, err := manifests.ChannelsD()
	require.NoError(t, err)

	s := runtime.NewScheme()
	require.NoError(t, scheme.AddToScheme(s))
	dyn := dynfake.NewSimpleDynamicClient(s)
	firstParty := map[string]bool{apimage.Channelsd.Name + ":dev": true}
	require.NoError(t, applyManifestGroups(context.Background(), dyn, groups, ov, "pull-secret", firstParty, nil, nil))

	got, err := dyn.Resource(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}).
		Namespace("agentprimitives-system").Get(context.Background(), "spicebox-channelsd", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "1Gi", containerMemoryLimit(t, got.Object, "channelsd"))
	secrets, _, err := unstructured.NestedSlice(got.Object, "spec", "template", "spec", "imagePullSecrets")
	require.NoError(t, err)
	assert.Equal(t, []any{map[string]any{"name": "pull-secret"}}, secrets, "install's own rewrite survives the overlay")
}

func TestRender_Overlays(t *testing.T) {
	offline := &apcmd.Globals{BundleFn: func() (*kube.Bundle, error) {
		panic("render must not touch the cluster")
	}}
	gke := func(out string, overlays ...string) RenderConfig {
		return RenderConfig{OutputDir: out, ClusterKind: cloud.KeyGKE, NoDigestPin: true, Overlays: overlays}
	}

	t.Run("overlay is copied, listed as a component, and wins in the build", func(t *testing.T) {
		out := t.TempDir()
		ov := memoryOverlayDir(t, "spicebox-operator", "operator", "2Gi")
		require.NoError(t, runRender(context.Background(), io.Discard, offline, gke(out, ov), false))

		rel := userOverlayPath(0, ov)
		assert.FileExists(t, filepath.Join(out, rel, "kustomization.yaml"))
		k, err := os.ReadFile(filepath.Join(out, "kustomization.yaml"))
		require.NoError(t, err)
		assert.Contains(t, string(k), "components:\n  - "+rel+"\n")

		built, err := buildOverlay(out)
		require.NoError(t, err)
		docs, err := manifests.Split(built)
		require.NoError(t, err)
		var found bool
		for _, d := range docs {
			if d.GetKind() == "Deployment" && d.GetName() == "spicebox-operator" {
				found = true
				assert.Equal(t, "2Gi", containerMemoryLimit(t, d.Object, "operator"))
			}
		}
		assert.True(t, found, "operator Deployment in the built output")
	})

	t.Run("re-render without the overlay: copy removed, not left behind", func(t *testing.T) {
		out := t.TempDir()
		ov := memoryOverlayDir(t, "spicebox-operator", "operator", "2Gi")
		require.NoError(t, runRender(context.Background(), io.Discard, offline, gke(out, ov), false))
		require.NoError(t, runRender(context.Background(), io.Discard, offline, gke(out), false))
		assert.NoDirExists(t, filepath.Join(out, userOverlaysDir))
	})

	t.Run("conflicting overlays: refused before anything is written", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "deploy")
		err := runRender(context.Background(), io.Discard, offline, gke(out,
			memoryOverlayDir(t, "spicebox-operator", "operator", "2Gi"),
			memoryOverlayDir(t, "spicebox-operator", "operator", "4Gi")), false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "overlays conflict")
		assert.NoDirExists(t, out)
	})

	t.Run("overlay for a component render does not emit: refused up front, naming the flag", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "deploy")
		pg := memoryOverlayDir(t, "spicebox-postgres", "postgres", "2Gi")
		err := runRender(context.Background(), io.Discard, offline, gke(out, pg), false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spicebox-postgres")
		assert.Contains(t, err.Error(), "--with-postgres")
		assert.NoDirExists(t, out)

		cfg := gke(out, pg)
		cfg.WithPostgres = true
		require.NoError(t, runRender(context.Background(), io.Discard, offline, cfg, false))
	})

	t.Run("render and install produce the same overlaid fields", func(t *testing.T) {
		out := t.TempDir()
		ov := memoryOverlayDir(t, "spicebox-operator", "operator", "2Gi")
		require.NoError(t, runRender(context.Background(), io.Discard, offline, gke(out, ov), false))
		built, err := buildOverlay(out)
		require.NoError(t, err)
		rendered := findDoc(t, built, "Deployment", "spicebox-operator")

		set, err := loadOverlays(io.Discard, overlay.Base, []string{ov})
		require.NoError(t, err)
		installed := findDoc(t, manifests.Install, "Deployment", "spicebox-operator")
		require.NoError(t, set.Apply(installed))

		resources := func(d *unstructured.Unstructured) any {
			cs, _, err := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
			require.NoError(t, err)
			return cs[0].(map[string]any)["resources"]
		}
		assert.Equal(t, resources(installed), resources(rendered))
	})

	t.Run("overlay inside the output's overlays dir: refused", func(t *testing.T) {
		out := t.TempDir()
		inside := filepath.Join(out, userOverlaysDir, "mine")
		require.NoError(t, os.MkdirAll(inside, 0o755))
		src := memoryOverlayDir(t, "spicebox-operator", "operator", "2Gi")
		require.NoError(t, os.CopyFS(inside, os.DirFS(src)))
		err := runRender(context.Background(), io.Discard, offline, gke(out, inside), false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "keep overlays outside the output directory")
		assert.DirExists(t, inside, "the source overlay must not be deleted")
	})
}

func findDoc(t *testing.T, yaml []byte, kind, name string) *unstructured.Unstructured {
	t.Helper()
	docs, err := manifests.Split(yaml)
	require.NoError(t, err)
	for _, d := range docs {
		if d.GetKind() == kind && d.GetName() == name {
			return d
		}
	}
	require.FailNowf(t, "missing doc", "%s %s", kind, name)
	return nil
}
