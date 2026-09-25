package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Pin mode values, ordered strongest-first for ceiling folding:
// block > approve > warn > off. An empty Mode on a PinningRule means approve.
const (
	PinModeBlock   = "block"
	PinModeApprove = "approve"
	PinModeWarn    = "warn"
	PinModeOff     = "off"
)

// PinRecord is the recorded identity of a pinned dependency — one shape for
// every kind, written by the resolving controller and consumed by settings
// enforcement, drift detection, audit logging, and `oap pin`. Definition CRs
// carry the baseline; AgentSession status carries per-session observations.
type PinRecord struct {
	// Kind is the pinning registry kind name (skill, image, mcp, cli, …).
	// Validated against the registry by controllers, not by a CRD enum, so
	// new kinds register without an API change.
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// Strength is the syntactic pin strength of the declared ref.
	// +kubebuilder:validation:Enum=frozen;named;unpinned
	Strength string `json:"strength"`

	// Digest is the immutable identity: git sha, sha256:… image digest,
	// canonical tool-manifest hash, or binary hash.
	// +optional
	Digest string `json:"digest,omitempty"`

	// Version is the human-readable identity: tag, serverInfo.version, or
	// version-probe output.
	// +optional
	Version string `json:"version,omitempty"`

	// ObservedAt is when the recording controller observed this identity.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`

	// Details carries kind-specific extras (toolCount, registry host, …).
	// +optional
	Details map[string]string `json:"details,omitempty"`
}

// PinningPolicy is the unified per-kind pinning ceiling carried in
// SettingsLimits. Rules narrow downward (strongest wins per kind across
// tiers); Bypass entries are tier-scoped escape hatches — a bypass only
// neutralizes rules set at its own tier or below, so a namespace bypass
// cannot pierce a cluster ceiling.
type PinningPolicy struct {
	// Rules is at most one requirement per dependency kind.
	// +optional
	// +listType=map
	// +listMapKey=kind
	Rules []PinningRule `json:"rules,omitempty"`

	// Bypass exempts named dependencies from this tier's rules and lower.
	// +optional
	// +listType=atomic
	Bypass []PinningBypass `json:"bypass,omitempty"`
}

// PinningRule is the requirement for one dependency kind.
type PinningRule struct {
	// Kind is the pinning registry kind name this rule governs.
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// MinStrength is the minimum pin strength a declared ref must have.
	// Empty = no floor (drift observation still applies).
	// +kubebuilder:validation:Enum=frozen;named
	// +optional
	MinStrength string `json:"minStrength,omitempty"`

	// Mode governs what a violation or drift does: block (fail closed),
	// approve (force per-call user approval), warn (surface warnings only),
	// off (observe only). Empty = approve.
	// +kubebuilder:validation:Enum=block;approve;warn;off
	// +optional
	Mode string `json:"mode,omitempty"`
}

// PinningBypass exempts one named dependency from this tier's (and lower
// tiers') pinning rules. Reason is required: the escape hatch must leave an
// audit trail.
type PinningBypass struct {
	// Kind is the pinning registry kind this exemption applies to.
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// Name identifies the exempted item: a canonical skill name, MCPServer
	// name, SidecarToolbox name, or SpiceboxToolkit name. Trailing-wildcard
	// patterns are allowed (same syntax as AllowedSkills).
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Reason is the required audit-trail justification for the exemption.
	// +kubebuilder:validation:MinLength=1
	Reason string `json:"reason"`
}

// ObservedPin is one per-session pin observation.
type ObservedPin struct {
	// Name identifies the dependency: MCPServer ref name, SidecarToolbox
	// name, Toolkit name, or canonical skill name.
	Name string `json:"name"`
	// Pin is the identity observed at session start; drift is this against the
	// definition CR's baseline.
	Pin PinRecord `json:"pin"`
}

// EffectivePinning preserves the per-tier pinning policies. Bypass entries are
// tier-scoped, so the policies cannot be pre-folded to one list: per-item
// evaluation happens via settings.PinRequirementFor.
type EffectivePinning struct {
	// Cluster is the cluster-tier policy, kept un-folded.
	// +optional
	Cluster *PinningPolicy `json:"cluster,omitempty"`
	// Namespace is the namespace-tier policy, kept un-folded.
	// +optional
	Namespace *PinningPolicy `json:"namespace,omitempty"`
}

// UpsertObservedPin upserts (by Name + Pin.Kind) one ObservedPin into list.
// When an entry with the same Name and Kind already exists, its Pin is
// replaced; otherwise a new entry is appended. Keying by both fields prevents
// a same-named dependency from different kinds (e.g. an MCP server and an
// image pin that share a name) from clobbering each other. The updated slice
// is returned; the caller is responsible for assigning it back (e.g.
// sess.Status.ObservedPins = UpsertObservedPin(sess.Status.ObservedPins, name, pin)).
func UpsertObservedPin(list []ObservedPin, name string, pin PinRecord) []ObservedPin {
	for i := range list {
		if list[i].Name == name && list[i].Pin.Kind == pin.Kind {
			list[i].Pin = pin
			return list
		}
	}
	return append(list, ObservedPin{Name: name, Pin: pin})
}
