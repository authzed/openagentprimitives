package gke

import (
	"context"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

func init() {
	cloud.Register(Strategy{}, "gke")
}

// Strategy is the GKE cloud.Strategy implementation. It captures all
// GKE-specific install behavior — gateway class, DNS/LB CIDRs, Artifact
// Registry derivation, and workspace storage resolution — so cmd/oap can
// dispatch through the cloud.Strategy interface with no GKE branches.
type Strategy struct{}

// Key returns the registry key for this strategy.
func (Strategy) Key() string { return "gke" }

// DisplayName returns the human-readable label used in install messages.
func (Strategy) DisplayName() string { return "GKE" }

// IsManaged reports that GKE is a big-three managed cloud, gating the
// non-local external-access guard.
func (Strategy) IsManaged() bool { return true }

// ProviderIDPrefix is the gce:// prefix used on GKE node spec.providerIDs,
// used by cloud.Detect to identify GKE clusters.
func (Strategy) ProviderIDPrefix() string { return "gce://" }

// DNSEgressCIDRs returns the link-local range required for GKE's
// NodeLocal DNSCache. GKE (including Autopilot) resolves DNS via the
// NodeLocal cache on 169.254.x.x rather than the kube-dns ClusterIP —
// a default-deny egress policy silently drops those lookups. Scoped to
// port 53 the broad link-local range is safe: the metadata server
// (169.254.169.254) does not serve DNS.
func (Strategy) DNSEgressCIDRs() []string { return []string{"169.254.0.0/16"} }

// GatewayBackendIngressCIDRs returns Google's well-known ranges for the
// GKE global external L7 load balancer and its health-check probes.
// A namespace-wide default-deny ingress policy MUST allow these or the
// LB marks every backend unhealthy and serves 503.
func (Strategy) GatewayBackendIngressCIDRs() []string {
	return []string{"35.191.0.0/16", "130.211.0.0/22"}
}

// GatewayAddressWait returns a generous address-wait budget for GKE's managed
// global external L7 Gateway. The first provision of a brand-new Gateway
// routinely takes ~10m end to end: the controller reserves the LB address
// quickly, but the forwarding rules + target HTTPS proxy (and thus the
// Programmed condition + status.addresses oap polls for) only land several
// minutes later. An 8m budget surfaced a false "stall" while the LB was still
// coming up, so the deadline is 15m with a 10m expected-ready soft-warn.
func (Strategy) GatewayAddressWait() (deadline, eta time.Duration) {
	return 15 * time.Minute, 10 * time.Minute
}

// RegistryFromProviderID derives the default Artifact Registry host for
// the cluster from a node's spec.providerID. It maps
// gce://PROJECT/ZONE/INSTANCE to REGION-docker.pkg.dev/PROJECT/ap (the
// Artifact Registry repository in the node's region). Returns "" for
// non-GKE or malformed IDs.
func (Strategy) RegistryFromProviderID(providerID string) string {
	project, zone, ok := parseGCEProviderID(providerID)
	if !ok {
		return ""
	}
	// region = zone minus its trailing "-<letter>" (us-central1-a → us-central1)
	region := zone
	if i := strings.LastIndex(zone, "-"); i > 0 {
		region = zone[:i]
	}
	if region == "" {
		return ""
	}
	return region + artifactRegistrySuffix + "/" + project + "/ap"
}

// ProjectFromProviderID derives the GCP project from a node's
// spec.providerID (gce://PROJECT/ZONE/INSTANCE → PROJECT). Used by the
// Google-managed TLS strategy to scope gcloud certificate-manager calls.
// Returns "" for non-GKE or malformed IDs.
func (Strategy) ProjectFromProviderID(providerID string) string {
	project, _, ok := parseGCEProviderID(providerID)
	if !ok {
		return ""
	}
	return project
}

// TLS returns the GoogleManagedTLS strategy which provisions GKE
// Certificate Manager DNS authorizations and managed certificates,
// bypassing cert-manager / ACME (which fights GKE's backend health check
// on the managed L7 Gateway).
func (Strategy) TLS() cloud.TLSStrategy { return GoogleManagedTLS{} }

// WorkspaceStorage returns the GKE workspace storage resolver. It
// defaults to the bundled node-local class on Standard and degrades to
// isolated on Autopilot (which rejects hostPath); a durable Filestore class
// is opt-in via --workspace-storage-class, never auto-preferred.
func (Strategy) WorkspaceStorage() cloud.WorkspaceStorage { return workspaceStorage{} }

// StatefulStorage returns the GKE RWO stateful-storage resolver. It steers the
// bundled Postgres/Neo4j PVCs onto Hyperdisk when the cluster default provisions
// Persistent Disk but the node pool is a Hyperdisk-only machine family.
func (Strategy) StatefulStorage() cloud.StatefulStorage { return gkeStatefulStorage{} }

// ArtifactStorage returns the GKE artifact-storage resolver: consented GCS
// bucket auto-provisioning (workload identity + labeled adopt-or-create +
// direct-principal IAM). See artifactstorage.go.
func (Strategy) ArtifactStorage() cloud.ArtifactStorage { return gkeArtifactStorage{} }

// InstallProfile returns the managed production profile: the durable-cluster
// defaults, plus RequiresExternalHostname — a managed cloud with no external
// hostname is reachable only through `kubectl port-forward`.
func (Strategy) InstallProfile() cloud.InstallProfile { return cloud.ManagedProfile }

// Validate refuses a cluster whose nodes report a different managed cloud.
func (s Strategy) Validate(ctx context.Context, p cloud.ValidateParams) error {
	return cloud.ValidateManagedPrefix(ctx, s, p)
}

// workspaceStorage is the GKE cloud.WorkspaceStorage implementation.
type workspaceStorage struct{}

// Resolve decides the workspace storage class for GKE clusters. The DEFAULT is
// the bundled node-local local-path class on GKE Standard — the same shape
// every other cloud uses for its bundled fallback.
//
// This is a deliberate revert away from auto-preferring a durable Filestore RWX
// class. Filestore is an NFS mount, and the agent workspace is git-heavy (many
// small files), where NFS runs ~10x slower than node-local disk; that latency
// hurts every session, while the node-local class's failure mode (volumes
// stranding on node churn, and node ephemeral-storage filling up) is bounded by
// the operator's workspacevolume janitor and the agentsession disk-pressure
// reclaim. Filestore therefore stays AVAILABLE but opt-in: an operator names it
// explicitly with --workspace-storage-class, which cmd/oap handles on the
// ExplicitClass path before Resolve is ever consulted. Resolve never inspects
// or auto-selects a Filestore class, so it never emits a CostWarning.
//
// On Autopilot the bundled hostPath provisioner is denied by the Warden
// webhook, and with Filestore no longer auto-selected there is no free default:
// Resolve degrades to isolated /work and tells the operator to name a Filestore
// class explicitly (or deploy on Standard).
func (workspaceStorage) Resolve(ctx context.Context, p cloud.WorkspaceParams) (cloud.Decision, error) {
	autopilot, err := cloud.IsGKEAutopilot(ctx, p.Clients.Typed)
	if err != nil {
		return cloud.Decision{}, err
	}
	if autopilot {
		return cloud.Decision{
			Degraded: true,
			Message: "GKE Autopilot: the bundled hostPath workspace provisioner is denied here (Warden), and Filestore is " +
				"no longer auto-selected. For durable shared workspaces pass --workspace-storage-class=<a Filestore RWX class> " +
				"(enable the Filestore CSI driver first), or deploy on a GKE Standard cluster. Running with isolated /work.",
		}, nil
	}

	// GKE Standard: the bundled node-local local-path provisioner is the
	// default. NeedsBundled applies the provisioner manifests, then the caller
	// probes before trusting the class. Its volumes are node-local and strand
	// when their node is removed; the operator's workspacevolume janitor
	// reclaims the Released remnants and the agentsession reclaim sweep bounds
	// how long terminal-session volumes occupy a node's disk.
	return cloud.Decision{
		ClassName:    cloud.BundledWorkspaceStorageClass,
		NeedsBundled: true,
		Verify:       cloud.WorkspaceProbeBeforeUse,
	}, nil
}

// artifactRegistrySuffix is the host suffix of every GKE Artifact Registry
// endpoint (e.g. us-east1-docker.pkg.dev). Single home for this literal;
// also used by IsGKEArtifactRegistry and parseARRegistry in registry.go.
const artifactRegistrySuffix = "-docker.pkg.dev"

// filestoreProvisioner is the GKE Filestore CSI driver's provisioner name. The
// managed classes GKE creates when the addon is enabled (standard-rwx,
// enterprise-multishare-rwx, …) all report it.
const filestoreProvisioner = "filestore.csi.storage.gke.io"

// parseGCEProviderID splits a gce://PROJECT/ZONE/INSTANCE provider ID into
// its project and zone. Returns ok=false for non-gce prefixes, malformed
// IDs, or an empty project, zone, or instance. Shared by RegistryFromProviderID
// and ProjectFromProviderID.
func parseGCEProviderID(providerID string) (project, zone string, ok bool) {
	project, zone, _, ok = parseGCEProviderIDParts(providerID)
	return project, zone, ok
}

// parseGCEProviderIDParts splits gce://PROJECT/ZONE/INSTANCE into all three
// fields. ok=false for a non-gce prefix, fewer than three slash-separated
// segments, or an empty project, zone, or instance. Extra trailing segments
// are tolerated (matching the original parseGCEProviderID behaviour).
func parseGCEProviderIDParts(providerID string) (project, zone, instance string, ok bool) {
	rest, found := strings.CutPrefix(providerID, "gce://")
	if !found {
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}
