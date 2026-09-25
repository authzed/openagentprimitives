package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,spicebox},shortName=sbxtb
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Valid",type="string",JSONPath=".status.conditions[?(@.type=='Valid')].status"
// +kubebuilder:printcolumn:name="Reachable",type="string",JSONPath=".status.conditions[?(@.type=='Reachable')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient
//
// SidecarToolbox declares a user-supplied MCP server that runs as a sidecar
// container alongside the agent runner pod: its image, sandbox shape, tool
// allowlist, CEL constraints and egress policy.
//
// Namespaced. Reconciled by pkg/controllers/sidecartoolbox, which validates
// the spec and runs a one-shot probe Pod to confirm the image is reachable and
// its live tool list matches the allowlist. Upstream credentials are resolved
// by the AgentSession reconciler at pod-create time and frozen for the life of
// the pod -- there is no just-in-time refresh for a sidecar.
type SidecarToolbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SidecarToolboxSpec   `json:"spec,omitempty"`
	Status SidecarToolboxStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SidecarToolboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SidecarToolbox `json:"items"`
}

type SidecarToolboxSpec struct {
	// Name is the toolbox's toolspec-level name.
	Name string `json:"name"`
	// Version is the spec author's version of this declaration.
	Version string `json:"version"`
	// Intent is the one-line purpose shown to authors and reviewers.
	Intent string `json:"intent,omitempty"`

	// Source is where the sidecar's container image comes from.
	Source SidecarToolboxSource `json:"source"`
	// Sandbox is the SpiceboxClass shape and egress the sidecar runs under.
	Sandbox SidecarToolboxSandbox `json:"sandbox"`
	// Transport is how the runner reaches the sidecar's MCP endpoint.
	Transport SidecarToolboxTransport `json:"transport"`
	// UpstreamAuth is the credential the sidecar needs for its own upstream.
	UpstreamAuth SidecarToolboxUpstream `json:"upstreamAuth"`

	// Config is an opaque configuration document for the sidecar image,
	// delivered verbatim as the AP_SIDECAR_CONFIG environment variable. The
	// platform does not interpret it; validation belongs to whoever admits the
	// object (the workshop webhook parses it for first-party images known to
	// consume one, e.g. ap-api-adapter). Bounded because it rides in the pod
	// environment.
	//
	// MaxLength counts Unicode code points, not bytes — up to 4 bytes each in
	// UTF-8 — so it alone admits up to 262144 bytes. The BYTE cap (what execve
	// and apiadapter.MaxConfigBytes both count) is enforced by
	// apiadapter.Parse, which both the workshop webhook (at admission) and the
	// adapter binary (at boot) run. Deliberately NOT a CEL XValidation here:
	// this spec type is embedded by value into AgentSession's
	// status.resolvedSidecarToolboxes snapshot, and a CEL rule that reaches a
	// status path both trips the status-CEL guard (pkg/platform/settings'
	// TestNoUnguardedStatusCELRules) and blows the apiserver's CRD rule-cost
	// budget inside that unbounded status array — the agentsessions CRD is
	// then refused at install. Residual admin-path risk (no webhook, a
	// >131072-byte multi-byte document): execve fails with E2BIG and the
	// kubelet reports the container-start error on the Pod.
	// +optional
	// +kubebuilder:validation:MaxLength=65536
	Config string `json:"config,omitempty"`

	// Tools mirrors MCPServerTool field-for-field — same allowlist, CEL,
	// deny effects, sensitive fields. See MCPServerTool for details.
	Tools []MCPServerTool `json:"tools"`

	// ToolResourceMap declares, per-tool, the SpiceDB resource a read accesses —
	// identical semantics to MCPServer.spec.toolResourceMap. Consumed by the
	// runner's information-leakage gate so a sidecar tool participates in
	// per-datum egress (its result mints a pt-tag) instead of falling to the
	// undeclared coarse floor.
	// +optional
	ToolResourceMap []ToolResourceMapping `json:"toolResourceMap,omitempty"`

	// SpiceDBSchema declares the SpiceDB resource definitions this toolbox
	// contributes — identical to MCPServer.spec.spicedbSchema. The guardian's
	// schema composer concatenates fragments across every MCPServer AND
	// SidecarToolbox in the cluster so the audiences the tags derive from
	// resolve; identical declarations dedupe, conflicting ones fail the reconcile.
	// +optional
	SpiceDBSchema *SpiceDBSchemaFragment `json:"spicedbSchema,omitempty"`

	// MCPUIAppTools is ACCEPTED BUT NOT CONSUMED: setting enabled: true changes
	// nothing, and no error, log, or condition says so. It is typed identically
	// to MCPServer's stanza so a toolbox can declare the intent to be an MCP-UI
	// app-tool origin, but nothing reads it, so no sidecar tool is
	// browser-callable. Recorded, not enforced — the same posture as this CRD's
	// effectiveAllowedHosts.
	//
	// A toolbox tool named in AgentUI.spec.tools is therefore denied by the
	// three-way grant's origin condition: the right fail-closed answer for an
	// unwired origin, just not the one this field's name suggests.
	//
	// Absent ⇒ disabled: the permanent, fail-closed default.
	// +optional
	MCPUIAppTools *MCPUIAppToolsSpec `json:"mcpUiAppTools,omitempty"`

	// SecretInputs binds secret-output handles (produced by an earlier tool
	// call) to this toolbox. A toolbox with any SecretInput is "secret-gated":
	// it is NOT injected into the agent pod; the operator runs it as a separate
	// per-session pod once the bound secret is available.
	// +optional
	SecretInputs []SidecarToolboxSecretInput `json:"secretInputs,omitempty"`

	// Isolation declares that this toolbox must run in its own per-session pod
	// even though it has no SecretInputs. A secret-gated toolbox (any
	// SecretInputs) is ALREADY isolated for its own reason -- its gating
	// secret only exists per-session, so it cannot be baked into the runner
	// pod's spec -- regardless of this field; Isolation is for the toolbox
	// that needs a separate pod's consequences WITHOUT a gating secret:
	//
	//   - Its own NetworkPolicy, scoped to exactly this sidecar's ingress/
	//     egress, instead of inheriting the runner pod's shared policy (see
	//     pkg/controllers/agentsession/netpol.go: BuildRunnerNetworkPolicy vs
	//     BuildSidecarNetworkPolicy).
	//   - Eligibility for BuildSidecarPod's identity branch (a projected
	//     ServiceAccount token + operator env), which is reachable only in
	//     separate-pod mode -- an in-pod sidecar can never receive it.
	//
	// Absent (or "auto") never changes an existing toolbox's run mode:
	// today's in-pod behavior is preserved exactly.
	// +optional
	// +kubebuilder:validation:Enum=auto;isolated
	Isolation SidecarToolboxIsolation `json:"isolation,omitempty"`
}

