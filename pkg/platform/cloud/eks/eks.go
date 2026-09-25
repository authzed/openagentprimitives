// Package eks implements the cloud.Strategy for Amazon EKS clusters.
// It registers itself under the "eks" key so cloud.Detect can dispatch
// through the cloud.Strategy interface without any EKS-specific branches
// in cmd/oap.
package eks

import (
	"context"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud/certmanager"
)

func init() {
	cloud.Register(Strategy{}, "eks")
}

// Strategy is the EKS cloud.Strategy implementation.
type Strategy struct{}

// Key returns the registry key for this strategy.
func (Strategy) Key() string { return "eks" }

// DisplayName returns the human-readable label used in install messages.
func (Strategy) DisplayName() string { return "EKS" }

// IsManaged reports that EKS is a big-three managed cloud.
func (Strategy) IsManaged() bool { return true }

// ProviderIDPrefix is the aws:// prefix used on EKS node spec.providerIDs,
// used by cloud.Detect to identify EKS clusters.
func (Strategy) ProviderIDPrefix() string { return "aws://" }

// DNSEgressCIDRs returns nil; EKS uses in-cluster CoreDNS so no link-local
// exemption is needed.
func (Strategy) DNSEgressCIDRs() []string { return nil }

// GatewayBackendIngressCIDRs returns nil; EKS load balancer health-check
// CIDRs are not required at the cluster network-policy layer.
func (Strategy) GatewayBackendIngressCIDRs() []string { return nil }

// GatewayAddressWait returns the address-wait budget for the bundled Envoy
// Gateway's load balancer, which comes up faster than GKE's managed global L7,
// so a genuine stall stays detectable promptly.
func (Strategy) GatewayAddressWait() (deadline, eta time.Duration) {
	return 8 * time.Minute, 7 * time.Minute
}

// RegistryFromProviderID returns "" because ECR registry derivation is not
// supported via node providerID on EKS.
func (Strategy) RegistryFromProviderID(_ string) string { return "" }

// ProjectFromProviderID returns "" because EKS has no GCP project concept.
func (Strategy) ProjectFromProviderID(_ string) string { return "" }

// ResolveClusterIdentity returns the zero identity: an EKS node's providerID
// (aws:///ZONE/INSTANCE) names the instance, not the cluster, so neither the
// cluster name nor a console deep link is derivable from a node alone.
func (Strategy) ResolveClusterIdentity(_ context.Context, _ cloud.ClusterIdentityParams) cloud.ClusterIdentity {
	return cloud.ClusterIdentity{}
}

// TLS returns the cert-manager + Let's Encrypt strategy, which is the
// default for EKS (HTTP-01 through the Envoy Gateway).
func (Strategy) TLS() cloud.TLSStrategy { return certmanager.Strategy{} }

// WorkspaceStorage returns the EKS workspace storage resolver. It prefers
// the AWS EFS CSI driver for native RWX, falling back to the bundled
// local-path provisioner.
func (Strategy) WorkspaceStorage() cloud.WorkspaceStorage { return eksWorkspaceStorage{} }

// StatefulStorage trusts the cluster default RWO StorageClass. (GKE swaps this
// to a real resolver in stateful.go.)
func (Strategy) StatefulStorage() cloud.StatefulStorage { return cloud.NoopStatefulStorage{} }

// ArtifactStorage: S3 + IRSA auto-provisioning is a tracked TODO; until then
// installs bring a bucket via --artifact-store-url.
func (Strategy) ArtifactStorage() cloud.ArtifactStorage {
	return cloud.RequireExplicitArtifactStorage{CloudName: "EKS", Example: "s3://<bucket>"}
}

// UnwedgeTerminatingNamespace is a no-op on EKS today. The AWS Load Balancer
// Controller's elbv2.k8s.aws / service.k8s.aws/resources finalizers can wedge a
// namespace similarly; implementing that cleanup is a tracked follow-up.
func (Strategy) UnwedgeTerminatingNamespace(context.Context, cloud.Clients, cloud.Reporter, string, bool) (cloud.UnwedgeReport, error) {
	return cloud.UnwedgeReport{}, nil
}

// EnsureGatewayController installs the bundled Envoy Gateway controller and
// creates its GatewayClass. EKS has no managed Gateway controller, so the
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

// eksWorkspaceStorage is the EKS cloud.WorkspaceStorage implementation.
type eksWorkspaceStorage struct{}

// Resolve decides the RWX storage class for EKS clusters. It first looks for
// a StorageClass backed by the EFS CSI driver (efs.csi.aws.com); if none is
// present it falls back to the bundled local-path provisioner.
func (eksWorkspaceStorage) Resolve(ctx context.Context, p cloud.WorkspaceParams) (cloud.Decision, error) {
	return cloud.ResolveRWXOrBundled(ctx, p.Clients.Typed, []string{"efs.csi.aws.com"})
}
