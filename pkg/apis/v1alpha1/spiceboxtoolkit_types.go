package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories={authzed,spicebox},shortName=sbxtk
// +kubebuilder:subresource:status
// +genclient
// +genclient:nonNamespaced
//
// SpiceboxToolkit is the machine-readable description of one CLI a sandbox
// tool can be built from: its subcommands, flags, env, parser config and
// default per-tool authz policy. A SpiceboxToolspec narrows a toolkit down to
// the specific tool an agent is actually handed.
//
// Cluster-scoped. Reconciled by pkg/controllers/spiceboxtoolkit, which detects
// collisions with the built-in toolkits and stamps a cli-kind pin baseline
// onto status.
type SpiceboxToolkit struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SpiceboxToolkitSpec   `json:"spec,omitempty"`
	Status SpiceboxToolkitStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SpiceboxToolkitList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpiceboxToolkit `json:"items"`
}

// SpiceboxToolkitSpec mirrors pkg/tools/toolspec/toolkit.Toolkit.
//
// The shape is re-declared rather than embedded so the CRD carries explicit
// kubebuilder annotations and controller-gen emits a stable DeepCopy.
// Conversion uses a JSON round-trip (ToToolkit), so this field set must stay
// byte-compatible with toolkit.Toolkit's JSON shape.
type SpiceboxToolkitSpec struct {
	// Name is the toolkit identifier a SpiceboxToolspec references.
	Name string `json:"name"`
	// Version is the upstream CLI version this description was written against.
	Version string `json:"version,omitempty"`
	// ToolkitRevision is the description's own revision, bumped when the
	// declared surface changes.
	ToolkitRevision string `json:"toolkitRevision"`
	// Target identifies the binary this toolkit drives.
	Target ToolkitTarget `json:"target"`
	// Docs is prose about the CLI, surfaced to authors.
	Docs string `json:"docs,omitempty"`
	// Parser selects how an invocation's argv is validated and bound.
	Parser ToolkitParserConfig `json:"parser"`
	// Env describes the environment variables the binary reads. It documents
	// and wires them — notably which are sensitive — and is not a filter on the
	// environment the binary actually receives. Mirrors
	// pkg/tools/toolspec/toolkit.Toolkit.Env.
	Env ToolkitEnv `json:"env"`

	// EnvDefaults is static, non-secret environment this toolkit sets on every
	// invocation of its binary, so a CLI's own hardening travels with the
	// description of that CLI. It is a floor: the runner stamps it onto each
	// ToolCall's spec.env, and the toolcall controller demotes any key still
	// carrying this declared value below SpiceboxSession.spec.defaultEnv and
	// SpiceboxClass.spec.envDefaults, so an operator setting the same key keeps
	// winning.
	//
	// Values are never secret. A key that Env.Allowed marks sensitive is
	// rejected: those come from an AgentIdentity credential, and a static
	// default for the same key would either fail the call
	// (ToolCallEnvShadowsAgent) or replace the credential with a fixed string.
	// Mirrors pkg/tools/toolspec/toolkit.Toolkit.EnvDefaults.
	// +optional
	EnvDefaults map[string]string `json:"envDefaults,omitempty"`

	// GlobalFlags are flags accepted before any subcommand.
	GlobalFlags []ToolkitFlag `json:"globalFlags,omitempty"`
	// Subcommands are the invocations this toolkit describes; anything absent
	// here is not expressible as a tool.
	Subcommands []ToolkitSubcommand `json:"subcommands"`

	// Permission is the toolkit-wide default per-tool authz policy.
	// Subcommands may override individually via their own Permission
	// block. Mirrors pkg/tools/toolspec/toolkit.Toolkit.Permission.
	// +optional
	Permission *authz.Permission `json:"permission,omitempty"`

	// StreamFormat names a registered parser in pkg/tools/toolkitstream/registry.
	// When non-empty AND a subcommand has Mode stream or interactive, the
	// sandbox tool routes the bridge's stdout through the named parser before
	// publishing KindToolSessionEvent envelopes. Empty (default) preserves
	// the raw KindToolSessionDelta path. Mirrors
	// pkg/tools/toolspec/toolkit.Toolkit.StreamFormat. Example: "claude-stream-json".
	// +optional
	StreamFormat string `json:"streamFormat,omitempty"`

	// SiteURL is the toolkit-backed service's user-facing homepage.
	// identityd discovers a favicon from this URL for the credential
	// rows backed by this toolkit's sensitive env vars (e.g., a
	// github-token row resolves through the github toolkit). Optional;
	// unset renders the deterministic generated fallback icon.
	//
	// Must be http or https; admission rejects other schemes.
	// +optional
	SiteURL string `json:"siteURL,omitempty"`

	// SpiceDBSchema declares the SpiceDB resource types this toolkit's
	// permission checks name, so the toolkit ships the definitions its own
	// checks depend on. Mirrors pkg/toolspec/toolkit.Toolkit.SpiceDBSchema; the
	// guardian composes it alongside MCPServer and SpiceDBBootstrap fragments.
	// +optional
	SpiceDBSchema *SpiceDBSchemaFragment `json:"spicedbSchema,omitempty"`
}

