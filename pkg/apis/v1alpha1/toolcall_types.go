package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabelToolUseID is the ToolCall metadata label carrying the LLM-emitted
// tool_use ID (the RAW id, not the RFC-1123-normalized form the object name
// uses). The runner stamps it when it builds the ToolCall; the toolcall
// controller reads it to make the per-dispatch tool_dispatch_snapshot audit
// entry ID unique. It is the ONLY component of that ID that distinguishes two
// dispatches which land on the same (turnIndex, sequence) — turnIndex resets
// to 0 on session resume and the memory scope (namespace/name) is reused
// across recreated sessions that share a deterministic name — so an unstamped
// (empty) value collapses the ID to `tds-<turn>-<seq>-` and two such runs
// collide on an append-only write.
const LabelToolUseID = "agentprimitives.authzed.com/toolUseID"

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,spicebox},shortName=tc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Session",type="string",JSONPath=".spec.session"
// +kubebuilder:printcolumn:name="Tool",type="string",JSONPath=".spec.tool"
// +kubebuilder:printcolumn:name="Exit",type="integer",JSONPath=".status.exitCode"
// +kubebuilder:printcolumn:name="Succeeded",type="string",JSONPath=".status.conditions[?(@.type=='Succeeded')].status"
// +kubebuilder:printcolumn:name="Failed",type="string",JSONPath=".status.conditions[?(@.type=='Failed')].status"
// +kubebuilder:printcolumn:name="Timeout",type="string",JSONPath=".status.conditions[?(@.type=='Timeout')].status",priority=1
// +kubebuilder:printcolumn:name="Canceled",type="string",JSONPath=".status.conditions[?(@.type=='Canceled')].status",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient
//
// ToolCall is one execution of one sandbox tool inside a SpiceboxSession: the
// tool name, its args, non-secret env, stdin, and credential references --
// never token bytes. The runner creates it; the outcome lands on status.
//
// Namespaced. Reconciled by pkg/controllers/toolcall, which validates the call
// against the session's resolved toolspecs, hands the credential descriptors
// to the token broker to materialize exec env, and runs the process. Secrets
// supplied through spec.env are rejected rather than passed through.
type ToolCall struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ToolCallSpec   `json:"spec,omitempty"`
	Status ToolCallStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ToolCallList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ToolCall `json:"items"`
}

// +kubebuilder:validation:Enum=sync;stream;interactive
type ToolCallMode string

const (
	ToolCallModeSync        ToolCallMode = "sync"
	ToolCallModeStream      ToolCallMode = "stream"
	ToolCallModeInteractive ToolCallMode = "interactive"
)

type ToolCallSpec struct {
	// Session is the name of the SpiceboxSession (same namespace) this call targets.
	Session string `json:"session"`

	// Tool is the name of the tool from the session's class catalog.
	Tool string `json:"tool"`

	// Credentials is the resolved credential descriptor set the runner
	// stamps onto the ToolCall. The toolcall controller hands it to the
	// token broker to materialize exec env vars. References only — never
	// token bytes. When empty, no credentials are injected.
	// +optional
	Credentials []CredentialDescriptor `json:"credentials,omitempty"`

	// Args is the argv passed to the tool, after the class's DefaultArgs.
	// +optional
	Args []string `json:"args,omitempty"`

	// Env is non-secret configuration for the tool process. Use a bound
	// AgentIdentity for any secret material; supplying secrets here is an
	// error — the operator rejects keys whose name matches the well-known
	// secret-name pattern.
	// +optional
	Env map[string]string `json:"env,omitempty"`

	// Stdin is inline bytes piped to the tool's stdin.
	// For large payloads, use InputArtifacts instead.
	// +optional
	Stdin string `json:"stdin,omitempty"`

	// InputArtifacts are stored payloads materialized into the pod before exec.
	// +optional
	InputArtifacts []InputArtifact `json:"inputArtifacts,omitempty"`

	// Workspace selects which workspace volume the tool runs against.
	// +optional
	Workspace WorkspaceConfig `json:"workspace,omitempty"`

	// CaptureOutputs lists paths (inside the pod) to harvest after exec.
	// Supports template variables: {{.callId}}, {{.sessionName}}.
	// +optional
	CaptureOutputs []string `json:"captureOutputs,omitempty"`

	// Timeout is the hard deadline for the tool process.
	// +kubebuilder:default="60s"
	Timeout metav1.Duration `json:"timeout,omitempty"`

	// Mode selects the execution path.
	// +kubebuilder:default=sync
	// +optional
	Mode ToolCallMode `json:"mode,omitempty"`

	// IdleTimeout (interactive mode only) ends the session after this much
	// wall-clock with no activity — no tool output and no human input. The
	// runner-side bridge enforces it. Zero means a system default applies.
	// +optional
	IdleTimeout metav1.Duration `json:"idleTimeout,omitempty"`

	// MaxDuration (interactive mode only) is a hard wall-clock cap on the
	// whole session, enforced by the controller's exec deadline. Zero means
	// a system safety ceiling applies.
	// +optional
	MaxDuration metav1.Duration `json:"maxDuration,omitempty"`

	// StreamTokenHash is the hex-encoded SHA-256 of the gateway stream token.
	// The runner sets this at ToolCall creation and keeps the token preimage
	// in memory; the raw token never touches the API server. Required for
	// streaming ToolCalls (mode=stream or mode=interactive).
	// +optional
	StreamTokenHash string `json:"streamTokenHash,omitempty"`

	// PreDispatchSnapshot, when non-nil, requests the operator's
	// toolcall controller snapshot the bundle session's workspace
	// PVC *before* dispatching the tool. Set by the runner when the
	// tool's stateImpact is readwrite or external — enables fork-
	// from-this-turn semantics. The controller writes the resulting
	// tool_dispatch_snapshot memory entry after the snapshot Job
	// completes.
	// +optional
	PreDispatchSnapshot *PreDispatchSnapshot `json:"preDispatchSnapshot,omitempty"`
}

