package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories={authzed,spicebox},shortName=pubep
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +genclient
// +genclient:nonNamespaced
//
// PublicEndpoint is how this cluster is reached from the public Internet.
//
// Cluster-scoped because it describes the cluster's own front door, not any
// one namespace's. status.url is the single source of truth for where this
// cluster is reachable; everything that needs a public address reads it,
// directly or through the ConfigMap the controller derives from it.
type PublicEndpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PublicEndpointSpec   `json:"spec,omitempty"`
	Status            PublicEndpointStatus `json:"status,omitempty"`
}

type PublicEndpointSpec struct {
	// Target is the in-cluster Service the tunnel forwards to.
	Target PublicEndpointTarget `json:"target"`
	// Provider names a registered tunnel provider: "ngrok" today. Dispatched
	// through pkg/web/localtunnel/registry; a new provider is a registration,
	// never a branch in a consumer.
	// +kubebuilder:validation:MinLength=1
	Provider string `json:"provider"`
	// AuthTokenRef names the Secret key holding the provider's auth token.
	AuthTokenRef ClusterSecretKeyRef `json:"authTokenRef"`
	// LocalURL is where the target is reachable FROM THE HOST while no tunnel
	// is up — the address a port-forward binds, scheme and port included:
	// "http://localhost:8080" for a `kubectl port-forward`-shaped install,
	// "http://127.0.0.1:17080" for the desktop app's own forward.
	//
	// It is set by whoever creates this CR because the cluster cannot know it.
	// The host half and the port half BOTH vary, and they vary independently:
	// `oap install` seeds webd's external URL as "http://localhost:8080", while
	// `oap desktop` binds 127.0.0.1 on a port it picks at runtime by scanning
	// upward from a base. webd dispatches on an EXACT bare-host match, so
	// "localhost" and "127.0.0.1" are different origins to it and guessing
	// either one 404s the browser at the address the user was told to open.
	//
	// REQUIRED, and deliberately not defaulted: a default is what produced the
	// bug this field exists to close — a plausible-looking address written by
	// something that could not know the real one. The controller ALSO refuses
	// an empty value at runtime, for a CR created against an earlier version of
	// this CRD where the apiserver did not yet enforce it.
	// +kubebuilder:validation:MinLength=1
	LocalURL string `json:"localURL"`
	// ReservedDomain pins a stable hostname. Empty means the provider assigns
	// one per session, which changes on every restart — see the repointing
	// the channel controller does in response.
	ReservedDomain string `json:"reservedDomain,omitempty"`
}

type PublicEndpointTarget struct {
	// Namespace is the Service's namespace.
	Namespace string `json:"namespace"`
	// Service is the Service's name.
	Service string `json:"service"`
	// Port is the Service port to forward to.
	Port int32 `json:"port"`
}

type PublicEndpointStatus struct {
	// URL is the live public address, empty until the tunnel is Ready. It is
	// an OBSERVATION: only this controller writes it, and it is never mirrored
	// into an applied field.
	URL string `json:"url,omitempty"`
	// Phase is Pending, Ready or Failed.
	Phase string `json:"phase,omitempty"`
	// ObservedAt is when the controller last CHANGED url, phase or the Ready
	// condition. It is deliberately NOT a per-reconcile heartbeat: a
	// freshly-stamped timestamp on every reconcile would make every reconcile a
	// status write, churning the object and re-triggering every watcher for no
	// new information.
	ObservedAt *metav1.Time       `json:"observedAt,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const (
	PublicEndpointPhasePending = "Pending"
	PublicEndpointPhaseReady   = "Ready"
	PublicEndpointPhaseFailed  = "Failed"
)

// PublicEndpoint condition types.
const (
	// PublicEndpointConditionReady is True exactly while the operator holds an
	// open tunnel for this endpoint and status.url names it. False carries the
	// reason the tunnel is not up, in the reasons below.
	PublicEndpointConditionReady = "Ready"
)

// PublicEndpoint condition reasons.
const (
	// ReasonPublicEndpointTunnelOpen — the tunnel is open and status.url is live.
	ReasonPublicEndpointTunnelOpen = "TunnelOpen"
	// ReasonPublicEndpointTunnelOpening — a concurrent reconcile of this same
	// endpoint is inside the provider's Start. This one declined to open a
	// second provider session and will re-check shortly.
	ReasonPublicEndpointTunnelOpening = "TunnelOpening"
	// ReasonPublicEndpointAuthTokenMissing — spec.authTokenRef does not resolve
	// to a non-empty Secret key. A prerequisite that has not arrived yet rather
	// than a failure, so the phase is Pending and the controller re-checks.
	ReasonPublicEndpointAuthTokenMissing = "AuthTokenSecretMissing"
	// ReasonPublicEndpointProviderUnknown — spec.provider names no registered
	// tunnel provider. Permanent until the spec is edited, so the phase is
	// Failed and the controller does not requeue.
	ReasonPublicEndpointProviderUnknown = "UnknownProvider"
	// ReasonPublicEndpointTunnelFailed — the provider refused to open the
	// tunnel (bad token, provider outage, session limit reached).
	ReasonPublicEndpointTunnelFailed = "TunnelStartFailed"
	// ReasonPublicEndpointLocalURLMissing — spec.localURL is empty. The CRD
	// marks the field required, so this is reachable only for a CR created
	// against an earlier version of the schema; refused rather than defaulted,
	// because a guessed local address 404s webd for every route.
	ReasonPublicEndpointLocalURLMissing = "LocalURLMissing"
	// ReasonPublicEndpointClusterKindRefusesTunnels — this cluster kind's
	// PublicEndpointPolicy is Never. Durable clusters have real ingress and
	// webd's external URL already names a genuine host; opening a tunnel would
	// overwrite it. Refused rather than quietly ignored so the CR says why.
	ReasonPublicEndpointClusterKindRefusesTunnels = "ClusterKindRefusesTunnels"
	// ReasonPublicEndpointReservedDomainUnsupported — spec.reservedDomain is
	// set but no registered provider honors it yet. Refused fail-closed rather
	// than accepted-and-ignored: silently handing back a per-session URL that
	// changes on every restart would leave the operator believing a promise
	// the system is not keeping.
	ReasonPublicEndpointReservedDomainUnsupported = "ReservedDomainUnsupported"
)

// +kubebuilder:object:root=true

// PublicEndpointList contains a list of PublicEndpoint.
type PublicEndpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PublicEndpoint `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PublicEndpoint{}, &PublicEndpointList{})
}
