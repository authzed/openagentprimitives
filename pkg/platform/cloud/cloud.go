package cloud

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/authzed/openagentprimitives/pkg/platform/schedfit"
)

// UnwedgeReport describes what UnwedgeTerminatingNamespace found and (when
// remediate=true) did. The consumer (oap clean / oap install) owns all
// user-facing printing of this report; cloud backends never write to stdout.
type UnwedgeReport struct {
	StuckFinalizers       []string // finalizer names found blocking termination
	CloudResourcesDeleted []string // cloud resources deleted (e.g. GCP NEG self-links)
	FinalizersCleared     []string // namespaced objects whose finalizer was cleared
	Blocked               []string // objects we refused to force, with the reason
	ManualCommands        []string // exact commands for the surface-only / degraded path
}

// Strategy is the per-cloud install behavior contract. One Strategy is
// registered per cloud provider (gke/eks/aks) plus one default for
// unmanaged/undetectable clusters. cmd/oap dispatches through this interface
// so it contains zero "if cloud == gke" branches.
type Strategy interface {
	Key() string              // one of cloud.Key* — "gke"/"eks"/"aks"/"default" (local/unmanaged)
	DisplayName() string      // human label for messages
	IsManaged() bool          // big-three managed cloud?
	ProviderIDPrefix() string // node spec.providerID prefix this cloud owns; "" for default

	DNSEgressCIDRs() []string
	GatewayBackendIngressCIDRs() []string
	// GatewayAddressWait returns the webd Gateway address wait budget: deadline
	// is the poll window before oap surfaces a stall and the keep-waiting prompt,
	// eta drives the one-time "taking longer than expected" soft-warn. GKE's
	// first global L7 provision routinely runs ~10m (the frontend lags the
	// reserved address), so it returns a larger budget than the bundled-Envoy
	// clouds — whose faster LBs keep a genuine stall promptly detectable rather
	// than masked behind a GKE-sized window.
	GatewayAddressWait() (deadline, eta time.Duration)
	RegistryFromProviderID(providerID string) string
	ProjectFromProviderID(providerID string) string

	TLS() TLSStrategy
	WorkspaceStorage() WorkspaceStorage
	// StatefulStorage decides the RWO block-storage class for the bundled
	// stateful PVCs (Postgres + Neo4j). Default no-op trusts the cluster
	// default; only GKE overrides (Hyperdisk-only node pools).
	StatefulStorage() StatefulStorage
	// ArtifactStorage ensures the operator's durable artifact store (and can
	// tear it down at clean time). GKE auto-provisions a labeled GCS bucket
	// via consented gcloud; other clouds require --artifact-store-url.
	ArtifactStorage() ArtifactStorage

	// SchedulingCeiling reports the largest single pod this cluster can ever
	// schedule, plus the free headroom on the node that sets that ceiling.
	//
	// Known=false means "not provable on this cloud" — an autoscaler can add a
	// node bigger than any present today, so the caller must SKIP its check
	// rather than guess. Fail-safe: an API error yields Known=false with the
	// reason in Ceiling.Source and a nil error, never a failed install. Headroom
	// is advisory only (it moves as pods come and go) — suggest a value to a
	// human with it, never decide something is impossible.
	SchedulingCeiling(ctx context.Context, cl Clients) (schedfit.Ceiling, schedfit.Headroom, error)

	// UnwedgeTerminatingNamespace clears cloud-specific dangling finalizers (and
	// the orphaned cloud resources they guard) that the cloud's own controllers
	// failed to reconcile, leaving namespace stuck Terminating. remediate=false
	// only inspects and reports, mutating nothing; remediate=true deletes the
	// orphaned resources and clears the finalizers — but never force-clears one
	// whose backing resource is still referenced by a live load balancer. A
	// no-op for clouds with no such failure mode, or when the unwedge tooling
	// (e.g. gcloud) is unavailable — then the report carries manual-fix commands
	// for the caller to surface.
	UnwedgeTerminatingNamespace(ctx context.Context, cl Clients, rep Reporter, namespace string, remediate bool) (UnwedgeReport, error)

	// EnsureGatewayController makes a working Gateway controller + the Gateway
	// API available on the current cluster before oap reads or applies webd's
	// Gateway/HTTPRoutes, offering to install/enable whatever this cloud needs.
	// Managed-Gateway clouds (GKE) offer to enable the control-plane Gateway API
	// on the connected cluster and wait until it is served; clouds that ship
	// their own controller (EKS/AKS/local) install Envoy Gateway and create its
	// GatewayClass. Result.Proceed=false means external access is skipped (the
	// reason is already surfaced).
	EnsureGatewayController(ctx context.Context, p GatewayControllerParams) (GatewayControllerResult, error)

	// InstallProfile returns the install-shape decisions that differ between
	// the lightweight developer kind and the durable-cluster kinds. See
	// profile.go for why this is an interface rather than a bool.
	InstallProfile() InstallProfile

	// ResolveClusterIdentity best-effort-derives the cluster's display name and
	// cloud-console deep link from one of its nodes, for an admin surface that
	// wants to name the cluster it is looking at.
	//
	// It NEVER errors and never blocks indefinitely: a probe that does not
	// answer (an unreachable metadata endpoint, a node name that does not fit
	// the cloud's pattern) leaves that field empty and logs, so the caller
	// renders whatever was derivable. Clouds that can derive nothing return the
	// zero value.
	ResolveClusterIdentity(ctx context.Context, p ClusterIdentityParams) ClusterIdentity

	// Validate reports whether the connected cluster is consistent with this
	// kind. It runs on EVERY install — for explicitly-chosen and detected kinds
	// alike — before the first mutation, so a wrong --cluster-kind fails fast
	// rather than halfway through a bring-up that leaves a crash-looping
	// cluster behind.
	Validate(ctx context.Context, p ValidateParams) error
}

