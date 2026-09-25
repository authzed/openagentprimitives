package local

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

const (
	artifactMountPath = "/var/lib/ap/artifacts"
	artifactPVCSize   = "5Gi"

	// create_dir + no_tmp_dir are fileblob driver options; blobstore.Open strips
	// query params from the ref base, so refs stay file:///var/lib/ap/artifacts/<key>.
	//
	// no_tmp_dir makes fileblob write its commit temp file NEXT TO the final
	// object (under the writable PVC) instead of the OS temp dir. The operator
	// container runs readOnlyRootFilesystem: true with no writable /tmp, so the
	// default temp write fails with "read-only file system" and crash-loops the
	// operator's artifact-store startup probe (envtest can't catch this — the
	// in-process operator has a real /tmp). Writing next to the target also keeps
	// the commit's temp→final rename on one filesystem, so it stays atomic.
	artifactStoreURL = "file://" + artifactMountPath + "?create_dir=true&no_tmp_dir=true"
)

// artifactStorage is the local kind's dev-grade on-disk artifact store: a small
// RWO PVC on the operator, which survives restarts AND rescheduling (the
// operator is single-replica, so RWO is fine).
type artifactStorage struct{}

// Ensure needs no cloud calls — the store is a PVC in the install bundle. It
// returns the URL together with the mount that makes the URL durable.
func (artifactStorage) Ensure(context.Context, cloud.ArtifactStorageParams) (cloud.ArtifactStorageResult, error) {
	return cloud.ArtifactStorageResult{
		URL: artifactStoreURL,
		RequiresOperatorVolume: &cloud.OperatorVolumeRequest{
			MountPath: artifactMountPath,
			MinSize:   artifactPVCSize,
		},
	}, nil
}

// Teardown is a no-op: the PVC is namespaced and dies with the namespace at
// `oap clean` time, so there is nothing to report or delete here.
func (artifactStorage) Teardown(context.Context, cloud.ArtifactTeardownParams) (cloud.ArtifactTeardownResult, error) {
	return cloud.ArtifactTeardownResult{}, nil
}
