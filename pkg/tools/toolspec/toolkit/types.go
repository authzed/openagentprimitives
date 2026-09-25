package toolkit

import (
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// Subcommand.Mode values. "" means sync (request/response).
const (
	SubcommandModeStream      = "stream"
	SubcommandModeInteractive = "interactive"
)

// Toolkit is the closed-world description of one CLI tool.
type Toolkit struct {
	// Name is the catalog key a spec names to reach this toolkit.
	Name string `json:"name"`
	// Version is the schema version of this document, not the CLI's version.
	Version string `json:"version"`
	// ToolkitRevision dates this description of the CLI; a spec authored
	// against a different revision is refused rather than silently revalidated.
	ToolkitRevision string `json:"toolkitRevision"`
	// Target is the binary this toolkit wraps and how to probe its version.
	Target Target `json:"target"`
	// Docs is free-form prose about the CLI, shown to spec authors.
	Docs string `json:"docs,omitempty"`
	// Parser selects how an invocation's argv is turned into a parsed call.
	Parser ParserConfig `json:"parser"`
	// Env describes the environment variables this toolkit knows about.
	Env Env `json:"env"`
	// EnvDefaults is static, non-secret environment the toolkit sets on every
	// invocation of its binary, so a CLI's own hardening is declared next to
	// the description of that CLI rather than per bundle or per class.
	//
	// It is a FLOOR, not an override: the runner stamps it onto the ToolCall's
	// spec.env at synthesis, and the toolcall controller demotes any key still
	// carrying this declared value below SpiceboxSession.spec.defaultEnv and
	// SpiceboxClass.spec.envDefaults, so an operator who sets the same key
	// keeps winning.
	//
	// Values are never secret. A key naming a `sensitive` entry in Env.Allowed
	// is refused at load: those are filled from an AgentIdentity credential,
	// and a static default that shadowed one would fail the tool call outright
	// (ToolCallEnvShadowsAgent) or hand the CLI a fixed string in place of the
	// credential.
	EnvDefaults map[string]string `json:"envDefaults,omitempty"`
	// GlobalFlags are flags valid before any subcommand path.
	GlobalFlags []Flag `json:"globalFlags,omitempty"`
	// Subcommands is the closed set of invocations this toolkit describes;
	// anything outside it is denied, never passed through.
	Subcommands []Subcommand `json:"subcommands"`
	// Permission is the toolkit-wide default; subcommands may
	// override individually.
	Permission *authz.Permission `json:"permission,omitempty"`
	// StreamFormat names a registered parser in pkg/tools/toolkitstream/registry.
	// When non-empty AND a subcommand has Mode stream or interactive, the
	// sandbox tool routes the bridge's stdout through the named parser before
	// publishing KindToolSessionEvent envelopes. Empty publishes raw
	// KindToolSessionDelta instead. Example value: "claude-stream-json".
	StreamFormat string `json:"streamFormat,omitempty"`
	// SiteURL mirrors SpiceboxToolkit.Spec.SiteURL for embedded
	// catalog entries. Set in toolkits/<name>.yaml for built-in
	// toolkits whose homepage is known at compile time. identityd
	// resolves a favicon from it when there's no SpiceboxToolkit CR
	// overriding the embedded entry.
	SiteURL string `json:"siteURL,omitempty"`

	// SpiceDBSchema declares the SpiceDB resource types this toolkit's
	// permission checks name, so a toolkit ships the definitions its own checks
	// depend on — the same contract MCPServer.spec.spicedbSchema gives an MCP
	// server for the types ITS tools name.
	//
	// A check names a resourceType and a permission; nothing defined them. `gh
	// pr view` gates on github_repo#read, no shipped manifest declared
	// github_repo, and a cluster running a gh-tooled agent under
	// toolCalls.mode=enforcing got `object definition "github_repo" not found`
	// on every gated call — the check resolved an id and then had nowhere to
	// ask. Putting the definition next to the check that needs it is what keeps
	// the two from drifting; toolkits/schema_coverage_test.go asserts they
	// cover each other in both directions.
	//
	// Shape is byte-compatible with v1alpha1.SpiceDBSchemaFragment. It is
	// re-declared here rather than imported because pkg/apis/v1alpha1 imports
	// THIS package (SpiceboxToolkitSpec mirrors Toolkit), so the dependency
	// cannot run the other way; the guardian converts by JSON round-trip.
	SpiceDBSchema *SpiceDBSchemaFragment `json:"spicedbSchema,omitempty"`
}

// SpiceDBSchemaFragment mirrors v1alpha1.SpiceDBSchemaFragment.
type SpiceDBSchemaFragment struct {
	Resources []SpiceDBResource `json:"resources,omitempty"`

	// RawZed is appended verbatim after Resources, for SpiceDB features the
	// structured form cannot express (subject-relations, unions).
	RawZed string `json:"rawZed,omitempty"`
}

// SpiceDBResource mirrors v1alpha1.SpiceDBResource.
type SpiceDBResource struct {
	Name        string              `json:"name"`
	Relations   []SpiceDBRelation   `json:"relations,omitempty"`
	Permissions []SpiceDBPermission `json:"permissions,omitempty"`

	// Display mirrors v1alpha1.SpiceDBResourceDisplay. Declared here for the
	// SAME reason SpiceDBPermission.Title is: an embedded toolkit reaches the
	// controller by marshalling THIS struct and unmarshalling it into the CR
	// spec (embeddedToolkitAsCR), so a field missing here is silently dropped
	// on the way — the YAML declares a display, nothing errors, and every card
	// falls back to the wire type name as though none had been written.
	// +optional
	Display *SpiceDBResourceDisplay `json:"display,omitempty"`

	// Standing and ApproverPermission mirror v1alpha1.SpiceDBResource's fields
	// of the same names. Declared here for the SAME reason Display and
	// SpiceDBPermission.Title are — an embedded toolkit reaches the controller by
	// marshalling THIS struct into the CR spec (embeddedToolkitAsCR), so a field
	// missing here is dropped in transit with no error.
	//
	// This one had already been dropped for real: Standing existed only on the CR
	// type, so no builtin toolkit could ever declare it and git_repo's standing
	// came from the CRD default rather than from git.yaml. That was invisible
	// while a default existed. With the default gone it is fatal — the resource
	// would arrive with standing unset and be refused — which is exactly the
	// property that makes the missing mirror announce itself instead of
	// quietly deciding who may approve a push.
	Standing           string `json:"standing"`
	ApproverPermission string `json:"approverPermission,omitempty"`
}

// SpiceDBResourceDisplay mirrors v1alpha1.SpiceDBResourceDisplay.
type SpiceDBResourceDisplay struct {
	Name  string `json:"name,omitempty"`
	Icon  string `json:"icon,omitempty"`
	Label string `json:"label,omitempty"`
}

// SpiceDBRelation mirrors v1alpha1.SpiceDBRelation.
type SpiceDBRelation struct {
	Name        string `json:"name"`
	SubjectType string `json:"subjectType"`
	Wildcard    bool   `json:"wildcard,omitempty"`
}

// SpiceDBPermission mirrors v1alpha1.SpiceDBPermission.
type SpiceDBPermission struct {
	Name string `json:"name"`
	Expr string `json:"expr"`

	// Title is the phrase a human reads on an approval card instead of the
	// permission's handle. Mirrors v1alpha1.SpiceDBPermission.Title.
	//
	// It has to exist on BOTH types. An embedded toolkit reaches the controller
	// by marshalling THIS struct and unmarshalling it into the CR spec
	// (embeddedToolkitAsCR), so a field missing here is silently dropped on the
	// way — the YAML declares a title, nothing errors, and every card falls back
	// to the detokenized handle as though none had been written.
	Title string `json:"title,omitempty"`

	// PlanningNote is one line of guidance the agent reads while DECLARING a
	// plan, alongside the list of handles it may declare. Mirrors
	// v1alpha1.SpiceDBPermission.PlanningNote.
	//
	// It exists because the knowledge that shapes a good plan is
	// toolkit-specific and belongs with the toolkit. git_repo splits its
	// permissions across two instances — read/write key on the checked-out
	// copy, fetch/push on the remote URL — so declaring `write` without `read`
	// yields a phase that can change files it cannot open. Nothing generic
	// could know that, and a phase that under-declares interrupts the human
	// mid-task with an amendment for a permission the plan should have carried
	// from the start.
	//
	// Same both-types rule as Title, for the same reason: an embedded toolkit
	// reaches the controller by marshalling THIS struct into the CR spec, so a
	// field missing here is dropped in transit with nothing to show for it.
	PlanningNote string `json:"planningNote,omitempty"`
}

type Target struct {
	// Binary is the executable name invoked in the sandbox.
	Binary string `json:"binary"`
	// VersionRange is the semver range this description is valid for. Empty
	// means any version is accepted.
	VersionRange string `json:"versionRange,omitempty"`
	// VersionProbe says how to read the installed version. Nil means the
	// version cannot be checked, so VersionRange goes unverified.
	VersionProbe *VersionProbe `json:"versionProbe,omitempty"`
}

type VersionProbe struct {
	// Args are appended to Target.Binary to make it print its version.
	Args []string `json:"args"`
	// Extract pulls the version string out of that output.
	Extract ProbeExtract `json:"extract"`
}

type ProbeExtract struct {
	// Kind selects the extraction strategy.
	Kind string `json:"kind"` // "regex" | "json" | "line1"
	// Pattern is the regex (capture group 1 is the version) or JSON path,
	// depending on Kind. Unused by "line1".
	Pattern string `json:"pattern,omitempty"`
}

type ParserConfig struct {
	// Kind selects whether argv is parsed from this document's own
	// declarations or by a Go parser compiled into the binary.
	Kind string `json:"kind"` // "declarative" | "builtin"
	// Name is the registered builtin parser; required when Kind == "builtin".
	Name string `json:"name,omitempty"`
}

type Env struct {
	// Allowed is the toolkit's DESCRIPTION of the environment variables the
	// CLI reads. It is not an enforcement boundary: nothing filters the exec
	// environment against it. The process env is composed by the toolcall
	// controller from the broker-resolved credentials, ToolCall.spec.env,
	// SpiceboxSession.spec.defaultEnv and the sandbox container's own env,
	// and the only key-level gate on that path rejects secret-LOOKING keys in
	// ToolCall.spec.env (pkg/controllers/toolcall.firstSecretLikeKey).
	//
	// What the entries actually drive, all of it keyed off Sensitive:
	// redaction of the value wherever a call is surfaced
	// (pkg/tools/toolspec/validator), the credential a setup flow must obtain
	// (pkg/platform/identity/authkind/cli), the names a SpiceboxClass may not
	// shadow through envDefaults (pkg/controllers/spiceboxclass), and the
	// rendered toolkit report. Adding a variable here therefore documents and
	// wires it; leaving one out does not keep it out of the CLI's environment.
	Allowed []EnvVar `json:"allowed"`
}

type EnvVar struct {
	// Name is the environment variable's name as the CLI reads it.
	Name string `json:"name"`
	// Title is the short human label used in setup UI.
	Title string `json:"title,omitempty"`
	// Description explains to a human what value belongs here.
	Description string `json:"description,omitempty"`
	// Sensitive marks the value for redaction everywhere it could be surfaced.
	Sensitive bool `json:"sensitive,omitempty"`
	// Provider names the identity setup flow that can obtain this value.
	Provider string `json:"provider,omitempty"`
	// Prompt is the question shown when asking a user for the value directly.
	Prompt string `json:"prompt,omitempty"`
	// Credential is the name of the credential that fills this env var.
	Credential string `json:"credential,omitempty"`
}

type Subcommand struct {
	// Path is the subcommand words after the binary, e.g. ["pr", "view"]. An
	// empty path is the bare binary invocation.
	Path []string `json:"path"`
	// Description explains what this subcommand does, for spec authors.
	Description string `json:"description,omitempty"`
	// Positional declares the argument slots, in the order argv fills them.
	Positional []Positional `json:"positional,omitempty"`
	// Flags are the options valid for this subcommand, beyond GlobalFlags.
	Flags []Flag `json:"flags,omitempty"`
	// Effects is what running this subcommand does to the world; the
	// authorization layer reasons over it.
	Effects Effects `json:"effects"`
	// Permission overrides the toolkit-level default for this
	// subcommand.
	Permission *authz.Permission `json:"permission,omitempty"`

	// PermissionVariants branch this subcommand's authority on its ARGUMENTS.
	// CEL `When` over the PARSED args; first match wins, and Permission above
	// is the fallback.
	//
	// Same shape and semantics as MCPServerTool.PermissionVariants,
	// deliberately: a second vocabulary for one idea is how two paths drift,
	// and authz.ResolveVariant already implements this one.
	//
	// It exists because authority is not always a property of WHICH subcommand
	// ran. `gh api -X GET repos/o/n` reads; `gh api -X POST repos/o/n/pulls`
	// opens a pull request. One subcommand, two authorities — and
	// `claude --dangerously-skip-permissions` is the same shape on a CLI with no
	// subcommands at all. Without this, such a tool must be charged its most
	// dangerous reading on EVERY call, which is what forced `gh api` to route
	// every read through a human.
	//
	// Put the SAFE readings in variants and leave the dangerous one as the
	// fallback. An argument nobody anticipated then resolves to the strict
	// permission instead of slipping past an allowlist that failed to name it.
	PermissionVariants []authz.PermissionVariant `json:"permissionVariants,omitempty"`

	// Mode declares how the runner dispatches this subcommand:
	//   "" (default) — request/response (ToolCallMode: sync).
	//   "stream"      — parsed streaming output, no stdin bridge
	//                   (ToolCallMode: stream). For one-shot streaming
	//                   tools, e.g. `claude --print`.
	//   "interactive" — parsed streaming output + a live stdin pipe for
	//                   channel input (ToolCallMode: interactive).
	// "stream" and "interactive" both route stdout through the toolkit's
	// StreamFormat parser.
	Mode string `json:"mode,omitempty"`

	// Timeout overrides the mode-derived per-call execution budget for this
	// subcommand. A Go duration string (time.ParseDuration), e.g. "10m" for a
	// slow `git clone` or "30s" for a fast `git status`. When empty the mode
	// default applies: 30m for stream/interactive, 5m for sync. Validated at
	// load — a non-parseable or non-positive value fails LoadBytes.
	Timeout string `json:"timeout,omitempty"`
}

type Positional struct {
	// Name is the slot's key in a parsed call, and what a CEL constraint
	// addresses it by.
	Name string `json:"name"`
	// Type is the value shape the parser binds and validates against.
	Type string `json:"type"` // "string" | "int" | "path" | "url" | "enum" | "stringList"
	// Required denies an invocation that leaves this slot unfilled.
	Required bool `json:"required,omitempty"`
	// Values is the closed set accepted when Type is "enum"; unused otherwise.
	Values []string `json:"values,omitempty"`
	// SplitOn is the separator that turns one token into a list when Type is
	// "stringList"; empty means the whole token is a single element.
	SplitOn string `json:"splitOn,omitempty"`

	// AfterDashDash marks this slot as where post-`--` arguments start
	// binding. Set it when the wrapped CLI overloads `--` as a separator
	// rather than a plain option terminator: `git log [<rev>] [-- <path>…]`
	// declares AfterDashDash on `paths`, so `git log -- a.txt` binds a.txt to
	// `paths` and leaves `revision-range` unset — as git does.
	//
	// Leave it false when `--` is a plain option terminator (`git clone --
	// <repo> <dir>`): post-`--` arguments then keep filling the declared slots
	// in declared order. At most one positional per subcommand may set it.
	//
	// Post-`--` arguments are ALWAYS bound to some slot; a tail that fits no
	// slot is an ArgCountMismatch. Leaving them unbound would let any
	// `call.positional[...]` constraint be bypassed by inserting a `--`.
	AfterDashDash bool `json:"afterDashDash,omitempty"`
}

type Flag struct {
	// Long is the flag's `--name` form, without the dashes, and is the key a
	// parsed call and its CEL constraints address it by.
	Long string `json:"long"`
	// Short is the single-letter `-n` alias. Empty means there is none.
	Short string `json:"short,omitempty"`
	// Type is the value shape the parser binds; "bool" takes no value.
	Type string `json:"type"` // same set as Positional.Type plus "bool"
	// Description explains the flag to a spec author.
	Description string `json:"description,omitempty"`
	// Sensitive marks this flag's value for redaction everywhere it is
	// surfaced — the parsed call, denial reasons, and the trace.
	Sensitive bool `json:"sensitive,omitempty"`
	// RequiresConstraint marks a flag whose VALUE decides how much authority
	// the call carries, so a spec that admits a subcommand reachable with it
	// must constrain every value it accepts.
	//
	// git's `-c` is the motivating case: it sets one-shot config, which
	// reaches core.fsmonitor, core.hooksPath, core.sshCommand and
	// url.<base>.insteadOf — arbitrary command execution — and it OVERRIDES
	// the GIT_CONFIG_COUNT/KEY/VALUE pinning the sandbox image applies, so a
	// spec admitting it unconstrained yields a shell no matter how narrow its
	// allowSubcommands list is.
	//
	// This exists because the obligation was previously carried by a comment
	// in the toolkit YAML, and two of the four shipped git toolspecs did not
	// meet it — one of them advertised as read-only. A rule a machine can
	// check is the difference between a convention and a guarantee.
	RequiresConstraint bool `json:"requiresConstraint,omitempty"`
	// Values is the closed set accepted when Type is "enum"; unused otherwise.
	Values []string `json:"values,omitempty"`
	// SplitOn is the separator that turns one token into a list when Type is
	// "stringList"; empty means the whole token is a single element.
	SplitOn string `json:"splitOn,omitempty"`

	// OptionalValue declares that the wrapped CLI takes this flag's value
	// ONLY in the attached `--flag=value` form — the bare `--flag` takes no
	// argument. This is git's PARSE_OPT_OPTARG and pflag/cobra's NoOptDefVal:
	// `--decorate`, `--pretty`, `-U`, `--force-with-lease`, `--rebase`,
	// `--recurse-submodules`, `kubectl --dry-run`, and friends.
	//
	// The parser must not consume the following token for such a flag, because
	// the binary will not: `git push --force-with-lease origin main` gives git
	// remote=origin/refspec=main, so a parser that ate `origin` as the lease
	// value would report a positional set the binary never sees, and any
	// constraint over those positionals would run against the wrong argv or be
	// skipped.
	//
	// A bare occurrence records presence as `true` regardless of Type
	// (call.hasFlag() true, call.flag() empty): no value was supplied, and a
	// value-typed comparison against `true` fails the constraint closed rather
	// than matching a fabricated zero value.
	//
	// Invalid on Type "bool" — a bool never carries a value.
	OptionalValue bool `json:"optionalValue,omitempty"`
}

type Effects struct {
	// Destructive marks a subcommand whose result cannot simply be undone.
	Destructive bool `json:"destructive"`
	// Reads names the resource classes the subcommand reads. Empty means it
	// reads nothing a policy needs to reason about.
	Reads []string `json:"reads"`
	// Writes names the resource classes the subcommand mutates. Empty means it
	// is read-only.
	Writes []string `json:"writes"`
	// Network is where the subcommand talks to.
	Network NetworkEffect `json:"network"`
	// Filesystem is what the subcommand touches on disk.
	Filesystem FilesystemEffect `json:"filesystem"`
	// Creds is what credentials the subcommand needs and produces.
	Creds CredsEffect `json:"creds"`
}

type NetworkEffect struct {
	// Destinations are the hosts the subcommand contacts. Empty means it makes
	// no network calls; it is a declaration, not an enforced allowlist.
	Destinations []string `json:"destinations"`
}

type FilesystemEffect struct {
	// Paths are the locations the subcommand reads or writes. Empty means it
	// touches nothing outside its own working directory.
	Paths []string `json:"paths"`
}

type CredsEffect struct {
	// Required names credentials the subcommand cannot run without.
	Required []string `json:"required"`
	// Writes names credentials the subcommand mints or overwrites on disk.
	Writes []string `json:"writes"`
}

// Key returns the map key a parsed call and its CEL constraints address this
// flag by: the long name when there is one, otherwise the short name.
//
// Short-only flags are not an edge case here — git's `-c` and `-C` have no
// long form — and a rule keyed on Long alone would silently never fire for
// exactly the flags that most need one.
func (f Flag) Key() string {
	if f.Long != "" {
		return f.Long
	}
	return f.Short
}

// FlagsRequiringConstraint returns the addressing keys of every flag in this
// toolkit — global or per-subcommand — declared RequiresConstraint, in
// first-seen order and without duplicates.
//
// A spec that admits a subcommand reachable with one of these must constrain
// its values; see spec.ValidateRequiredFlagConstraints.
func (t *Toolkit) FlagsRequiringConstraint() []string {
	var out []string
	seen := map[string]bool{}
	add := func(fs []Flag) {
		for _, f := range fs {
			if !f.RequiresConstraint {
				continue
			}
			k := f.Key()
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, k)
		}
	}
	add(t.GlobalFlags)
	for i := range t.Subcommands {
		add(t.Subcommands[i].Flags)
	}
	return out
}