type ToolkitTarget struct {
	// Binary is the executable name as invoked inside the sandbox.
	Binary string `json:"binary"`
	// VersionRange is the semver range the description is valid for; empty
	// accepts any version.
	VersionRange string `json:"versionRange,omitempty"`
	// VersionProbe is how to ask the binary its version; nil skips the check.
	VersionProbe *ToolkitVersionProbe `json:"versionProbe,omitempty"`

	// PinnedBinaryHash, when set, asserts the sha256 of the toolkit binary
	// ("sha256:…"). RECORDED, not verified: in-sandbox measurement needs a
	// sandbox exec primitive that does not exist yet.
	// +optional
	PinnedBinaryHash string `json:"pinnedBinaryHash,omitempty"`
}

type ToolkitVersionProbe struct {
	// Args is the argv appended to the binary to make it print its version.
	Args []string `json:"args"`
	// Extract is how to pull the version string out of that output.
	Extract ToolkitProbeExtract `json:"extract"`
}

type ToolkitProbeExtract struct {
	// Kind selects the extraction strategy applied to the probe's output.
	// +kubebuilder:validation:Enum=regex;json;line1
	Kind string `json:"kind"`
	// Pattern is the regex or JSON path Kind applies; unused for line1.
	Pattern string `json:"pattern,omitempty"`
}

type ToolkitParserConfig struct {
	// Kind selects the argv parser: declarative drives off this spec, builtin
	// names a compiled-in parser.
	// +kubebuilder:validation:Enum=declarative;builtin
	Kind string `json:"kind"`
	// Name identifies the builtin parser; unused when Kind is declarative.
	Name string `json:"name,omitempty"`
}

type ToolkitEnv struct {
	// Allowed DESCRIBES the environment variables the binary reads. It is not
	// an enforcement boundary: nothing strips a variable for being absent here.
	// The environment a tool call runs with is composed by the ToolCall
	// controller from the broker-resolved credentials, ToolCall.spec.env,
	// SpiceboxSession.spec.defaultEnv, this toolkit's envDefaults and the
	// sandbox container's own env.
	//
	// What an entry drives is keyed off Sensitive: redaction wherever the call
	// is surfaced, which credential a setup flow must obtain, and which names a
	// SpiceboxClass may not shadow through its own envDefaults. Adding a
	// variable here documents and wires it; omitting one does not keep it out
	// of the binary's environment.
	Allowed []ToolkitEnvVar `json:"allowed"`
}

type ToolkitEnvVar struct {
	// Name is the environment variable name.
	Name string `json:"name"`

	// Title is the short user-facing display name for the credential this
	// env var carries ("GitHub"). Shown as the bold header of a
	// credential-request row. Takes precedence over the provider catalog;
	// when empty the catalog (via Provider) or a humanized credential name
	// is used. Only meaningful when Sensitive is true.
	// +optional
	Title string `json:"title,omitempty"`

	// Description is the user-facing explanation of what this variable carries.
	Description string `json:"description,omitempty"`
	// Sensitive marks the value as credential material: it is resolved from an
	// identity, masked in output, and never logged.
	Sensitive bool `json:"sensitive,omitempty"`

	// Provider names a provider in the /providers/ library that satisfies
	// this env's auth needs. Used by `oap agent setup-identity` to dispatch
	// the right setup flow. Optional.
	// +optional
	Provider string `json:"provider,omitempty"`

	// Prompt is free-text the LLM-fallback setup agent uses as system
	// context when no provider matches. Optional.
	// +optional
	Prompt string `json:"prompt,omitempty"`

	// Credential is the name of the credential (in the agent's identity
	// catalog) that fills this env var. When empty, resolution falls back
	// to a name derived from the env var (lowercased, "_"→"-"). Only
	// meaningful when Sensitive is true.
	// +optional
	Credential string `json:"credential,omitempty"`
}

