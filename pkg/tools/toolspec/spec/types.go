package spec

// Spec is a capability contract authored against a specific toolkit revision.
type Spec struct {
	// Name identifies this spec; it becomes the agent-visible tool name.
	Name string `json:"name"`
	// Version is the schema version of this document.
	Version string `json:"version"`
	// Intent is the human sentence describing what this spec is for.
	Intent string `json:"intent,omitempty"`
	// Toolkit names the toolkit and revision this spec was authored against.
	Toolkit ToolkitRef `json:"toolkit"`
	// Require holds preconditions that must hold before any call is allowed.
	Require Require `json:"require,omitempty"`
	// AllowSubcommands is the closed set of subcommand paths (space-joined)
	// this spec permits. Empty allows nothing.
	AllowSubcommands []string `json:"allowSubcommands"`
	// Deny rules run before Allow and short-circuit to a denial.
	Deny Deny `json:"deny,omitempty"`
	// Allow bounds what an otherwise-permitted call may reach. An unset stanza
	// imposes no bound; see each field's Set flag.
	Allow Allow `json:"allow,omitempty"`
	// Exceptions relax named Deny rules when their CEL guard holds.
	Exceptions []Exception `json:"exceptions,omitempty"`
	// Constraints are CEL predicates every allowed call must satisfy.
	Constraints []Constraint `json:"constraints,omitempty"`
	// Sensitive names the inputs whose values must be redacted from every
	// user-visible output.
	Sensitive Sensitive `json:"sensitive,omitempty"`
	// Generation is authoring provenance; nil for a hand-written spec.
	Generation *Generation `json:"generation,omitempty"`
	// SecretOutput, when non-nil, declares that this tool produces a single
	// secret value (e.g. a kubeconfig or token) that the runner must capture
	// out-of-band. The LLM only sees an opaque handle; the raw value is stored
	// in the per-session SecretOut store until explicitly consumed.
	SecretOutput *SecretOutputSpec `json:"secretOutput,omitempty"`
	// WritesRelationships are JIT SpiceDB relationship-write blocks the
	// runner evaluates after a SUCCESSFUL tool call. Mirrors
	// v1alpha1.SpiceboxToolspecSpec.WritesRelationships; json tags must
	// match exactly so the ToSpec JSON round-trip carries the value through.
	WritesRelationships []RelationshipWriteSpec `json:"writesRelationships,omitempty"`
	// Observes declares facts this tool's result asserts about specific
	// resource instances, co-derived with the subjects they are about. Mirrors
	// v1alpha1.SpiceboxToolspecSpec.Observes; json tags must match exactly so
	// the ToSpec JSON round-trip carries the value through.
	Observes []ObservesSpec `json:"observes,omitempty"`
}

type ToolkitRef struct {
	// Name is the toolkit's catalog key.
	Name string `json:"name"`
	// Revision must equal the toolkit's ToolkitRevision, or every call is
	// denied rather than validated against a description that has since moved.
	Revision string `json:"revision"`
}

type Require struct {
	// VerifiedBinaryVersion denies a call whose binary version could not be
	// probed and matched against the toolkit's range.
	VerifiedBinaryVersion bool `json:"verifiedBinaryVersion,omitempty"`
}

type Deny struct {
	// Effects denies a subcommand by what the toolkit says it does.
	Effects DenyEffects `json:"effects,omitempty"`
}

type DenyEffects struct {
	// Destructive denies every subcommand the toolkit marks destructive.
	Destructive bool `json:"destructive,omitempty"`
	// Reads denies subcommands reading any of these resource classes.
	Reads []string `json:"reads,omitempty"`
	// Writes denies subcommands mutating any of these resource classes.
	Writes []string `json:"writes,omitempty"`
	// Creds denies subcommands by their credential effects.
	Creds DenyCreds `json:"creds,omitempty"`
}

type DenyCreds struct {
	// Writes denies every subcommand that mints or overwrites a credential.
	Writes bool `json:"writes,omitempty"`
}

type Allow struct {
	// Network bounds the destinations a call may reach.
	Network AllowNetwork `json:"network,omitempty"`
	// Filesystem bounds the paths a call may touch.
	Filesystem AllowFilesystem `json:"filesystem,omitempty"`
	// Creds bounds the credentials a call may require.
	Creds AllowCreds `json:"creds,omitempty"`
}

