package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,spicebox},shortName=sbxses
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Class",type="string",JSONPath=".spec.class"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient
//
// SpiceboxSession is one live sandbox instantiated from a SpiceboxClass: the
// long-lived pod that this session's ToolCalls execute inside, plus its
// workspace, idle TTL and default env.
//
// Namespaced. Reconciled by pkg/controllers/spiceboxsession, which owns the
// sandbox pod through the registered backend (pkg/tools/sandboxkinds -- pod or
// agent-sandbox). An AgentSession that runs sandbox tools creates and owns one
// of these; it is also usable on its own, without an agent.
type SpiceboxSession struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SpiceboxSessionSpec   `json:"spec,omitempty"`
	Status SpiceboxSessionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SpiceboxSessionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpiceboxSession `json:"items"`
}

type SpiceboxSessionSpec struct {
	// Class names the cluster-scoped SpiceboxClass to instantiate.
	Class string `json:"class"`

	// Agent names an AgentIdentity in the same namespace whose credential
	// bindings are used as the default for ToolCalls in this session.
	// Optional; if unset and ToolCall.spec.agent is also unset, calls run with
	// no AgentIdentity-injected env.
	// +optional
	Agent string `json:"agent,omitempty"`

	// IdleTTL overrides the class's idle timeout for this session.
	// +optional
	IdleTTL *metav1.Duration `json:"idleTTL,omitempty"`

	// MaxDuration overrides the class's wall-clock lifetime cap.
	// +optional
	MaxDuration *metav1.Duration `json:"maxDuration,omitempty"`

	// Workspace selects whether the sandbox shares a workspace volume or gets
	// its own.
	// +optional
	Workspace WorkspaceConfig `json:"workspace,omitempty"`

	// DefaultEnv is non-secret environment merged into every ToolCall in this
	// session, at lower precedence than ToolCall.spec.env and any
	// AgentIdentity-injected env. Used by the AgentSession controller to stamp
	// static tool-hardening env (e.g. GIT_CONFIG_GLOBAL) onto bundle sessions.
	// +optional
	DefaultEnv map[string]string `json:"defaultEnv,omitempty"`

	// Toolspecs optionally narrows the Class's toolspec set for this session.
	// If set, every entry must appear in the resolved Class's spec.toolspecs.
	// If unset, the session inherits the full class set.
	// +optional
	Toolspecs []ToolspecRef `json:"toolspecs,omitempty"`

	// Mounts are the data mounts this session's pod materializes, resolved by
	// the AgentSession controller. Concatenated with the class's authored
	// mounts by the pod builder, which does not care which provenance a mount
	// came from.
	// +optional
	Mounts []SpiceboxMount `json:"mounts,omitempty"`

	// SkillBundles is DEPRECATED; superseded by Mounts. Retained so a session
	// created before mounts existed and re-hydrated afterwards still builds:
	// sessions are wakeable, so a removal would break in-flight work mid-upgrade.
	// Removal is Task 11 of the sandbox-data-mounts plan, not an aspiration.
	// +optional
	SkillBundles []SkillBundleMount `json:"skillBundles,omitempty"`

	// Sandbox overrides the referenced SpiceboxClass's sandbox backend for this
	// session. Stamped by the AgentSession reconciler with the tier-resolved
	// decision; unset for a directly-created session, which then uses the
	// class's own setting. Folded into status.resolvedClass at bind time.
	// +optional
	Sandbox *SandboxBackend `json:"sandbox,omitempty"`

	// ToolConfig is a snapshot of the owning AgentClass.spec.config, stamped by
	// the AgentSession controller at session creation. The toolcall controller
	// exposes it to toolspec constraint CEL as the `config` root. Snapshotted
	// (not read live) so a mid-session class edit cannot silently change the
	// authority a running session was admitted under.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	ToolConfig map[string]apiextensionsv1.JSON `json:"toolConfig,omitempty"`
}

// SkillBundleMount names one staged skill bundle the sandbox pod unpacks. The
// ConfigMap (created by the AgentSession controller in the session namespace)
// holds the gzipped tar under "bundle.tar.gz"; the init container extracts it
// to /skills/<MountName>/.
type SkillBundleMount struct {
	// MountName is the sanitized, collision-free directory name the bundle
	// unpacks to under /skills/<MountName>/.
	MountName string `json:"mountName"`
	// ConfigMapName is the per-session ConfigMap holding the bundle tarball
	// (binaryData key "bundle.tar.gz").
	ConfigMapName string `json:"configMapName"`
}

