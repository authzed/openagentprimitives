package cloud

import (
	"context"
	"fmt"
	"io"
)

// ArtifactStorage ensures durable artifact storage exists for the operator on
// a given cloud and returns the store URL to inject as the operator's
// ARTIFACT_STORE_URL. Unlike StatefulStorage (pure detection), Ensure MAY
// perform consented cloud-CLI side effects (bucket create, workload-identity
// enable, IAM binding) — the EnsureGatewayController contract, not the
// Resolve one. Teardown reports or removes the storage at `oap clean` time and
// must NEVER delete storage it cannot prove oap created.
type ArtifactStorage interface {
	Ensure(ctx context.Context, p ArtifactStorageParams) (ArtifactStorageResult, error)
	Teardown(ctx context.Context, p ArtifactTeardownParams) (ArtifactTeardownResult, error)
}

// ArtifactStorageParams carries what Ensure needs: cluster clients, the
// reporter, stdin + --yes for cloud.Confirm, and the workload-identity
// principal (operator ServiceAccount) the store must be writable by.
type ArtifactStorageParams struct {
	Clients                Clients
	Reporter               Reporter
	In                     io.Reader
	AssumeYes              bool
	OperatorNamespace      string
	OperatorServiceAccount string
}

// ArtifactStorageResult is Ensure's outcome: the URL to inject, plus any mount
// the operator needs for it.
type ArtifactStorageResult struct {
	URL string

	// RequiresOperatorVolume, when non-nil, means the store is backed by a
	// filesystem path that the operator pod must have mounted durably. Set by
	// backends returning a file:// URL; nil for object-store backends.
	//
	// It lives here rather than on InstallProfile so that the backend which
	// CHOSE the path is the one that declares it must be a mount — both halves
	// of one decision in one implementation, so they cannot drift.
	RequiresOperatorVolume *OperatorVolumeRequest
}

// OperatorVolumeRequest is a filesystem-backed store's mount requirement. It
// deliberately names only a path and a size: the PVC's name, namespace, and
// access mode are install-bundle shape, which is cmd/oap's domain, not this
// package's.
type OperatorVolumeRequest struct {
	MountPath string
	MinSize   string // resource.Quantity syntax, e.g. "5Gi"
}

// ArtifactTeardownParams carries what Teardown needs at `oap clean` time.
type ArtifactTeardownParams struct {
	Clients  Clients
	Reporter Reporter
	URL      string
	Delete   bool
}

// ArtifactTeardownResult reports what happened; KeptMessage is surfaced as
// one loud line so a kept bucket is never silent.
type ArtifactTeardownResult struct {
	Deleted     bool
	KeptMessage string
}

// RequireExplicitArtifactStorage is the shared non-automated fallback
// (local/unmanaged, EKS, AKS until their automation lands): Ensure fails
// closed telling the user exactly what to provide; Teardown never deletes.
type RequireExplicitArtifactStorage struct {
	CloudName string // display name for the error message
	Example   string // example URL, e.g. "s3://<bucket>"
}

func (r RequireExplicitArtifactStorage) Ensure(context.Context, ArtifactStorageParams) (ArtifactStorageResult, error) {
	return ArtifactStorageResult{}, fmt.Errorf(
		"no automated artifact storage for %s: pre-provision durable object storage and re-run with --artifact-store-url %s (the operator pod needs ambient credentials for it), or use `oap init --local` for a dev-grade on-disk store",
		r.CloudName, r.Example)
}

func (r RequireExplicitArtifactStorage) Teardown(_ context.Context, p ArtifactTeardownParams) (ArtifactTeardownResult, error) {
	if p.URL == "" {
		return ArtifactTeardownResult{}, nil
	}
	return ArtifactTeardownResult{
		KeptMessage: fmt.Sprintf("artifact store %s was not created by oap; left untouched", p.URL),
	}, nil
}
