package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories={authzed,spicebox},shortName=sbxtsp
// +kubebuilder:subresource:status
// +genclient
// +genclient:nonNamespaced
//
// SpiceboxToolspec is one sandbox tool as the agent sees it: a SpiceboxToolkit
// reference narrowed to an allowed subcommand set, with CEL argument
// constraints, denied effects and an authz policy.
//
// Cluster-scoped. Reconciled by pkg/controllers/spiceboxtoolspec, which
// resolves the referenced toolkit, compiles every CEL expression, and surfaces
// Valid=True/False. A Valid=False toolspec is skipped at ToolCall validation
// time rather than executed unchecked.
type SpiceboxToolspec struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SpiceboxToolspecSpec   `json:"spec,omitempty"`
	Status SpiceboxToolspecStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SpiceboxToolspecList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpiceboxToolspec `json:"items"`
}

// SpiceboxToolspecSpec mirrors pkg/tools/toolspec/spec.Spec.
type SpiceboxToolspecSpec struct {
	// Name is the LLM-facing tool name this spec produces.
	Name string `json:"name,omitempty"`
	// Version is the spec author's version of this declaration.
	Version string `json:"version,omitempty"`
	// Intent is the one-line purpose shown to authors and reviewers.
	Intent string `json:"intent,omitempty"`
	// Toolkit is the SpiceboxToolkit this spec narrows.
	Toolkit ToolspecToolkitRef `json:"toolkit"`
	// Require declares preconditions the sandbox must satisfy.
	Require ToolspecRequire `json:"require,omitempty"`
	// AllowSubcommands is the allowlist of toolkit subcommand paths; anything
	// absent is denied.
	AllowSubcommands []string `json:"allowSubcommands"`
	// Deny refuses calls by declared effect even inside AllowSubcommands.
	Deny ToolspecDeny `json:"deny,omitempty"`
	// Allow widens what a call may reach beyond the defaults.
	Allow ToolspecAllow `json:"allow,omitempty"`
	// Exceptions conditionally relax named Deny rules.
	Exceptions []ToolspecException `json:"exceptions,omitempty"`
	// Constraints are CEL predicates every call's arguments must satisfy.
	Constraints []ToolspecConstraint `json:"constraints,omitempty"`
	// Sensitive names arguments and env to redact from logs and prompts.
	Sensitive ToolspecSensitive `json:"sensitive,omitempty"`
	// SecretOutput, when non-nil, declares that this tool produces a single
	// secret value that the runner must capture out-of-band. Mirrors
	// pkg/tools/toolspec/spec.SecretOutputSpec; json tags must match exactly so the
	// ToSpec JSON round-trip carries the value through.
	// +optional
	SecretOutput *ToolspecSecretOutput `json:"secretOutput,omitempty"`
	// WritesRelationships are JIT SpiceDB relationship-write blocks the
	// runner evaluates after a SUCCESSFUL tool call. Reuses the MCPServer
	// per-tool type and CEL semantics (when/forEach/tuple), with two extra
	// bindings on the sandbox path: `session` ("<ns>/<name>" of the
	// AgentSession) and `args.argv` (the raw argv list the agent supplied).
	// For toolspecs that also declare secretOutput, `result` carries only
	// {success: bool} — the captured secret value is never exposed to CEL.
	// +optional
	WritesRelationships []MCPServerRelationshipWrite `json:"writesRelationships,omitempty"`

	// Observes declares facts this tool's result asserts about specific
	// resource instances, co-derived with the subjects they are about.
	// Evaluated after a SUCCESSFUL call. Reuses the MCPServer per-tool type
	// and CEL semantics; see MCPServerTool.Observes.
	// +optional
	// +listType=atomic
	Observes []ObservesBlock `json:"observes,omitempty"`
}

// ToolspecSecretOutput mirrors pkg/tools/toolspec/spec.SecretOutputSpec. Json tags
// must be identical to those on spec.SecretOutputSpec so that the ToSpec
// JSON round-trip carries all fields through without explicit mapping.
type ToolspecSecretOutput struct {
	// Name is the logical key for the secret (e.g. "kubeconfig").
	Name string `json:"name"`
	// Source declares where the secret value lives: "stdout" or "file:<absolute-path>".
	Source string `json:"source"`
	// Description is human-visible text about the secret (caveats, usage, expiry).
	// +optional
	Description string `json:"description,omitempty"`
}