type InputArtifact struct {
	// Path (relative) inside /work/in/<callId>/ where this artifact is materialized.
	Path string `json:"path"`
	// ArtifactRef is an opaque storage reference resolved by the operator's ArtifactStore.
	ArtifactRef string `json:"artifactRef"`
}

type ToolCallStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// StartedAt is when the tool process began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// FinishedAt is when the tool process exited or was killed.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// ExitCode is the process exit status; nil when it never ran or is running.
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`
	// StdoutArtifactRef points at the captured stdout in the artifact store.
	// +optional
	StdoutArtifactRef string `json:"stdoutArtifactRef,omitempty"`
	// StderrArtifactRef points at the captured stderr in the artifact store.
	// +optional
	StderrArtifactRef string `json:"stderrArtifactRef,omitempty"`
	// StdoutTruncated means the capture hit its size cap and is incomplete.
	// +optional
	StdoutTruncated bool `json:"stdoutTruncated,omitempty"`
	// StderrTruncated means the capture hit its size cap and is incomplete.
	// +optional
	StderrTruncated bool `json:"stderrTruncated,omitempty"`
	// OutputArtifacts are the files harvested per spec.captureOutputs.
	// +optional
	OutputArtifacts []OutputArtifact `json:"outputArtifacts,omitempty"`
	// Streaming is where a stream/interactive call's live output can be read.
	// +optional
	Streaming *StreamingEndpoint `json:"streaming,omitempty"`

	// Toolspec carries the toolspec validation result, populated on every
	// reconcile that ran the validator — which is whenever the session's class
	// declares any toolspecs.
	// +optional
	Toolspec *ToolspecValidation `json:"toolspec,omitempty"`

	// Agent reports the AgentIdentity binding that supplied env to this call.
	// nil when no agent was bound.
	// +optional
	Agent *AgentInjection `json:"agent,omitempty"`

	// Conditions carries Validated, Running, Succeeded, Failed, Timeout and
	// Canceled.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

type ToolspecValidation struct {
	// AcceptedBy is the name of the SpiceboxToolspec that returned Allow=true.
	// Mutually exclusive with Failures.
	// +optional
	AcceptedBy string `json:"acceptedBy,omitempty"`
	// Failures lists every candidate toolspec that denied this call. Empty
	// when AcceptedBy is set; populated when no candidate allowed.
	// +optional
	Failures []ToolspecFailure `json:"failures,omitempty"`
}

type ToolspecFailure struct {
	// SpecName is the SpiceboxToolspec that denied the call.
	SpecName string `json:"specName"`
	// Reason is the denial explanation, safe to show the agent.
	Reason string `json:"reason,omitempty"`
	// FailedOn points at the specific rule that denied; nil when the spec
	// denied without one.
	FailedOn *ToolspecFailedOnRef `json:"failedOn,omitempty"`
}

type ToolspecFailedOnRef struct {
	// Path locates the failing rule inside the toolspec.
	Path string `json:"path"`
	// Message is that rule's own denial text.
	Message string `json:"message,omitempty"`
}

type OutputArtifact struct {
	// Path is the in-pod path this artifact was harvested from.
	Path string `json:"path"`
	// ArtifactRef is the opaque storage reference the bytes landed under.
	ArtifactRef string `json:"artifactRef"`
	// Size is the stored artifact's size in bytes.
	Size int64 `json:"size"`
}

type StreamingEndpoint struct {
	// Available is false until the gateway is ready to serve this call's stream.
	Available bool `json:"available"`
	// GatewayEndpoint is the URL the runner connects to for live output.
	GatewayEndpoint string `json:"gatewayEndpoint,omitempty"`
}

type AgentInjection struct {
	// Injected lists the env keys that were materialized from credentials,
	// each with a masked sample of the value. The masked field is
	// defence-in-depth for casual inspection — anyone with `get` on this
	// ToolCall already has access to the same namespace's Secrets.
	Injected []InjectedEnvKey `json:"injected"`
}

type InjectedEnvKey struct {
	// Key is the environment variable name the credential filled.
	Key string `json:"key"`
	// Masked is a redacted display form of the injected value: a recognised
	// delimiter-terminated vendor prefix (e.g. ghp_) plus **** plus the last 4
	// characters, or **** alone for short or non-printable values. See pkg/x/credmask.
	Masked string `json:"masked"`
}

// PreDispatchSnapshot is the request the runner stamps onto a ToolCall
// whose tool has stateImpact ∈ {readwrite, external}. The toolcall
// controller honors it by running Snapshotter.Snapshot against the
// bundle session's workspace PVC at this handle, then dispatching the
// tool as usual. If the snapshot Job fails, the ToolCall's status
// captures the failure and the AgentSession's
// WorkspaceSnapshotFailed condition is set.
type PreDispatchSnapshot struct {
	// SessionUID is the parent AgentSession's UID. Snapshot handles
	// are addressed under this UID.
	SessionUID string `json:"sessionUID"`

	// TurnIndex is the runner's current turn.
	TurnIndex int32 `json:"turnIndex"`

	// Sequence is the within-turn ordering: 0 for the first stateful
	// dispatch in the turn, 1 for the second, etc.
	Sequence int32 `json:"sequence"`
}

func init() {
	SchemeBuilder.Register(&ToolCall{}, &ToolCallList{})
}
