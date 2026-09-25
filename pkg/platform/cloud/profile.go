package cloud

import "fmt"

// SpiceDBDatastore selects the datastore engine backing the operator-managed
// SpiceDBCluster.
type SpiceDBDatastore int

const (
	// DatastoreMemory is ephemeral and process-local. It is the ONLY engine on
	// which SpiceDB may seed its own schema via --datastore-bootstrap-files:
	// that feature applies solely to an empty datastore and is fatal against a
	// non-empty one. See installcmd.SpiceDBClusterMode.bootstrapsSchemaAtStartup.
	DatastoreMemory SpiceDBDatastore = iota
	// DatastorePostgres is shared and persistent — durable, and safe at N>1
	// replicas.
	DatastorePostgres
)

func (d SpiceDBDatastore) String() string {
	switch d {
	case DatastoreMemory:
		return "memory"
	case DatastorePostgres:
		return "postgres"
	default:
		return fmt.Sprintf("SpiceDBDatastore(%d)", int(d))
	}
}

// PublicEndpointPolicy says how a cluster kind reaches the public Internet:
// through a tunnel this project manages — created up front or only when
// something needs one — or not at all.
//
// The zero value is PublicEndpointNever, deliberately: a profile that forgot to
// answer, or a consumer holding an unset value, refuses to open a tunnel rather
// than opening one on infrastructure that has real ingress.
type PublicEndpointPolicy int

const (
	// PublicEndpointNever — this kind has real ingress and must never carry a
	// project-managed tunnel. A PublicEndpoint here is refused, not quietly
	// ignored: on such a cluster webd's external URL is a real https:// host
	// that `oap install` set up, and a tunnel would overwrite it.
	PublicEndpointNever PublicEndpointPolicy = iota
	// PublicEndpointAlways — a tunnel is part of bring-up; `oap install`
	// creates the PublicEndpoint. The single-tunnel dev flow AllowsSharedOrigin
	// already describes.
	PublicEndpointAlways
	// PublicEndpointOnDemand — a tunnel is created only when something needs
	// one (a channel declaring inbound webhooks). A cluster with no such need
	// never opens one.
	PublicEndpointOnDemand
)

func (p PublicEndpointPolicy) String() string {
	switch p {
	case PublicEndpointNever:
		return "Never"
	case PublicEndpointAlways:
		return "Always"
	case PublicEndpointOnDemand:
		return "OnDemand"
	default:
		return fmt.Sprintf("PublicEndpointPolicy(%d)", int(p))
	}
}

// AllowsTunnel reports whether a PublicEndpoint may open a tunnel under this
// policy. Both Always and OnDemand permit one; they differ only in WHO creates
// the CR, which is not a question the tunnel's reconciler asks.
//
// Derived rather than stored so that a fourth policy cannot be added without
// deciding here what it means, and so no consumer writes `!= Never`.
func (p PublicEndpointPolicy) AllowsTunnel() bool {
	return p == PublicEndpointAlways || p == PublicEndpointOnDemand
}

// CreatedAtInstall reports whether `oap install` itself creates the
// PublicEndpoint. Only Always does: OnDemand permits a tunnel but leaves the
// creation to whatever needs one (a declared channel with inbound webhooks),
// and Never permits none at all.
//
// Derived here for the same reason AllowsTunnel is: the two questions have
// different answers for exactly one policy value, so a call site spelling
// either as `== Always` would silently pick a side for any policy added later.
func (p PublicEndpointPolicy) CreatedAtInstall() bool {
	return p == PublicEndpointAlways
}

// InstallProfile is the set of install-shape decisions that differ between the
// lightweight developer kinds (local, desktop) and every durable-cluster kind
// (default, gke, eks, aks). It exists so that no consumer branches on
// local-vs-remote.
//
// It is a sub-interface of Strategy, alongside TLS/WorkspaceStorage/
// StatefulStorage/ArtifactStorage, rather than a value struct, so a single
// cloud can override one decision without restating the others — as gke does
// for StatefulStorage and ArtifactStorage alone.
type InstallProfile interface {
	// MemoryBackend is the operator's MEMORY_BACKEND value: "sqlite" or
	// "postgres".
	MemoryBackend() string

	// SpiceDBDatastore is the engine for the operator-managed SpiceDBCluster.
	SpiceDBDatastore() SpiceDBDatastore

	// UsesLocalDevImages reports whether first-party images are consumed as
	// local :dev tags rather than pushed to and pulled from a registry.
	UsesLocalDevImages() bool

	// AllowsSharedOrigin reports whether webd may serve its trusted-auth and
	// untrusted-artifact origins from one host. Only the single-tunnel dev flow
	// may: sharing them collapses the cross-origin isolation the artifact
	// viewer depends on.
	AllowsSharedOrigin() bool

	// RequiresExternalHostname reports whether an install without an external
	// hostname should be refused. True where the alternative is a cluster
	// reachable only through `kubectl port-forward`.
	RequiresExternalHostname() bool

	// PromptsForIdentityProvider reports whether `oap init` offers the
	// interactive IdP setup step.
	PromptsForIdentityProvider() bool

	// PromptsForMonitoring reports whether `oap init` offers the interactive
	// monitoring setup step.
	PromptsForMonitoring() bool

	// PublicEndpointPolicy reports whether a project-managed tunnel may reach
	// this cluster kind at all, and if so who creates it.
	//
	// This is a property of the CLUSTER, not of a command that happened to run
	// — which is why it is a policy and not the bool it replaced: the
	// PublicEndpoint reconciler runs inside the operator long after any install
	// command has exited, and it must be able to refuse a CR that predates the
	// answer.
	PublicEndpointPolicy() PublicEndpointPolicy

	// AllowsLocalOnlyIdentityProviders gates ClusterIdentityProvider kinds
	// whose idp.Kind.AllowedNonLocal() is false — e.g. `password`, a
	// single-user admin gate with no anti-brute-force posture of its own.
	AllowsLocalOnlyIdentityProviders() bool

	// ServesLocalWebChat gates NOTHING today: no binary reads it. The transcript
	// data plane it names (pkg/web/webui/chat) enumerates no agents, creates no
	// sessions, and authorizes every route on agentsession#interact per request,
	// so where it may be served is not a deployment-shape question — it mounts
	// wherever its NATS, SpiceDB and operator-URL prerequisites hold.
	//
	// It is retained only because it is `desktop`'s ONE distinguishing answer
	// from `local` (see pkg/platform/cloud/desktop): removing it collapses two
	// registered kinds into one, and what should distinguish them instead is a
	// cluster-kind question, not a WebUI one.
	//
	// Do NOT reach for it as a general "is this deployment private" flag — it
	// gates no channel kind and no WebUI plugin. Shared-origin decisions come
	// from AllowsSharedOrigin.
	ServesLocalWebChat() bool
}

