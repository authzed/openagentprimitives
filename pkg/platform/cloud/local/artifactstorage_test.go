package local_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/local"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/unmanaged"
)

func TestLocalArtifactStorageReturnsAFileURLAndDemandsItsMount(t *testing.T) {
	res, err := cloud.MustFor(cloud.KeyLocal).ArtifactStorage().
		Ensure(context.Background(), cloud.ArtifactStorageParams{})
	require.NoError(t, err, "the local kind provisions its own dev-grade store; it must not fail closed")

	assert.True(t, strings.HasPrefix(res.URL, "file://"), "got %q", res.URL)
	assert.Contains(t, res.URL, "no_tmp_dir=true",
		"the operator runs readOnlyRootFilesystem: fileblob must commit next to the target, not in /tmp")
	assert.Contains(t, res.URL, "create_dir=true")

	require.NotNil(t, res.RequiresOperatorVolume,
		"a file:// store is only durable if the operator has that path mounted")
	assert.NotEmpty(t, res.RequiresOperatorVolume.MountPath)
	assert.Contains(t, res.URL, res.RequiresOperatorVolume.MountPath,
		"the declared mount path must be the one the URL actually writes to")
	assert.NotEmpty(t, res.RequiresOperatorVolume.MinSize)
}

func TestObjectStoreKindsDemandNoOperatorVolume(t *testing.T) {
	// RequireExplicitArtifactStorage fails closed, so there is no result to
	// inspect — the point is that it does NOT hand back a volume request.
	res, err := cloud.MustFor(cloud.KeyDefault).ArtifactStorage().
		Ensure(context.Background(), cloud.ArtifactStorageParams{})
	require.Error(t, err, "unmanaged clusters must still require --artifact-store-url")
	assert.Nil(t, res.RequiresOperatorVolume)
}