type ToolkitSubcommand struct {
	// Path is the subcommand words after the binary, e.g. ["remote","add"].
	Path []string `json:"path"`
	// Description is the user-facing explanation of what the subcommand does.
	Description string `json:"description,omitempty"`
	// Positional declares the subcommand's positional arguments, in order.
	Positional []ToolkitPositional `json:"positional,omitempty"`
	// Flags are the flags this subcommand accepts beyond the global ones.
	Flags []ToolkitFlag `json:"flags,omitempty"`
	// Effects is the declared effect profile driving approval and authz gating.
	Effects ToolkitEffects `json:"effects"`

	// Permission overrides the toolkit-level default permission for
	// this subcommand. Required by the AgentClass validator's
	// "enforcing" mode when Effects.Destructive or Effects.Writes
	// flag the subcommand as state-mutating. Mirrors
	// pkg/tools/toolspec/toolkit.Subcommand.Permission.
	// +optional
	Permission *authz.Permission `json:"permission,omitempty"`

	// PermissionVariants branch this subcommand's authority on its ARGUMENTS:
	// CEL `When` over the parsed args, first match wins, Permission is the
	// fallback. Mirrors pkg/toolspec/toolkit.Subcommand.PermissionVariants and
	// matches MCPServerTool.PermissionVariants exactly — the same idea should
	// not grow a second vocabulary.
	// +optional
	PermissionVariants []authz.PermissionVariant `json:"permissionVariants,omitempty"`

	// Mode declares how the runner dispatches this subcommand. Mirrors
	// pkg/tools/toolspec/toolkit.Subcommand.Mode. Values: "" (sync),
	// "stream" (streaming output, no stdin bridge), "interactive"
	// (streaming output + live stdin pipe).
	// +optional
	Mode string `json:"mode,omitempty"`

	// Timeout overrides the mode-derived per-call execution budget for this
	// subcommand (a Go duration string, e.g. "10m" for a slow clone). Empty
	// applies the mode default: 30m for stream/interactive, 5m for sync.
	// Mirrors pkg/tools/toolspec/toolkit.Subcommand.Timeout; a non-parseable or
	// non-positive value is rejected when the toolkit is loaded.
	// +optional
	Timeout string `json:"timeout,omitempty"`
}

type ToolkitPositional struct {
	// Name is the argument name the tool schema exposes to the agent.
	Name string `json:"name"`
	// Type is the value type used to validate and render the argument.
	Type string `json:"type"`
	// Required rejects an invocation that omits this argument.
	Required bool `json:"required,omitempty"`
	// Values, when non-empty, restricts the argument to this enumeration.
	Values []string `json:"values,omitempty"`
	// SplitOn, when non-empty, splits one supplied value into several argv
	// entries on this separator.
	SplitOn string `json:"splitOn,omitempty"`

	// AfterDashDash marks this slot as where post-`--` arguments start
	// binding, for a CLI that overloads `--` as a separator rather than a
	// plain option terminator (git's `log [<rev>] [-- <path>…]`). Mirrors
	// pkg/tools/toolspec/toolkit.Positional.AfterDashDash; at most one positional
	// per subcommand may set it.
	// +optional
	AfterDashDash bool `json:"afterDashDash,omitempty"`
}

type ToolkitFlag struct {
	// Long is the flag's long form, without leading dashes.
	Long string `json:"long"`
	// Short is the single-letter form, without its dash; empty if none.
	Short string `json:"short,omitempty"`
	// Type is the value type; "bool" means the flag takes no value.
	Type string `json:"type"`
	// Description is the user-facing explanation of the flag.
	Description string `json:"description,omitempty"`
	// Sensitive keeps the flag's value out of logs and approval prompts.
	Sensitive bool `json:"sensitive,omitempty"`
	// Values, when non-empty, restricts the flag to this enumeration.
	Values []string `json:"values,omitempty"`
	// SplitOn, when non-empty, splits one supplied value into repeated flag
	// occurrences on this separator.
	SplitOn string `json:"splitOn,omitempty"`

	// OptionalValue declares that the binary accepts this flag's value only in
	// the attached `--flag=value` form (git's PARSE_OPT_OPTARG, pflag's
	// NoOptDefVal), so the parser must not consume the following token as the
	// value — the binary won't. Mirrors
	// pkg/tools/toolspec/toolkit.Flag.OptionalValue; invalid on type "bool".
	// +optional
	OptionalValue bool `json:"optionalValue,omitempty"`
}

