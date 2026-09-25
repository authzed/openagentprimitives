package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories={authzed,spicebox},shortName=sbxcls
// +kubebuilder:subresource:status
// +genclient
// +genclient:nonNamespaced
//
// SpiceboxClass is the template a tool sandbox is cut from: image, resources,
// network mode, mounts, and the toolspecs and toolchains available inside it.
// A SpiceboxSession instantiates one.
//
// Cluster-scoped. Reconciled by pkg/controllers/spiceboxclass, which validates
// the spec against the selected sandbox backend (pkg/tools/sandboxkinds) and
// the referenced toolchains, surfaces Valid, and maintains the pre-warm pool.
// A class naming a missing or Valid=False toolchain goes Valid=False rather
// than starting a sandbox without the compiler it promised.
type SpiceboxClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SpiceboxClassSpec   `json:"spec,omitempty"`
	Status SpiceboxClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SpiceboxClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpiceboxClass `json:"items"`
}

type SpiceboxClassSpec struct {
	// Image is the OCI image used for sandbox pods of this class. Optional: when
	// empty, the operator's configured default sandbox image is used
	// (--sandbox-image, which `oap install` registry-qualifies for the cluster, the
	// same way it does the runner image). Set it only to pin a custom sandbox
	// image — most classes (and every identity-setup bundle) should leave it
	// unset so the image is portable across local and cloud clusters.
	// +optional
	Image string `json:"image,omitempty"`

	// RuntimeClassName selects the Kubernetes RuntimeClass. Defaults to "kata-fc" when unset.
	// +optional
	RuntimeClassName *string `json:"runtimeClassName,omitempty"`

	// Resources are the sandbox container's CPU, memory and storage limits.
	Resources SpiceboxResources `json:"resources"`

	// Network is the sandbox's egress posture.
	// +optional
	Network SpiceboxNetwork `json:"network,omitempty"`

	// Mounts are extra read-only volumes composed into every sandbox pod.
	// +optional
	Mounts []SpiceboxMount `json:"mounts,omitempty"`

	// Tools are the executables a ToolCall may dispatch inside this sandbox.
	// +optional
	Tools []SpiceboxTool `json:"tools,omitempty"`

	// Toolspecs lists SpiceboxToolspec CRs whose validation rules apply to
	// ToolCalls in sessions of this class. At least one Toolspec must cover
	// each tool in spec.tools, otherwise the class is marked Valid=False.
	// +optional
	Toolspecs []ToolspecRef `json:"toolspecs,omitempty"`

	// Toolchains names SpiceboxToolchain CRs whose payloads are composed into
	// every pod of this class as read-only overlays under /opt/ap-toolchains.
	// Each named toolchain must exist and be Valid=True or the session fails
	// closed. Order is irrelevant: the operator sorts and deduplicates.
	// +optional
	Toolchains []string `json:"toolchains,omitempty"`

	// SessionDefaults are the lifetime defaults sessions of this class inherit.
	// +optional
	SessionDefaults SessionDefaults `json:"sessionDefaults,omitempty"`

	// EnvDefaults is merged into every bundle pod's spicebox container as
	// unconditional environment variables. Used to point process-level env
	// at PrivateVolumes mount paths (e.g. GIT_DIR=/var/ap-git/.git so git's
	// metadata + url-rewrite credentials never land on the shared workspace).
	// Keys that exactly match an auth-injected env var (those declared
	// sensitive in any builtin toolkit's env.allowed) are rejected at class
	// validation.
	// +optional
	EnvDefaults map[string]string `json:"envDefaults,omitempty"`

	// PrivateVolumes are emptyDir volumes mounted only on this class's
	// bundle pods. Use these to hold per-bundle state that must NOT be
	// visible to other bundles sharing the workspace (e.g. .git directory
	// for credential isolation).
	// +optional
	PrivateVolumes []PrivateVolume `json:"privateVolumes,omitempty"`

	// Sandbox selects the backend that runs this class's sandboxes. Optional:
	// an unset kind defaults to "pod", the built-in Kubernetes Pod backend.
	//
	// The pod-vocabulary fields above (Image, RuntimeClassName, Resources,
	// Mounts, PrivateVolumes) remain the shared vocabulary every pod-shaped
	// backend consumes; this stanza carries only the discriminator and
	// backend-specific extras.
	// +optional
	Sandbox SandboxBackend `json:"sandbox,omitempty"`
}