// +kubebuilder:validation:Enum=shared;isolated
type WorkspaceMode string

const (
	WorkspaceShared   WorkspaceMode = "shared"
	WorkspaceIsolated WorkspaceMode = "isolated"
)

type WorkspaceConfig struct {
	// Mode is whether /workspace is a shared RWX volume or private to this
	// sandbox.
	// +kubebuilder:default=shared
	Mode WorkspaceMode `json:"mode,omitempty"`

	// SharedClaimName is the name of the RWX PersistentVolumeClaim to mount
	// at /workspace. Set by the AgentSession controller for bundle sessions
	// when a workspace StorageClass is configured. Ignored when Mode is
	// isolated or this is empty.
	// +optional
	SharedClaimName string `json:"sharedClaimName,omitempty"`
}

type SpiceboxSessionStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// PodName is the sandbox Pod managed by this session.
	// +optional
	PodName string `json:"podName,omitempty"`

	// LastActivityAt is the most recent time a ToolCall completed against this session.
	// +optional
	LastActivityAt *metav1.Time `json:"lastActivityAt,omitempty"`

	// CallCount is a monotonic counter of completed ToolCalls.
	// +optional
	CallCount int64 `json:"callCount,omitempty"`

	// ResolvedClass is a snapshot of the SpiceboxClass spec at bind time.
	// Tool catalogs are frozen for the session lifetime, even if the class changes.
	// +optional
	ResolvedClass *SpiceboxClassSpec `json:"resolvedClass,omitempty"`

	// ResolvedToolchains is the frozen, self-contained resolution of the
	// class's spec.toolchains, captured at first bind alongside ResolvedClass.
	// The pod builder reads only this — it never re-reads SpiceboxToolchain CRs,
	// so a catalog edit mid-session cannot change a running pod.
	// +optional
	ResolvedToolchains []ToolchainMount `json:"resolvedToolchains,omitempty"`

	// ToolchainSetDigest is a stable hex sha256 over the (name, image) pairs of
	// ResolvedToolchains, order-independent and image-sensitive. Recorded
	// alongside the freeze and carried into the toolchain-audit "resolved" entry
	// so an investigation can match a session's pod to what was attested.
	// +optional
	ToolchainSetDigest string `json:"toolchainSetDigest,omitempty"`

	// ResolvedAgent is a snapshot of spec.agent at session bind time. Frozen
	// for the session lifetime so a mid-flight Session edit doesn't change
	// identity for an in-flight session.
	// +optional
	ResolvedAgent string `json:"resolvedAgent,omitempty"`

	// EffectiveToolspecs is the resolved set of SpiceboxToolspec names in use
	// for this session — equal to spec.toolspecs if non-empty, else the
	// resolved class's spec.toolspecs. Recomputed on session and class changes.
	// +optional
	EffectiveToolspecs []string `json:"effectiveToolspecs,omitempty"`

	// Sandbox is the durable handle to this session's sandbox, written by the
	// SpiceboxSession controller once the backend has created it. It is the
	// only link the ToolCall controller has to the sandbox, and it must
	// survive an operator restart, so it lives here rather than in memory.
	// +optional
	Sandbox *SandboxHandle `json:"sandbox,omitempty"`

	// Conditions carries Ready, Progressing, Failed and Terminated.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

func init() {
	SchemeBuilder.Register(&SpiceboxSession{}, &SpiceboxSessionList{})
}

// SandboxHandle is the CRD-serializable form of sandboxkinds.Handle: an opaque
// reference only the owning kind interprets.
type SandboxHandle struct {
	// Kind is the sandbox kind that owns Ref.
	Kind string `json:"kind"`
	// Ref is the backend's own identifier for this sandbox. The pod backend
	// uses "<namespace>/<podName>"; another backend may use a bare ID.
	Ref string `json:"ref"`
	// Prewarmed reports whether this sandbox was adopted from a pool of
	// already-running capacity rather than created on demand.
	//
	// LOAD-BEARING, not decoration. A pre-warming backend may resolve Ref
	// differently for an adopted sandbox than for a cold one, and every verb
	// (status, teardown, exec) resolves from the stored handle alone. It is
	// persisted here, rather than recomputed, precisely so it survives an
	// operator restart: recomputing it would ask a question whose inputs no
	// longer exist once the sandbox is running.
	// +optional
	Prewarmed bool `json:"prewarmed,omitempty"`
}