type ToolspecToolkitRef struct {
	// Name is the SpiceboxToolkit name (or a builtin toolkit's name).
	Name string `json:"name"`
	// Revision is the toolkitRevision this spec was authored against; a
	// mismatch is a validation failure, not a silent upgrade.
	Revision string `json:"revision"`
}

type ToolspecRequire struct {
	// VerifiedBinaryVersion refuses the call unless the sandbox's binary
	// version was probed and matched the toolkit's declared range.
	VerifiedBinaryVersion bool `json:"verifiedBinaryVersion,omitempty"`
}

type ToolspecDeny struct {
	// Effects refuses calls whose declared effects match.
	Effects ToolspecDenyEffects `json:"effects,omitempty"`
}

type ToolspecDenyEffects struct {
	// Destructive denies any subcommand the toolkit marks destructive.
	Destructive bool `json:"destructive,omitempty"`
	// Reads names resource kinds whose reads are denied.
	Reads []string `json:"reads,omitempty"`
	// Writes names resource kinds whose writes are denied.
	Writes []string `json:"writes,omitempty"`
	// Creds denies credential-touching effects.
	Creds ToolspecDenyCreds `json:"creds,omitempty"`
}

type ToolspecDenyCreds struct {
	// Writes denies any subcommand that would write credential material.
	Writes bool `json:"writes,omitempty"`
}

type ToolspecAllow struct {
	// Network widens where a call may reach.
	Network ToolspecAllowNetwork `json:"network,omitempty"`
	// Filesystem widens what a call may touch on disk.
	Filesystem ToolspecAllowFilesystem `json:"filesystem,omitempty"`
	// Creds names the credentials a call may consume.
	Creds ToolspecAllowCreds `json:"creds,omitempty"`
}

type ToolspecAllowNetwork struct {
	// Destinations are the hosts a call may contact; empty allows none.
	Destinations []string `json:"destinations,omitempty"`
}

type ToolspecAllowFilesystem struct {
	// PathsUnder are the directory roots a call may touch.
	PathsUnder []string `json:"pathsUnder,omitempty"`
}

type ToolspecAllowCreds struct {
	// Required names the credentials that must resolve for a call to run.
	Required []string `json:"required,omitempty"`
}

type ToolspecException struct {
	// Overrides names the deny rules this exception relaxes.
	Overrides []string `json:"overrides"`
	// When is the CEL predicate that must hold for the relaxation to apply.
	When string `json:"when"`
	// Message explains the exception in denial output.
	Message string `json:"message,omitempty"`
}

type ToolspecConstraint struct {
	// CEL is a boolean expression over the call's arguments; false denies.
	CEL string `json:"cel"`
	// Message is the denial text shown when CEL evaluates false.
	Message string `json:"message,omitempty"`
}

type ToolspecSensitive struct {
	// Flags names flags whose values must be redacted.
	Flags []string `json:"flags,omitempty"`
	// Env names environment variables whose values must be redacted.
	Env []string `json:"env,omitempty"`
	// Positional names positional arguments whose values must be redacted.
	Positional []string `json:"positional,omitempty"`
}

type SpiceboxToolspecStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ResolvedToolkit names the SpiceboxToolkit CR (or "<builtin>") that this
	// Toolspec's spec.toolkit reference resolved to.
	// +optional
	ResolvedToolkit string `json:"resolvedToolkit,omitempty"`
	// Conditions carries Valid; see SpiceboxToolspecConditionValid.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

const (
	SpiceboxToolspecConditionValid = "Valid"
)

// ToSpec converts the CR spec to the toolspec library type via JSON round-trip.
func (s *SpiceboxToolspecSpec) ToSpec() (*spec.Spec, error) {
	data, err := jsonMarshal(s)
	if err != nil {
		return nil, err
	}
	return spec.LoadBytes(data)
}

func init() {
	SchemeBuilder.Register(&SpiceboxToolspec{}, &SpiceboxToolspecList{})
}