// PrivateVolume describes a per-bundle emptyDir mount.
type PrivateVolume struct {
	// Name is the volume name (also the pod volume Name). Must be a
	// DNS-1123 label. Rejected at class validation if it collides with a
	// pod-builder-reserved volume name (work, tmp, workspace), a ConfigMap
	// mount name from spec.mounts, or another PrivateVolume.
	Name string `json:"name"`

	// MountPath is the absolute path inside the spicebox container where
	// the volume is mounted. Must start with "/". Rejected at class
	// validation if it collides with a pod-builder-reserved mount path
	// (/work, /tmp, /workspace) or another PrivateVolume.
	MountPath string `json:"mountPath"`
}

type SpiceboxResources struct {
	// CPU is the sandbox container's CPU limit.
	CPU resource.Quantity `json:"cpu"`
	// Memory is the container's memory limit, and the real cap on the
	// memory-backed /tmp and /work — see TmpSize.
	Memory resource.Quantity `json:"memory"`
	// EphemeralStorage is the container's disk-backed storage limit.
	EphemeralStorage resource.Quantity `json:"ephemeralStorage"`
	// PidsLimit caps processes inside the sandbox, bounding fork bombs.
	// +kubebuilder:default=64
	// +kubebuilder:validation:Minimum=1
	PidsLimit int64 `json:"pidsLimit,omitempty"`

	// TmpSize is the size limit of the pod's /tmp. Defaults to 50Mi when unset.
	//
	// /tmp and /work are memory-backed (tmpfs) emptyDirs. The size here is a CAP,
	// not a reservation: pages are charged against this class's Memory limit only
	// as they are written. So raising TmpSize alone buys nothing — the memory
	// limit binds first, and the pod is OOM-killed rather than told ENOSPC. Raise
	// Memory alongside it.
	//
	// Prefer pointing a tool's scratch at the disk-backed /var/ap-cache over
	// growing tmpfs at all: TMPDIR, GOCACHE, PNPM_HOME and the XDG roots already
	// resolve there when a toolchain is attached, so /tmp holds only what a tool
	// writes in defiance of them.
	// +optional
	TmpSize *resource.Quantity `json:"tmpSize,omitempty"`

	// WorkSize is the size limit of the pod's /work. Defaults to 100Mi when
	// unset. Memory-backed; see TmpSize.
	// +optional
	WorkSize *resource.Quantity `json:"workSize,omitempty"`

	// CacheSize is the size limit of the disk-backed /var/ap-cache volume the
	// toolchain overlay mounts. Defaults to 2Gi when unset; only takes effect
	// when a toolchain is attached (no toolchains, no cache volume).
	//
	// Unlike TmpSize/WorkSize this is DISK-backed, not memory-backed: it holds
	// the large consumers a toolchain redirects here — GOCACHE, GOMODCACHE,
	// PNPM_HOME, the XDG roots and TMPDIR. The pod builder raises the
	// container's ephemeral-storage limit by exactly this value, so raising it
	// is charged against node disk, not Memory. Raise it for a build box that
	// resolves a large dependency graph (e.g. reviewing a Go dependency-bump
	// PR): the module + build caches easily exceed 2Gi and the kubelet evicts
	// the pod when the volume passes its SizeLimit.
	// +optional
	CacheSize *resource.Quantity `json:"cacheSize,omitempty"`
}

// Scratch-volume defaults. Read them through EffectiveTmpSize /
// EffectiveWorkSize / EffectiveCacheSize rather than the pointers, so "unset"
// means the same thing in the pod builder and in validation.
var (
	DefaultTmpSize  = resource.MustParse("50Mi")
	DefaultWorkSize = resource.MustParse("100Mi")
	// DefaultCacheSize is the historical /var/ap-cache SizeLimit, applied when
	// a toolchain-bearing class does not set CacheSize.
	DefaultCacheSize = resource.MustParse("2Gi")
)

// DefaultSandboxKind is the backend a class gets when spec.sandbox.kind is
// unset. Single source of truth for that name: the CRD default marker, the
// pod kind's registry key, and SandboxBackend.ResolvedKind all derive from it.
const DefaultSandboxKind = "pod"

// EffectiveTmpSize returns TmpSize, or the default when unset.
func (r SpiceboxResources) EffectiveTmpSize() resource.Quantity {
	if r.TmpSize != nil {
		return r.TmpSize.DeepCopy()
	}
	return DefaultTmpSize.DeepCopy()
}