type ToolkitEffects struct {
	// Destructive means the invocation can remove or overwrite state.
	Destructive bool `json:"destructive"`
	// Reads names the resource kinds the invocation reads.
	Reads []string `json:"reads"`
	// Writes names the resource kinds the invocation mutates.
	Writes []string `json:"writes"`
	// Network is where the invocation may reach.
	Network ToolkitNetworkEffect `json:"network"`
	// Filesystem is what the invocation may touch on disk.
	Filesystem ToolkitFsEffect `json:"filesystem"`
	// Creds is which credentials the invocation needs or rewrites.
	Creds ToolkitCredsEffect `json:"creds"`
}

type ToolkitNetworkEffect struct {
	// Destinations names the hosts the invocation contacts; empty means none.
	Destinations []string `json:"destinations"`
}

type ToolkitFsEffect struct {
	// Paths names the filesystem locations the invocation touches.
	Paths []string `json:"paths"`
}

type ToolkitCredsEffect struct {
	// Required names credentials the invocation needs to succeed.
	Required []string `json:"required"`
	// Writes names credentials the invocation may overwrite.
	Writes []string `json:"writes"`
}

type SpiceboxToolkitStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Pin is the recorded pin baseline in the common PinRecord shape:
	// Digest = pinnedBinaryHash assertion (if set), Version = versionRange
	// (audit metadata). Strength reflects the declared ref: frozen iff
	// PinnedBinaryHash is set, named iff VersionRange is set, else unpinned.
	// +optional
	Pin *PinRecord `json:"pin,omitempty"`

	// Conditions carries Valid; see SpiceboxToolkitConditionValid.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// SpiceboxToolkit condition types.
const (
	SpiceboxToolkitConditionValid = "Valid"
	// SpiceboxToolkitConditionSpiceDBSchemaValid is guardian-owned, NOT owned by
	// the spiceboxtoolkit controller that owns Valid above. False means this
	// toolkit's spec.spicedbSchema fragment was excluded from the cluster-wide
	// schema compose, either as FragmentInvalid (bad on its own — a RawZed
	// syntax error, a reserved scaffold definition redeclared, an internal
	// conflict) or FragmentConflict (valid alone, but colliding with an
	// already-accepted fragment; earlier namespace/name wins and the later is
	// marked here).
	//
	// A SpiceboxToolkit CR is installed, so it is tenant input like MCPServer
	// and SidecarToolbox — the //go:embed'ed built-in toolkits are a different,
	// compile-time set and are not gated by this condition. Isolating the
	// failure here rather than freezing the compose pass is what stops one
	// tenant's bad or hostile toolkit from taking down SchemaIncluded for every
	// AgentSessionGrants in the cluster.
	SpiceboxToolkitConditionSpiceDBSchemaValid = "SpiceDBSchemaValid"
)

// SpiceboxToolkitReasonFragmentInvalid is the SpiceDBSchemaValid=False reason
// when spec.spicedbSchema fails per-fragment validation ON ITS OWN — a RawZed
// syntax error, a redeclared reserved scaffold definition, or an internal
// conflict.
const SpiceboxToolkitReasonFragmentInvalid = "FragmentInvalid"

// SpiceboxToolkitReasonFragmentConflict is the SpiceDBSchemaValid=False reason
// when this fragment is valid alone but collides with an already-accepted one.
// First in order wins, so the toolkit carrying this reason is the later of the
// two.
const SpiceboxToolkitReasonFragmentConflict = "FragmentConflict"

// SpiceboxToolkitReasonFragmentValid is the SpiceDBSchemaValid=True reason,
// stamped only to clear a PREVIOUSLY-invalid fragment that has since been
// fixed. A fragment that has always been valid never carries this condition.
const SpiceboxToolkitReasonFragmentValid = "FragmentValid"

// ToToolkit converts the CR spec to the toolspec library type via JSON round-trip.
// The CR spec field set is byte-compatible with toolkit.Toolkit's JSON encoding.
func (s *SpiceboxToolkitSpec) ToToolkit() (*toolkit.Toolkit, error) {
	data, err := jsonMarshal(s)
	if err != nil {
		return nil, err
	}
	return toolkit.LoadBytes(data)
}

func init() {
	SchemeBuilder.Register(&SpiceboxToolkit{}, &SpiceboxToolkitList{})
}
