package installcmd

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// artifactOperatorDoc builds a minimal spicebox-operator Deployment doc
// matching config/manager/deployment.yaml's identity (name spicebox-operator,
// container "operator"). Named distinctly from install_develop_test.go's
// variadic operatorDoc(env ...map[string]any) helper to avoid a duplicate
// declaration in this package; reuses that helper under the hood so both test
// files build the identical doc shape.
func artifactOperatorDoc() *unstructured.Unstructured {
	return operatorDoc(map[string]any{"name": "MEMORY_BACKEND", "value": "postgres"})
}

func TestSetOperatorEnvArtifactStoreURL(t *testing.T) {
	docs := []*unstructured.Unstructured{artifactOperatorDoc()}
	require.NoError(t, setOperatorEnv(docs, artifactStoreURLEnvName, "gs://b"))
	containers, _, _ := unstructured.NestedSlice(docs[0].Object, "spec", "template", "spec", "containers")
	env := containers[0].(map[string]any)["env"].([]any)
	found := false
	for _, e := range env {
		m := e.(map[string]any)
		if m["name"] == "ARTIFACT_STORE_URL" {
			found = true
			assert.Equal(t, "gs://b", m["value"])
		}
	}
	assert.True(t, found)
}

func TestInjectOperatorArtifactVolume(t *testing.T) {
	docs := []*unstructured.Unstructured{artifactOperatorDoc()}
	vol := cloud.OperatorVolumeRequest{MountPath: "/var/lib/ap/artifacts", MinSize: "5Gi"}
	require.NoError(t, injectOperatorArtifactVolume(docs, vol))

	vols, found, err := unstructured.NestedSlice(docs[0].Object, "spec", "template", "spec", "volumes")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, vols, 1)
	v := vols[0].(map[string]any)
	assert.Equal(t, localArtifactVolumeName, v["name"])
	assert.Equal(t, localArtifactPVCName, v["persistentVolumeClaim"].(map[string]any)["claimName"])

	containers, _, _ := unstructured.NestedSlice(docs[0].Object, "spec", "template", "spec", "containers")
	mounts := containers[0].(map[string]any)["volumeMounts"].([]any)
	m := mounts[0].(map[string]any)
	assert.Equal(t, localArtifactVolumeName, m["name"])
	assert.Equal(t, vol.MountPath, m["mountPath"])

	// idempotent on re-run (same names, no duplicates)
	require.NoError(t, injectOperatorArtifactVolume(docs, vol))
	vols, _, _ = unstructured.NestedSlice(docs[0].Object, "spec", "template", "spec", "volumes")
	assert.Len(t, vols, 1)
}

func TestLocalArtifactPVCDoc(t *testing.T) {
	d := localArtifactPVCDoc(cloud.OperatorVolumeRequest{MountPath: "/var/lib/ap/artifacts", MinSize: "5Gi"})
	assert.Equal(t, "PersistentVolumeClaim", d.GetKind())
	assert.Equal(t, localArtifactPVCName, d.GetName())
	assert.Equal(t, "agentprimitives-system", d.GetNamespace())
	modes, _, _ := unstructured.NestedStringSlice(d.Object, "spec", "accessModes")
	assert.Equal(t, []string{"ReadWriteOnce"}, modes)
	size, _, _ := unstructured.NestedString(d.Object, "spec", "resources", "requests", "storage")
	assert.Equal(t, "5Gi", size)
}

type fakeArtifactStorage struct {
	url string
	vol *cloud.OperatorVolumeRequest
	err error
}

func (f fakeArtifactStorage) Ensure(context.Context, cloud.ArtifactStorageParams) (cloud.ArtifactStorageResult, error) {
	return cloud.ArtifactStorageResult{URL: f.url, RequiresOperatorVolume: f.vol}, f.err
}
func (f fakeArtifactStorage) Teardown(context.Context, cloud.ArtifactTeardownParams) (cloud.ArtifactTeardownResult, error) {
	return cloud.ArtifactTeardownResult{}, nil
}

// fakeArtifactStrategy embeds the nil cloud.Strategy interface (the
// fakeUnwedgeStrategy pattern in clean_test.go) so it only needs to override
// the two methods resolveArtifactStoreURL actually calls.
type fakeArtifactStrategy struct {
	cloud.Strategy
	storage cloud.ArtifactStorage
}

func (f fakeArtifactStrategy) ArtifactStorage() cloud.ArtifactStorage { return f.storage }
func (f fakeArtifactStrategy) DisplayName() string                    { return "fake" }

func TestResolveArtifactStoreURLPrecedence(t *testing.T) {
	t.Run("explicit flag wins, no Ensure call", func(t *testing.T) {
		// strat is nil: if resolveArtifactStoreURL called through to
		// strat.ArtifactStorage(), this would panic — proving Ensure is skipped.
		url, vol, err := resolveArtifactStoreURL(context.Background(), nil, nil,
			ArtifactStoreOptions{ExplicitURL: "s3://mine"}, false, nil)
		require.NoError(t, err)
		assert.Equal(t, "s3://mine", url)
		assert.Nil(t, vol, "an explicit --artifact-store-url must yield no volume request")
	})
	t.Run("no explicit URL: the kind's Ensure result (URL + volume) flows through", func(t *testing.T) {
		strat := fakeArtifactStrategy{storage: fakeArtifactStorage{
			url: "file:///var/lib/ap/artifacts?create_dir=true&no_tmp_dir=true",
			vol: &cloud.OperatorVolumeRequest{MountPath: "/var/lib/ap/artifacts", MinSize: "5Gi"},
		}}
		url, vol, err := resolveArtifactStoreURL(context.Background(), &kube.Bundle{}, strat,
			ArtifactStoreOptions{}, false, nil)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(url, "file://"))
		require.NotNil(t, vol)
		assert.Equal(t, "/var/lib/ap/artifacts", vol.MountPath)
		assert.Equal(t, "5Gi", vol.MinSize)
	})
	t.Run("object-store kind: no volume request", func(t *testing.T) {
		strat := fakeArtifactStrategy{storage: fakeArtifactStorage{url: "gs://bucket"}}
		url, vol, err := resolveArtifactStoreURL(context.Background(), &kube.Bundle{}, strat,
			ArtifactStoreOptions{}, false, nil)
		require.NoError(t, err)
		assert.Equal(t, "gs://bucket", url)
		assert.Nil(t, vol)
	})
}