// SidecarToolboxIsolation is SidecarToolboxSpec.Isolation's type. See that
// field's doc comment for what each value buys.
type SidecarToolboxIsolation string

const (
	// SidecarToolboxIsolationAuto is the default: Isolation contributes
	// nothing to the run-mode decision, so a non-secret-gated toolbox runs
	// in-pod exactly as it always has.
	SidecarToolboxIsolationAuto SidecarToolboxIsolation = "auto"
	// SidecarToolboxIsolationIsolated forces a separate per-session pod even
	// when the toolbox has no SecretInputs.
	SidecarToolboxIsolationIsolated SidecarToolboxIsolation = "isolated"
)

type SidecarToolboxSecretInput struct {
	// Name is the env-var name (when Deliver=="env") or the logical key.
	Name string `json:"name"`
	// Deliver is how the value reaches the sidecar at startup: "env" (env var
	// named Name) or "file:<path>" (mounted file at <path>).
	// +kubebuilder:validation:Pattern=`^(env|file:/.+)$`
	Deliver string `json:"deliver"`
	// From is the secret-output logical name the producer emitted (matches the
	// producer's secretOutput.name / the per-session Secret key).
	From string `json:"from"`
}

// SidecarToolboxSource is a discriminated union — exactly one of Image
// or Inline must be set. Validated by the kind's ValidateFile and the
// controller's spec phase.
type SidecarToolboxSource struct {
	// Image is a prebuilt container image reference.
	// +optional
	Image string `json:"image,omitempty"`
	// Inline builds the sidecar from a script layered onto a base image.
	// +optional
	Inline *SidecarToolboxInlineSource `json:"inline,omitempty"`
}

type SidecarToolboxInlineSource struct {
	// BaseImage is the image the script runs on top of.
	BaseImage string `json:"baseImage"`
	// Script is where the server's source comes from.
	Script SidecarToolboxScriptSource `json:"script"`
	// Entrypoint is the argv that starts the server inside the container.
	Entrypoint []string `json:"entrypoint"`
}

type SidecarToolboxScriptSource struct {
	// ConfigMapRef names the ConfigMap key holding the script body.
	ConfigMapRef SidecarToolboxConfigMapKeyRef `json:"configMapRef"`
}

type SidecarToolboxConfigMapKeyRef struct {
	// Name is the ConfigMap's name, in the toolbox's own namespace.
	Name string `json:"name"`
	// Key is the data key within the ConfigMap.
	Key string `json:"key"`
}

type SidecarToolboxSandbox struct {
	// Class names a SpiceboxClass (cluster-scoped) whose resources,
	// runtimeClassName, default network mode, pidsLimit are inherited.
	Class string `json:"class"`
	// Network widens the class's egress for this toolbox.
	// +optional
	Network SidecarToolboxNetwork `json:"network,omitempty"`
}

type SidecarToolboxNetwork struct {
	// AllowedHosts merges with the SpiceboxClass's network.allowedHosts.
	// If the class's mode is "none" and any AllowedHosts are supplied
	// here, the effective sidecar network upgrades to "allowlist" with
	// exactly these hosts.
	// +optional
	AllowedHosts []string `json:"allowedHosts,omitempty"`
}

