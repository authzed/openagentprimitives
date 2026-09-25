// Package local implements the cloud.Strategy for the lightweight developer
// kind: sqlite memory, an in-memory SpiceDB datastore, a file:// artifact PVC,
// local :dev images, no digest pinning. It is opt-in ONLY (--local / oap
// desktop): it reports no ProviderIDPrefix, so cloud.Detect can never select
// it and always falls through to pkg/platform/cloud/unmanaged instead.
package local

import (
	"context"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud/certmanager"
)

func init() {
	cloud.Register(Strategy{}, cloud.KeyLocal)
}

// Strategy is the local cloud.Strategy implementation.
type Strategy struct{}

// Key returns the `local` kind key. local is opt-in ONLY (--local, oap desktop):
// it reports no ProviderIDPrefix, so Detect skips it and falls back to the
// `default` kind instead.
func (Strategy) Key() string { return cloud.KeyLocal }

// DisplayName returns a human-readable label for messages.
func (Strategy) DisplayName() string { return "local development cluster" }

// InstallProfile returns the lightweight developer profile: sqlite memory, an
// in-memory SpiceDB datastore, a file:// artifact PVC, local :dev images, and
// no digest pinning.
func (Strategy) InstallProfile() cloud.InstallProfile { return cloud.DevProfile }

// Validate is defined in validate.go — the whole method (managed-providerID
// refusal + kubeconfig host-pattern heuristic) lives there in one place.

// IsManaged reports false — this strategy covers non-managed clusters.
func (Strategy) IsManaged() bool { return false }

// ProviderIDPrefix returns "" — local is opt-in only and reports no fixed
// providerID prefix, so cloud.Detect can never select it and always falls
// through to pkg/platform/cloud/unmanaged instead.
func (Strategy) ProviderIDPrefix() string { return "" }

// DNSEgressCIDRs returns nil; no cloud-specific DNS egress ranges needed.
func (Strategy) DNSEgressCIDRs() []string { return nil }

// GatewayBackendIngressCIDRs returns nil; no cloud-specific LB health-check
// CIDRs needed.
func (Strategy) GatewayBackendIngressCIDRs() []string { return nil }

// GatewayAddressWait returns the address-wait budget for the bundled Envoy
// Gateway's load balancer, which comes up faster than GKE's managed global L7,
// so a genuine stall stays detectable promptly.
func (Strategy) GatewayAddressWait() (deadline, eta time.Duration) {
	return 8 * time.Minute, 7 * time.Minute
}

// RegistryFromProviderID returns "" because local clusters have no
// cloud-native registry derivable from a providerID.
func (Strategy) RegistryFromProviderID(_ string) string { return "" }

// ProjectFromProviderID returns "" because local clusters have no GCP
// project concept.
func (Strategy) ProjectFromProviderID(_ string) string { return "" }

// ResolveClusterIdentity returns the zero identity: a local cluster has no
// cloud that knows its name and no console to link to. (desktop embeds this
// Strategy and inherits the same answer.)
func (Strategy) ResolveClusterIdentity(_ context.Context, _ cloud.ClusterIdentityParams) cloud.ClusterIdentity {
	return cloud.ClusterIdentity{}
}

// TLS returns the cert-manager + Let's Encrypt strategy, the default for
// any cluster accessible over the public internet with HTTP-01.
func (Strategy) TLS() cloud.TLSStrategy { return certmanager.Strategy{} }

// WorkspaceStorage returns the local workspace storage resolver. It has no
// preferred cloud-native RWX driver, so it always falls back to the bundled
// local-path provisioner.
func (Strategy) WorkspaceStorage() cloud.WorkspaceStorage { return localWorkspaceStorage{} }

// StatefulStorage trusts the cluster default RWO StorageClass. (GKE swaps this
// to a real resolver in stateful.go.)
func (Strategy) StatefulStorage() cloud.StatefulStorage { return cloud.NoopStatefulStorage{} }

// ArtifactStorage returns the local kind's on-disk store. Unlike the other
// kinds, local provisions its own: a file:// PVC needs no cloud object store
// and no --artifact-store-url.
func (Strategy) ArtifactStorage() cloud.ArtifactStorage { return artifactStorage{} }

// UnwedgeTerminatingNamespace is a no-op on local clusters: there is
// no cloud controller leaving dangling finalizers.
func (Strategy) UnwedgeTerminatingNamespace(context.Context, cloud.Clients, cloud.Reporter, string, bool) (cloud.UnwedgeReport, error) {
	return cloud.UnwedgeReport{}, nil
}

// EnsureGatewayController installs the bundled Envoy Gateway controller and
// creates its GatewayClass. local clusters have no managed Gateway
// controller, so the generic shared helper does the work.
func (Strategy) EnsureGatewayController(ctx context.Context, p cloud.GatewayControllerParams) (cloud.GatewayControllerResult, error) {
	return cloud.EnsureEnvoyGatewayController(ctx, p)
}

// localWorkspaceStorage is the local cloud.WorkspaceStorage implementation.
type localWorkspaceStorage struct{}

// Resolve decides the RWX storage class for local clusters. There is no
// preferred cloud-native driver; it looks for any known RWX class and falls
// back to the bundled local-path provisioner if none is found.
func (localWorkspaceStorage) Resolve(ctx context.Context, p cloud.WorkspaceParams) (cloud.Decision, error) {
	return cloud.ResolveRWXOrBundled(ctx, p.Clients.Typed, nil)
}