type AllowNetwork struct {
	// Destinations is the set of allowed hostnames. The call's resolved
	// network destinations must be a subset of this list. Exact match only.
	Destinations []string `json:"destinations,omitempty"`
	// Set records that the stanza was PRESENT in the source document, so an
	// explicit empty list (allow nothing) is distinguishable from omission
	// (impose no bound). Never serialized; derived at load.
	Set bool `json:"-"`
}

type AllowFilesystem struct {
	// PathsUnder are directory prefixes a call's filesystem effects must fall
	// under.
	PathsUnder []string `json:"pathsUnder,omitempty"`
	// Set records that the stanza was PRESENT in the source document — see
	// AllowNetwork.Set.
	Set bool `json:"-"`
}

type AllowCreds struct {
	// Required is the set of credentials a call is permitted to need.
	Required []string `json:"required,omitempty"`
	// Set records that the stanza was PRESENT in the source document — see
	// AllowNetwork.Set.
	Set bool `json:"-"`
}

type Exception struct {
	// Overrides names the deny rules this exception relaxes.
	Overrides []string `json:"overrides"`
	// When is the CEL guard that must hold for the override to apply.
	When string `json:"when"`
	// Message explains the exception in a decision trace.
	Message string `json:"message,omitempty"`
}

type Constraint struct {
	// CEL is a boolean expression over the parsed call; false denies.
	CEL string `json:"cel"`
	// Message is the user-facing reason shown when this constraint denies.
	Message string `json:"message,omitempty"`
}

type Sensitive struct {
	// Flags names flags (by their long form) whose values must be redacted.
	Flags []string `json:"flags,omitempty"`
	// Env names environment variables whose values must be redacted.
	Env []string `json:"env,omitempty"`
	// Positional names positional slots whose values must be redacted.
	Positional []string `json:"positional,omitempty"`
}

// SecretOutputSpec declares that a tool's stdout (or a specific output file) is
// a secret value that must be diverted out-of-band by the runner — the LLM
// never sees the raw value, only an opaque handle.
//
// Source ∈ {"stdout", "file:<absolute-path>"}; load.go rejects anything else.
type SecretOutputSpec struct {
	// Name is the logical key for this secret (e.g. "kubeconfig"). Shown in the
	// handle line emitted to the LLM; also used as the per-session store key.
	Name string `json:"name"`
	// Source declares where the secret value lives in the tool's output:
	// "stdout" means the entire stdout stream is the value;
	// "file:<absolute-path>" means the file the tool wrote at that path is.
	Source string `json:"source"`
	// Description is human/user-visible text about the secret (caveats, usage,
	// expiry). MUST NOT contain the value itself.
	Description string `json:"description,omitempty"`
}

// RelationshipWriteSpec mirrors v1alpha1.MCPServerRelationshipWrite.
// JSON tags must match exactly: the CRD ToSpec conversion is a JSON
// round-trip.
type RelationshipWriteSpec struct {
	// When is a CEL boolean expression with `args`, `result` (and, on the
	// sandbox path, `session`) in scope; the block is skipped if it
	// evaluates false. When unset, the block is always taken.
	When string `json:"when,omitempty"`
	// ForEach is a CEL expression that must evaluate to a list. One tuple
	// is emitted per element, with `item` bound to that element. When
	// unset, exactly one tuple is emitted.
	ForEach string `json:"forEach,omitempty"`
	// Tuple holds the CEL expressions that compose the SpiceDB
	// relationship tuple.
	Tuple RelationshipTupleSpec `json:"tuple"`
	// Exclusive makes this write atomically write-once per subject: the write
	// FAILS (no tuple written) if the subject already holds `relation` on ANY
	// resource of the tuple's resource type (SpiceDB MUST_NOT_MATCH
	// precondition). Used for session-pin semantics. Mirrors
	// v1alpha1.MCPServerRelationshipWrite.Exclusive; the json tag must match
	// byte-for-byte so the CRD ToSpec round-trip carries it. Default false.
	Exclusive bool `json:"exclusive,omitempty"`
	// RequireSlotBound refuses every tuple this block emits unless the calling
	// session holds a slot grant on the tuple's RESOURCE. Mirrors
	// v1alpha1.MCPServerRelationshipWrite.RequireSlotBound — see that field for
	// what the gate is for; the json tag must match byte-for-byte, because this
	// struct is the sandbox dispatcher's ONLY view of the block and a dropped
	// tag would silently ungate it. Default false.
	RequireSlotBound bool `json:"requireSlotBound,omitempty"`
}