// EffectiveWorkSize returns WorkSize, or the default when unset.
func (r SpiceboxResources) EffectiveWorkSize() resource.Quantity {
	if r.WorkSize != nil {
		return r.WorkSize.DeepCopy()
	}
	return DefaultWorkSize.DeepCopy()
}

// EffectiveCacheSize returns CacheSize, or the default when unset.
func (r SpiceboxResources) EffectiveCacheSize() resource.Quantity {
	if r.CacheSize != nil {
		return r.CacheSize.DeepCopy()
	}
	return DefaultCacheSize.DeepCopy()
}

// +kubebuilder:validation:Enum=none;allowlist
type NetworkMode string

const (
	NetworkModeNone      NetworkMode = "none"
	NetworkModeAllowlist NetworkMode = "allowlist"
)

type SpiceboxNetwork struct {
	// Mode is the egress posture: none denies all, allowlist permits DNS plus
	// coarse outbound TCP.
	// +kubebuilder:default=none
	Mode NetworkMode `json:"mode,omitempty"`
	// AllowedHosts is the intended hostname allowlist. RECORDED, not enforced:
	// stock NetworkPolicy cannot express hostnames, so a DNS-aware policy
	// controller must consume it to narrow beyond Mode's L3/L4 rules.
	// +optional
	AllowedHosts []string `json:"allowedHosts,omitempty"`
}

type SpiceboxMount struct {
	// Name is the pod volume name.
	Name string `json:"name"`
	// Source is where the mounted content comes from.
	Source MountSource `json:"source"`
	// MountPath is the absolute path inside the sandbox container.
	MountPath string `json:"mountPath"`
	// Format describes what the source content is. Defaults to raw, which is
	// a direct read-only volume mount and is what every mount predating this
	// field does.
	// +kubebuilder:default=raw
	// +optional
	Format MountFormat `json:"format,omitempty"`
	// Digest is the expected content identity ("sha256:..."), verified before
	// the content is used. Empty skips verification.
	// +optional
	Digest string `json:"digest,omitempty"`
}

type MountSource struct {
	// ConfigMapRef names a ConfigMap in the session's namespace.
	// +optional
	ConfigMapRef *corev1.LocalObjectReference `json:"configMapRef,omitempty"`
}

// MountFormat describes what a mount's source content IS. The builder derives
// the delivery machinery from it: raw mounts the volume directly, tarGz needs
// an initContainer to expand the archive first.
// +kubebuilder:validation:Enum=raw;tarGz
type MountFormat string

const (
	MountFormatRaw   MountFormat = "raw"
	MountFormatTarGz MountFormat = "tarGz"
)

// +kubebuilder:validation:Enum=short;long;persistent
type ExpectedDuration string

const (
	ExpectedDurationShort      ExpectedDuration = "short"
	ExpectedDurationLong       ExpectedDuration = "long"
	ExpectedDurationPersistent ExpectedDuration = "persistent"
)

type SpiceboxTool struct {
	// Name is how a ToolCall refers to this tool.
	Name string `json:"name"`
	// Command is the absolute path + argv[0] of the tool binary.
	Command []string `json:"command"`
	// DefaultArgs are prepended to every invocation's arguments.
	// +optional
	DefaultArgs []string `json:"defaultArgs,omitempty"`
	// ExpectedDuration picks the dispatch mode: short runs synchronously, long
	// and persistent stream.
	// +kubebuilder:default=short
	ExpectedDuration ExpectedDuration `json:"expectedDuration,omitempty"`
}

// ToolspecRef is a cluster-scoped reference to a SpiceboxToolspec.
type ToolspecRef struct {
	// Name is the SpiceboxToolspec CR name.
	Name string `json:"name"`
}

type SessionDefaults struct {
	// IdleTTL is how long a session may sit idle before it is terminated.
	// +kubebuilder:default="30m"
	IdleTTL metav1.Duration `json:"idleTTL,omitempty"`
	// MaxDuration is the wall-clock lifetime cap on a session.
	// +kubebuilder:default="4h"
	MaxDuration metav1.Duration `json:"maxDuration,omitempty"`
}

type SpiceboxClassStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries Valid; see SpiceboxClassConditionValid.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ToolspecCoverage maps each tool name in spec.tools to the names of the
	// SpiceboxToolspecs (from spec.toolspecs) that authorize it. A tool with
	// no covering Toolspec causes Valid=False.
	// +optional
	ToolspecCoverage map[string][]string `json:"toolspecCoverage,omitempty"`

	// ResolvedToolchains is the class's toolchain names resolved to concrete
	// mounts, recorded so an operator can answer "what is this class actually
	// running?" without reading controller logs.
	//
	// OBSERVATION ONLY. Nothing consumes it for correctness — sessions resolve
	// fresh at bind and freeze their own copy. A stale value here can therefore
	// never mis-shape a session; at worst it mis-shapes the warm pool, which
	// self-heals when the next reconcile re-points the template.
	// +optional
	ResolvedToolchains []ToolchainMount `json:"resolvedToolchains,omitempty"`

	// ToolchainSetDigest is the digest of the resolved set. Computed by the SAME
	// function the session path uses, so this and
	// SpiceboxSession.status.toolchainSetDigest are directly comparable by eye —
	// which is how an operator confirms a session's pod matches the warm pool's
	// shape.
	// +optional
	ToolchainSetDigest string `json:"toolchainSetDigest,omitempty"`

	// ToolchainResolutionMessage explains why resolution failed, when it did.
	// Pre-warming is skipped in that case but the class stays Valid: class
	// validity must not depend on the toolchain catalog, or an unrelated
	// toolchain edit would flip classes red that never asked for a pool. This
	// field is what keeps that decision from making the failure invisible.
	// +optional
	ToolchainResolutionMessage string `json:"toolchainResolutionMessage,omitempty"`
}

func init() {
	SchemeBuilder.Register(&SpiceboxClass{}, &SpiceboxClassList{})
}

// SandboxBackend selects and configures a sandbox backend.
type SandboxBackend struct {
	// Kind names a registered sandbox kind. Defaults to "pod". An
	// UNRECOGNIZED kind marks the class Valid=False — lookup never falls back
	// to the built-in backend, because silently installing a different
	// substrate than the one asked for is worse than refusing.
	// +kubebuilder:default=pod
	// +optional
	Kind string `json:"kind,omitempty"`

	// Config is passed through to the backend verbatim. Opaque to AP: each
	// kind parses its own. Unset for the built-in pod backend, which takes all
	// of its configuration from the pod-vocabulary fields above.
	// +optional
	Config *apiextensionsv1.JSON `json:"config,omitempty"`

	// WarmPool requests pre-warmed capacity for this backend. Unset or
	// replicas: 0 means pre-warming is off — it spends real money on idle
	// capacity, so it is opt-in. Only a backend whose Runtime implements the
	// sandboxkinds.Prewarmer interface can honor a non-zero value; asking for
	// it on a backend that cannot is a class validation error, not a silent
	// no-op.
	//
	// PREREQUISITE: pre-warming only actually adopts a pod when the
	// cluster's agent-sandbox install allows the
	// "agentprimitives.authzed.com" label domain (its AllowedLabelDomains
	// defaults to "sandbox.users.io" alone). Without that grant AP cannot
	// label an adopted pod, and — because AP's per-session NetworkPolicy
	// selects on that label — the backend degrades to ordinary cold
	// sandboxes and emits a monitoring warning rather than run one
	// unpoliced.
	// +optional
	WarmPool *WarmPoolConfig `json:"warmPool,omitempty"`
}

// WarmPoolConfig is how much pre-warmed capacity to keep, and where.
type WarmPoolConfig struct {
	// Replicas is the number of sandboxes kept ready, PER NAMESPACE listed
	// below. 0 disables pre-warming.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Namespaces lists the namespaces to keep pre-warmed capacity in; a
	// separate pool is created in EACH. SpiceboxClass is cluster-scoped but the
	// pool objects are namespaced, and adoption only ever looks a pool up in
	// the ADOPTING SESSION's own namespace — so a pool in a namespace nothing
	// runs sessions in is pure idle spend nothing can adopt. Naming them
	// explicitly rather than discovering them is deliberate: pre-warming spends
	// real money, so the cluster owner states exactly where, as they state
	// exactly how many.
	//
	// Replicas > 0 with an empty Namespaces is a class validation error, not a
	// silent no-op.
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`
}

// ResolvedKind returns the backend name to look up: the explicit kind, or the
// documented default when unset.
//
// The kubebuilder default fills this in at the API server, so an empty value
// should never reach a controller in production. Resolving it here anyway keeps
// the meaning of "unset" in one place rather than depending on the served
// schema being current, and makes the path reachable from a unit test.
func (b SandboxBackend) ResolvedKind() string {
	if b.Kind == "" {
		return DefaultSandboxKind
	}
	return b.Kind
}
