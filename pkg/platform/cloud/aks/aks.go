// Package aks implements the cloud.Strategy for Azure Kubernetes Service clusters.
// It registers itself under the "aks" key so cloud.Detect can dispatch
// through the cloud.Strategy interface without any AKS-specific branches
// in cmd/oap.
package aks

import (
	"context"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud/certmanager"
)

func init() {
	cloud.Register(Strategy{}, "aks")
}

// Strategy is the AKS cloud.Strategy implementation.
type Strategy struct{}

// Key returns the registry key for this strategy.
func (Strategy) Key() string { return "aks" }

// DisplayName returns the human-readable label used in install messages.
func (Strategy) DisplayName() string { return "AKS" }

// IsManaged reports that AKS is a big-three managed cloud.
func (Strategy) IsManaged() bool { return true }

// ProviderIDPrefix is the azure:// prefix used on AKS node spec.providerIDs,
// used by cloud.Detect to identify AKS clusters.
func (Strategy) ProviderIDPrefix() string { return "azure://" }

// DNSEgressCIDRs returns nil; AKS uses in-cluster CoreDNS so no link-local
// exemption is needed.
func (Strategy) DNSEgressCIDRs() []string { return nil }

// GatewayBackendIngressCIDRs returns nil; AKS load balancer health-check
// CIDRs are not required at the cluster network-policy layer.
func (Strategy) GatewayBackendIngressCIDRs() []string { return nil }

// GatewayAddressWait returns the address-wait budget for the bundled Envoy
// Gateway's load balancer, which comes up faster than GKE's managed global L7,
// so a genuine stall stays detectable promptly.
func (Strategy) GatewayAddressWait() (deadline, eta time.Duration) {
	return 8 * time.Minute, 7 * time.Minute
}

// RegistryFromProviderID returns "" because ACR registry derivation is not
// supported via node providerID on AKS.
func (Strategy) RegistryFromProviderID(_ string) string { return "" }

// ProjectFromProviderID returns "" because AKS has no GCP project concept.
func (Strategy) ProjectFromProviderID(_ string) string { return "" }

// ResolveClusterIdentity returns the zero identity: an AKS node's providerID
// names the scale-set VM, not the cluster, so neither the cluster name nor a
// portal deep link is derivable from a node alone.
func (Strategy) ResolveClusterIdentity(_ context.Context, _ cloud.ClusterIdentityParams) cloud.ClusterIdentity {
	return cloud.ClusterIdentity{}
}

// TLS returns the cert-manager + Let's Encrypt strategy, which is the
// default for AKS (HTTP-01 through the Envoy Gateway).
func (Strategy) TLS() cloud.TLSStrategy { return certmanager.Strategy{} }

// WorkspaceStorage returns the AKS workspace storage resolver. It prefers
// the Azure Files CSI driver for native RWX, falling back to the bundled
// local-path provisioner.
func (Strategy) WorkspaceStorage() cloud.WorkspaceStorage { return aksWorkspaceStorage{} }

// StatefulStorage trusts the cluster default RWO StorageClass. (GKE swaps this
// to a real resolver in stateful.go.)
func (Strategy) StatefulStorage() cloud.StatefulStorage { return cloud.NoopStatefulStorage{} }

// ArtifactStorage: Azure Blob + workload-identity auto-provisioning is a
// tracked TODO; until then installs bring a container via --artifact-store-url.
func (Strategy) ArtifactStorage() cloud.ArtifactStorage {
	return cloud.RequireExplicitArtifactStorage{CloudName: "AKS", Example: "azblob://<container>"}
}

// UnwedgeTerminatingNamespace is a no-op on AKS today.
func (Strategy) UnwedgeTerminatingNamespace(context.Context, cloud.Clients, cloud.Reporter, string, bool) (cloud.UnwedgeReport, error) {
	return cloud.UnwedgeReport{}, nil
}

// EnsureGatewayController installs the bundled Envoy Gateway controller and
// creates its GatewayClass. AKS has no managed Gateway controller, so the
// generic shared helper does the work.
func (Strategy) EnsureGatewayController(ctx context.Context, p cloud.GatewayControllerParams) (cloud.GatewayControllerResult, error) {
	return cloud.EnsureEnvoyGatewayController(ctx, p)
}

// InstallProfile returns the managed production profile: the durable-cluster
// defaults, plus RequiresExternalHostname — a managed cloud with no external
// hostname is reachable only through `kubectl port-forward`.
func (Strategy) InstallProfile() cloud.InstallProfile { return cloud.ManagedProfile }

// Validate refuses a cluster whose nodes report a different managed cloud.
func (s Strategy) Validate(ctx context.Context, p cloud.ValidateParams) error {
	return cloud.ValidateManagedPrefix(ctx, s, p)
}

// aksWorkspaceStorage is the AKS cloud.WorkspaceStorage implementation.
type aksWorkspaceStorage struct{}

// Resolve decides the RWX storage class for AKS clusters. It first looks for
// a StorageClass backed by the Azure Files CSI driver (file.csi.azure.com); if
// none is present it falls back to the bundled local-path provisioner.
func (aksWorkspaceStorage) Resolve(ctx context.Context, p cloud.WorkspaceParams) (cloud.Decision, error) {
	return cloud.ResolveRWXOrBundled(ctx, p.Clients.Typed, []string{"file.csi.azure.com"})
}
