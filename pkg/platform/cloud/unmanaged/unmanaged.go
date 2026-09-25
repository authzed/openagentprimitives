// Package unmanaged implements the cloud.Strategy fallback for clusters that
// are not identified as a big-three managed cloud (e.g. kind, bare-metal,
// on-prem, or any cluster whose nodes carry no recognized providerID). It
// registers itself under cloud.KeyDefault so cloud.Default and cloud.Detect
// fall back to it when no other strategy matches.
//
// Its cloud behavior (TLS, workspace/stateful storage, Gateway) is inherited
// verbatim from pkg/platform/cloud/local, but it carries the production
// InstallProfile: an unmanaged cluster is a real cluster whose data must be
// durable, unlike the opt-in `local` kind.
package unmanaged

import (
	"context"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud/certmanager"
	"github.com/authzed/openagentprimitives/pkg/platform/schedfit"
)

func init() {
	cloud.Register(Strategy{}, cloud.KeyDefault)
}

// Strategy is the unmanaged cloud.Strategy implementation.
type Strategy struct{}

// Key returns the default kind's key.
func (Strategy) Key() string { return cloud.KeyDefault }

// DisplayName returns a human-readable label for messages.
func (Strategy) DisplayName() string {
	return "unmanaged cluster (on-prem, bare-metal, or undetected)"
}

// IsManaged reports false — this strategy covers non-managed clusters.
func (Strategy) IsManaged() bool { return false }

// ProviderIDPrefix returns "" because unmanaged clusters have no fixed
// providerID prefix; cloud.Detect falls back to this strategy when no
// registered prefix matches.
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

// RegistryFromProviderID returns "" because unmanaged clusters have no
// cloud-native registry derivable from a providerID.
func (Strategy) RegistryFromProviderID(_ string) string { return "" }

// ProjectFromProviderID returns "" because unmanaged clusters have no GCP
// project concept.
func (Strategy) ProjectFromProviderID(_ string) string { return "" }

// ResolveClusterIdentity returns the zero identity: an unmanaged cluster
// (kind, bare-metal, on-prem) has no cloud that knows its name and no console
// to link to.
func (Strategy) ResolveClusterIdentity(_ context.Context, _ cloud.ClusterIdentityParams) cloud.ClusterIdentity {
	return cloud.ClusterIdentity{}
}

// TLS returns the cert-manager + Let's Encrypt strategy, the default for
// any cluster accessible over the public internet with HTTP-01.
func (Strategy) TLS() cloud.TLSStrategy { return certmanager.Strategy{} }

// WorkspaceStorage returns the unmanaged workspace storage resolver. It has no
// preferred cloud-native RWX driver, so it always falls back to the bundled
// local-path provisioner.
func (Strategy) WorkspaceStorage() cloud.WorkspaceStorage { return unmanagedWorkspaceStorage{} }

// StatefulStorage trusts the cluster default RWO StorageClass. (GKE swaps this
// to a real resolver in stateful.go.)
func (Strategy) StatefulStorage() cloud.StatefulStorage { return cloud.NoopStatefulStorage{} }

// ArtifactStorage has no automated backend on unmanaged clusters: there
// is no single cloud object-storage API to target, so installs must bring
// their own bucket/container via --artifact-store-url.
func (Strategy) ArtifactStorage() cloud.ArtifactStorage {
	return cloud.RequireExplicitArtifactStorage{
		CloudName: "unmanaged clusters",
		Example:   "s3://<bucket> | gs://<bucket> | azblob://<container>",
	}
}

// SchedulingCeiling reads the cluster's own nodes: an unmanaged cluster — kind,
// a fixed bare-metal pool — does not grow, so the largest node present is the
// real ceiling. See cloud.NodeSchedulingCeiling for why that is unsafe on an
// autoscaling cloud.
func (Strategy) SchedulingCeiling(ctx context.Context, cl cloud.Clients) (schedfit.Ceiling, schedfit.Headroom, error) {
	return cloud.NodeSchedulingCeiling(ctx, cl)
}

// UnwedgeTerminatingNamespace is a no-op on unmanaged clusters: there is
// no cloud controller leaving dangling finalizers.
func (Strategy) UnwedgeTerminatingNamespace(context.Context, cloud.Clients, cloud.Reporter, string, bool) (cloud.UnwedgeReport, error) {
	return cloud.UnwedgeReport{}, nil
}

// EnsureGatewayController installs the bundled Envoy Gateway controller and
// creates its GatewayClass. unmanaged clusters have no managed Gateway
// controller, so the generic shared helper does the work.
func (Strategy) EnsureGatewayController(ctx context.Context, p cloud.GatewayControllerParams) (cloud.GatewayControllerResult, error) {
	return cloud.EnsureEnvoyGatewayController(ctx, p)
}

// InstallProfile returns the production profile: an unmanaged cluster is a real
// cluster (on-prem, bare-metal) whose data must be durable. Only the opt-in
// `local` kind gets the lightweight profile — so `oap install` on a bare-metal
// production cluster, and `mage smoke`'s `oap init` on kind, both keep the full
// postgres bundle exactly as before this kind existed.
func (Strategy) InstallProfile() cloud.InstallProfile { return cloud.ProductionProfile }

// Validate accepts any cluster, but refuses one whose nodes identify a managed
// cloud: silently installing the generic profile onto GKE would skip Hyperdisk
// pinning, the GCS artifact bucket, and the managed Gateway.
//
// This refusal does NOT honor p.AllowOverride: that flag is
// --allow-non-local-cluster, local's kubeconfig-host-pattern escape hatch
// (a corporate dev cluster on a private VPN) — it has nothing to do with
// unmanaged's providerID check. The generic profile is simply not available
// on a cluster whose nodes identify a managed cloud — installing it there
// would silently skip that cloud's storage, artifact, and Gateway handling —
// so there is no override, flag, or explicit --cluster-kind that waives this
// refusal; the only way onto a managed cloud is its own kind.
func (Strategy) Validate(ctx context.Context, p cloud.ValidateParams) error {
	got, err := cloud.DetectedManagedKey(ctx, p)
	if err != nil {
		return err
	}
	if got == "" {
		return nil
	}
	return fmt.Errorf("this cluster's nodes report %s, but the %s cluster kind was selected: pass --cluster-kind=%s to get its storage, artifact, and Gateway handling", got, cloud.KeyDefault, got)
}

// unmanagedWorkspaceStorage is the unmanaged cloud.WorkspaceStorage implementation.
type unmanagedWorkspaceStorage struct{}

// Resolve decides the RWX storage class for unmanaged clusters. There is no
// preferred cloud-native driver; it looks for any known RWX class and falls
// back to the bundled local-path provisioner if none is found.
func (unmanagedWorkspaceStorage) Resolve(ctx context.Context, p cloud.WorkspaceParams) (cloud.Decision, error) {
	return cloud.ResolveRWXOrBundled(ctx, p.Clients.Typed, nil)
}