var registry = struct {
	byKey map[string]Strategy
}{byKey: map[string]Strategy{}}

// Register adds s under the given keys. At least one key is REQUIRED: a kind
// that does not name itself cannot be stamped into AP_CLUSTER_KIND or named in
// an error message. A duplicate key or a keyless call panics — a programming
// error caught at init().
func Register(s Strategy, keys ...string) {
	if s == nil {
		panic("cloud: Register(nil)")
	}
	if len(keys) == 0 {
		panic(fmt.Sprintf("cloud: Register(%T) with no keys; every kind must name its key (see cloud.Key*)", s))
	}
	for _, k := range keys {
		if existing, ok := registry.byKey[k]; ok {
			panic(fmt.Sprintf("cloud: key %q already registered to %s; cannot also register %s", k, existing.Key(), s.Key()))
		}
		registry.byKey[k] = s
	}
}

// For returns the Strategy registered under key.
//
// An unregistered key — or an empty one — is an ERROR. There is no silent
// downgrade to the default kind: a typo'd --cluster-kind or a stale
// AP_CLUSTER_KIND on a live Deployment would otherwise install (or serve) the
// wrong profile without a word. On GKE that means no Hyperdisk pinning, no GCS
// artifact bucket, and no managed Gateway, all silently.
//
// Default() is the only route to the fallback, so every fallback is written out
// at the site where it happens.
func For(key string) (Strategy, error) {
	if key == "" {
		return nil, fmt.Errorf("empty cluster kind: pass one of %s, or call cloud.Default() to select the fallback explicitly", strings.Join(RegisteredKeys(), ", "))
	}
	s, ok := registry.byKey[key]
	if !ok {
		return nil, fmt.Errorf("unknown cluster kind %q (registered: %s)", key, strings.Join(RegisteredKeys(), ", "))
	}
	return s, nil
}

// Default returns the KeyDefault kind — the fallback for a cluster with no
// recognized providerID.
//
// It errors rather than returning a nil Strategy when nothing is registered:
// that state means a binary forgot its blank import (see cmd/oap/cloudimports.go,
// internal/cmd/operator/cloudimports.go), and a nil interface pushes a nil check onto
// every consumer.
func Default() (Strategy, error) {
	s, ok := registry.byKey[KeyDefault]
	if !ok {
		return nil, fmt.Errorf("no %q cluster kind registered: the binary is missing its blank import of pkg/platform/cloud/unmanaged (see cmd/oap/cloudimports.go)", KeyDefault)
	}
	return s, nil
}

// MustFor is For, panicking on error. It is for the two contexts that cannot
// handle one — init() registration and table-driven test literals — where a bad
// key is a programming error. Never use it on a request path.
func MustFor(key string) Strategy {
	s, err := For(key)
	if err != nil {
		panic("cloud: " + err.Error())
	}
	return s
}

// RegisteredKeys returns every registered key, sorted — for error messages and
// for tests that must cover all kinds without hardcoding the list.
func RegisteredKeys() []string {
	keys := make([]string, 0, len(registry.byKey))
	for k := range registry.byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Detect inspects node spec.providerID prefixes and returns the matching
// Strategy, falling back to Default() when nothing matches.
//
// On an API failure it returns (nil, err) — never a usable Strategy alongside
// an error, which invites callers to ignore err and proceed with a strategy
// that was never chosen.
//
// It can never return the KeyLocal/KeyDesktop kinds: they report no providerID
// prefix, so the scan skips them and the fallback is Default(). Both are opt-in
// only.
func Detect(ctx context.Context, kc kubernetes.Interface) (Strategy, error) {
	nodes, err := kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 5})
	if err != nil {
		return nil, fmt.Errorf("list nodes to detect cloud provider: %w", err)
	}
	for i := range nodes.Items {
		if s, ok := ForProviderID(nodes.Items[i].Spec.ProviderID); ok {
			return s, nil
		}
	}
	return Default()
}

// ForProviderID returns the registered Strategy that owns a node's
// spec.providerID prefix, and whether one matched. It is the single place a
// providerID becomes a cloud kind, so a caller holding a node (rather than a
// kubernetes.Interface to list with) does not re-implement the prefix match.
//
// A kind reporting no prefix is skipped, so this can never return `local` or
// `desktop`: they are opt-in only, and auto-selecting one on a real cluster
// would install an ephemeral datastore onto durable infrastructure. ok=false
// also covers an empty registry (a binary missing its blank imports) — a caller
// needing to tell those apart checks RegisteredKeys.
func ForProviderID(providerID string) (Strategy, bool) {
	if providerID == "" {
		return nil, false
	}
	for _, s := range registry.byKey {
		if p := s.ProviderIDPrefix(); p != "" && strings.HasPrefix(providerID, p) {
			return s, true
		}
	}
	return nil, false
}