// DevProfile is the lightweight developer profile, used by the `local` kind.
// Everything it selects trades durability and isolation for speed of bring-up
// on a single-node cluster.
var DevProfile InstallProfile = devProfile{}

// ProductionProfile is the durable-cluster profile, shared by `default`, `gke`,
// `eks`, and `aks`.
var ProductionProfile InstallProfile = productionProfile{}

type devProfile struct{}

func (devProfile) MemoryBackend() string                  { return "sqlite" }
func (devProfile) SpiceDBDatastore() SpiceDBDatastore     { return DatastoreMemory }
func (devProfile) UsesLocalDevImages() bool               { return true }
func (devProfile) AllowsSharedOrigin() bool               { return true }
func (devProfile) RequiresExternalHostname() bool         { return false }
func (devProfile) PromptsForIdentityProvider() bool       { return false }
func (devProfile) PromptsForMonitoring() bool             { return false }
func (devProfile) AllowsLocalOnlyIdentityProviders() bool { return true }

// PublicEndpointPolicy is Always for `local`: `oap install` creates the
// PublicEndpoint as part of bring-up, because a local cluster has no other way
// to be reached from the public Internet. desktopDevProfile overrides it.
func (devProfile) PublicEndpointPolicy() PublicEndpointPolicy { return PublicEndpointAlways }

// ServesLocalWebChat is false for the shared dev profile; only desktopDevProfile
// overrides it. It gates nothing — see InstallProfile.ServesLocalWebChat.
func (devProfile) ServesLocalWebChat() bool { return false }

type productionProfile struct{}

func (productionProfile) MemoryBackend() string              { return "postgres" }
func (productionProfile) SpiceDBDatastore() SpiceDBDatastore { return DatastorePostgres }
func (productionProfile) UsesLocalDevImages() bool           { return false }
func (productionProfile) AllowsSharedOrigin() bool           { return false }

// RequiresExternalHostname defaults to false for the shared production profile:
// an on-prem or bare-metal cluster may legitimately be service-only. The
// managed clouds override this to true — there, no hostname means the install
// is reachable only via `kubectl port-forward`.
func (productionProfile) RequiresExternalHostname() bool { return false }

func (productionProfile) PromptsForIdentityProvider() bool       { return true }
func (productionProfile) PromptsForMonitoring() bool             { return true }
func (productionProfile) AllowsLocalOnlyIdentityProviders() bool { return false }
func (productionProfile) ServesLocalWebChat() bool               { return false }

// PublicEndpointPolicy is Never for every durable-cluster kind. These clusters
// have real ingress, and `oap install` has already pointed webd's external URL
// at a genuine https:// host (or seeded it EMPTY so credential links fail
// CLOSED until it does — see installcmd.ensureWebdExternalURLConfigMap). A
// tunnel here would overwrite exactly the value that install went out of its
// way to get right.
func (productionProfile) PublicEndpointPolicy() PublicEndpointPolicy { return PublicEndpointNever }

// managedProfile is ProductionProfile with RequiresExternalHostname flipped on.
// gke/eks/aks return it; default returns ProductionProfile.
type managedProfile struct{ productionProfile }

func (managedProfile) RequiresExternalHostname() bool { return true }

// ManagedProfile is the production profile for the big-three managed clouds.
var ManagedProfile InstallProfile = managedProfile{}

// desktopDevProfile is DevProfile with two answers flipped: ServesLocalWebChat,
// which drives no behavior (see InstallProfile.ServesLocalWebChat), and
// PublicEndpointPolicy, which does.
type desktopDevProfile struct{ devProfile }

func (desktopDevProfile) ServesLocalWebChat() bool { return true }

// PublicEndpointPolicy is OnDemand for `desktop`, where `local` answers Always.
// This is the kind's first override that actually gates something: a desktop VM
// is useful with no public tunnel at all, so one is opened only when something
// needs inbound webhooks, whereas a `local` cluster's whole dev flow is the
// tunnel `oap install` opens for it.
func (desktopDevProfile) PublicEndpointPolicy() PublicEndpointPolicy { return PublicEndpointOnDemand }

// DesktopDevProfile is the dev profile for the `desktop` kind: identical to
// DevProfile except for the two answers above.
var DesktopDevProfile InstallProfile = desktopDevProfile{}

// NeedsBundledPostgres reports whether this install must stand up the bundled
// Postgres. It is DERIVED from the two facts that determine it rather than
// being a profile method of its own, so the three cannot drift out of sync.
func NeedsBundledPostgres(s Strategy) bool {
	p := s.InstallProfile()
	return p.SpiceDBDatastore() == DatastorePostgres || p.MemoryBackend() == "postgres"
}