// RelationshipTupleSpec mirrors v1alpha1.MCPServerRelationshipTuple — the
// (resource, relation, subject) triple expressed as CEL string expressions.
type RelationshipTupleSpec struct {
	// Resource is a CEL string expression that must evaluate to a SpiceDB
	// object reference of the form "<type>:<id>".
	Resource string `json:"resource"`
	// Relation is a CEL string expression that must evaluate to a
	// relation name declared on the Resource's type.
	Relation string `json:"relation"`
	// Subject is a CEL string expression that must evaluate to a
	// SpiceDB object reference of the form "<type>:<id>".
	Subject string `json:"subject"`
}

// ObservesSpec mirrors v1alpha1.ObservesBlock — one observation declaration,
// yielding subjects and facts from the same `item`. JSON tags must match
// exactly: the CRD ToSpec conversion is a JSON round-trip.
type ObservesSpec struct {
	// When is a CEL boolean expression with `args`, `result` (and, on the
	// sandbox path, `session`) in scope; the block is skipped if it evaluates
	// false. When unset, the block is always taken.
	When string `json:"when,omitempty"`
	// ForEach is a CEL expression that must evaluate to a list. One
	// observation is emitted per element, with `item` bound to that element.
	// When unset, exactly one observation is emitted with item=nil.
	ForEach string `json:"forEach,omitempty"`
	// Subjects are the objects this observation is about, as CEL string
	// expression pairs. At least one is required.
	Subjects []ObserveSubjectSpec `json:"subjects"`
	// Facts maps a fact name to a CEL expression yielding its value,
	// evaluated against the same `item` as Subjects.
	Facts map[string]string `json:"facts"`
}

// ObserveSubjectSpec mirrors v1alpha1.ObserveSubject — a (resourceType,
// resourceID) pair of CEL string expressions.
type ObserveSubjectSpec struct {
	// ResourceType is a CEL string expression yielding a SpiceDB definition
	// name (commonly a literal, e.g. `"github_pr"`).
	ResourceType string `json:"resourceType"`
	// ResourceID is a CEL string expression yielding the object id.
	ResourceID string `json:"resourceID"`
}

// Generation is optional LLM-authored metadata about how a spec was produced.
// The validator ignores this field entirely; only the renderer and tooling read it.
type Generation struct {
	// Source identifies what authored the spec, e.g. "llm:<model-id>".
	Source string `json:"source"`
	// GeneratedAt is the RFC 3339 authoring timestamp.
	GeneratedAt string `json:"generatedAt,omitempty"`
	// Warnings are caveats the author emitted about the result.
	Warnings []string `json:"warnings,omitempty"`
	// Unmatched are parts of the stated intent the spec does not cover.
	Unmatched []Unmatched `json:"unmatched,omitempty"`
	// Excluded are subcommands deliberately left out of AllowSubcommands.
	Excluded []Excluded `json:"excluded,omitempty"`
	// Descriptions maps a subcommand path to prose the renderer shows for it.
	Descriptions map[string]string `json:"descriptions,omitempty"`
	// TestCases are the invocations the convergence loop checks the spec
	// against.
	TestCases []TestCase `json:"testCases,omitempty"`
}

type Unmatched struct {
	// Request is the part of the intent that went unserved.
	Request string `json:"request"`
	// Reason says why the spec does not cover it.
	Reason string `json:"reason"`
}

type Excluded struct {
	// Name is the subcommand path that was left out.
	Name string `json:"name"`
	// Reason says why it was excluded.
	Reason string `json:"reason"`
}

type TestCase struct {
	// Intent is the human sentence this invocation is meant to represent.
	Intent string `json:"intent"`
	// Argv is the full invocation, binary included.
	Argv []string `json:"argv"`
	// Env is the environment the case is validated under.
	Env map[string]string `json:"env,omitempty"`
	// Cwd is the working directory the case is validated under.
	Cwd string `json:"cwd,omitempty"`
	// BinaryVersion is the version the case pretends the binary reported;
	// empty means unverified.
	BinaryVersion string `json:"binaryVersion,omitempty"`
	// ExpectAllow is the outcome the author intends for this invocation.
	ExpectAllow bool `json:"expectAllow"`
	// LastRunActual, when non-nil, is the actual Decision.Allow from the most
	// recent convergence-loop run. When LastRunActual != ExpectAllow the test
	// failed to converge — the renderer surfaces these.
	LastRunActual *bool `json:"lastRunActual,omitempty"`
	// LastRunReason is the validator's short reason string for the mismatch.
	// Only populated when LastRunActual != ExpectAllow.
	LastRunReason string `json:"lastRunReason,omitempty"`
}
