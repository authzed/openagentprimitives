package cloud

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequireExplicitArtifactStorage(t *testing.T) {
	r := RequireExplicitArtifactStorage{CloudName: "EKS", Example: "s3://<bucket>"}

	t.Run("Ensure fails closed naming the flag and the cloud", func(t *testing.T) {
		_, err := r.Ensure(context.Background(), ArtifactStorageParams{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--artifact-store-url")
		assert.Contains(t, err.Error(), "EKS")
		assert.Contains(t, err.Error(), "s3://<bucket>")
	})

	t.Run("Teardown never deletes; reports kept for a configured URL", func(t *testing.T) {
		res, err := r.Teardown(context.Background(), ArtifactTeardownParams{URL: "s3://b", Delete: true})
		require.NoError(t, err)
		assert.False(t, res.Deleted)
		assert.Contains(t, res.KeptMessage, "s3://b")
	})

	t.Run("Teardown with no URL is silent", func(t *testing.T) {
		res, err := r.Teardown(context.Background(), ArtifactTeardownParams{})
		require.NoError(t, err)
		assert.False(t, res.Deleted)
		assert.Empty(t, res.KeptMessage)
	})
}