type SidecarToolboxTransport struct {
	// Port is advisory — the operator allocates a free port at pod-build
	// time and sets MCP_PORT in the sidecar's env regardless. Sidecar
	// code is required to read MCP_PORT to bind.
	// +kubebuilder:default=8080
	Port int32 `json:"port,omitempty"`
	// Path is the HTTP path the sidecar's MCP Streamable-HTTP endpoint listens
	// on (e.g. "/mcp"); empty means the pod root "/". The runner appends it to
	// the pod URL for BOTH the tools/list reachability probe and tool-call
	// dispatch, so it must match where the sidecar actually serves MCP. A
	// leading slash is optional — the runner normalizes it.
	// +optional
	Path string `json:"path,omitempty"`
	// Healthcheck is how the operator's probe decides the sidecar is up.
	// +optional
	Healthcheck SidecarToolboxHealthcheck `json:"healthcheck,omitempty"`
}

type SidecarToolboxHealthcheck struct {
	// Path is the HTTP path probed for readiness.
	// +kubebuilder:default=/healthz
	Path string `json:"path,omitempty"`
	// TimeoutSeconds bounds how long the probe waits for the sidecar to answer.
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=1
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// UpstreamAuthProviderNone is the explicit sentinel for
// SidecarToolboxUpstream.Provider that opts a sidecar OUT of the
// AgentIdentity/provider-library credential path. Its credential is instead a
// projected ServiceAccount token + bearer, minted by a controller (the
// AgentSession/Workshop reconcilers) — never a credential resolved from the
// /providers/ library. It is a valid, non-empty Provider value: only a truly
// empty Provider is refused, because the field must always state explicit
// intent (a real provider name, or this sentinel), never an accidental
// omission.
const UpstreamAuthProviderNone = "none"

type SidecarToolboxUpstream struct {
	// Provider names a provider in the /providers/ library, or the
	// UpstreamAuthProviderNone sentinel ("none") for a controller-issued-token
	// sidecar that needs no AgentIdentity credential. Drives
	// `oap agent setup-identity` and projects the resolved credential
	// into the sidecar's env at session boot via the toolbox: authkind.
	Provider string `json:"provider"`

	// EnvVar is the environment variable name the resolved upstream
	// credential is injected into (in the per-session sidecar Secret).
	// Empty means the sidecar needs no upstream credential (e.g. the echo
	// example) — the per-session Secret is written empty.
	// +optional
	EnvVar string `json:"envVar,omitempty"`
}

type SidecarToolboxStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// LastValidatedAt is when the spec last passed validation.
	// +optional
	LastValidatedAt *metav1.Time `json:"lastValidatedAt,omitempty"`
	// ObservedTools is the tool list the admission-time probe saw. Not read at
	// runtime — the runner's live per-session probe is authoritative there.
	// +optional
	ObservedTools []string `json:"observedTools,omitempty"`

	// Pin is the recorded pin baseline in the common PinRecord shape:
	// Digest = kubelet-resolved image digest (sha256:…), Version = declared
	// image tag (audit metadata). Strength reflects the declared ref: frozen
	// iff the source image is by-digest, named iff by-tag, else unpinned.
	// +optional
	Pin *PinRecord `json:"pin,omitempty"`

	// Conditions carries Valid, Reachable and PinDrift.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

const (
	SidecarToolboxConditionValid     = "Valid"
	SidecarToolboxConditionReachable = "Reachable"
	// SidecarToolboxConditionSpiceDBSchemaValid is the guardian-owned condition
	// reporting whether this toolbox's spec.spicedbSchema fragment was accepted
	// into the cluster-wide compose. False (with FragmentInvalid/FragmentConflict)
	// means the fragment was excluded so ONE bad or hostile toolbox cannot freeze
	// schema convergence for every session — the same isolation MCPServer gets.
	SidecarToolboxConditionSpiceDBSchemaValid = "SpiceDBSchemaValid"
)

// SidecarToolbox SpiceDBSchemaValid reasons, mirroring the MCPServer set:
// FragmentInvalid = failed per-fragment ValidateFragment on its own;
// FragmentConflict = valid alone but collided with an already-accepted fragment;
// FragmentValid = stamped True only to clear a previously-invalid fragment.
const (
	SidecarToolboxReasonFragmentInvalid  = "FragmentInvalid"
	SidecarToolboxReasonFragmentConflict = "FragmentConflict"
	SidecarToolboxReasonFragmentValid    = "FragmentValid"
)

// LookupToolResourceMapping returns the mapping for tool name `name`, or nil.
// Mirrors MCPServerSpec.LookupToolResourceMapping.
func (s *SidecarToolboxSpec) LookupToolResourceMapping(name string) *ToolResourceMapping {
	for i := range s.ToolResourceMap {
		if s.ToolResourceMap[i].Tool == name {
			return &s.ToolResourceMap[i]
		}
	}
	return nil
}

func init() {
	SchemeBuilder.Register(&SidecarToolbox{}, &SidecarToolboxList{})
}
