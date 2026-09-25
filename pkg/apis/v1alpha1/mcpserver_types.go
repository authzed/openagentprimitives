package v1alpha1

import (
	"encoding/json"
	"fmt"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/authz"
	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,mcp},shortName=mcpsrv
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Valid",type="string",JSONPath=".status.conditions[?(@.type=='Valid')].status"
// +kubebuilder:printcolumn:name="Reachable",type="string",JSONPath=".status.conditions[?(@.type=='Reachable')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient
//
// MCPServer declares one MCP endpoint an agent may call: where it lives
// (spec.server), the allowlisted tools, per-tool CEL argument constraints, the
// credential used at runtime, and the SpiceDB schema fragment its resources
// need.
//
// Namespaced. Reconciled by pkg/controllers/mcpserver, which probes the
// server's tools/list, compiles every constraint, and reflects the outcome on
// Valid and Reachable. The probe is unauthenticated -- runtime auth resolves
// spec.auth.credential at session startup, so Reachable=True is not evidence
// that the agent's credential works.
type MCPServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MCPServerSpec   `json:"spec,omitempty"`
	Status MCPServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type MCPServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MCPServer `json:"items"`
}

// ToolResourceMapping declares how a specific MCP tool relates to SpiceDB
// resources. Consumed by the runner's information-leakage gate.
type ToolResourceMapping struct {
	// Tool is the MCP tool name this mapping applies to.
	Tool string `json:"tool"`

	// Reads declares the resource the tool reads. Set this OR NoTaint;
	// unset both is treated as "unmapped" and may block tool dispatch
	// depending on AgentClass.Authz.InformationLeakage.Mode.
	// +optional
	Reads *ToolReads `json:"reads,omitempty"`

	// NoTaint marks the tool as a pure utility that reads no user data.
	// Set true to opt out of taint capture and the requester view check.
	// +optional
	NoTaint bool `json:"noTaint,omitempty"`
}

// ToolReads describes the SpiceDB resource a read-style MCP tool accesses.
//
// Exactly one of `idArg` or `resultIDField` must be set. Use `idArg` when
// the agent passes the SpiceDB resource ID directly (e.g. an MCPServer
// that takes a UUID). Use `resultIDField` when the agent passes a
// human-readable identifier (e.g. Linear's "L-140") and the SpiceDB
// resource ID (UUID) only appears in the tool's response — the hook
// parses the result content as JSON and extracts the named field.
type ToolReads struct {
	// ResourceType is the SpiceDB definition name (e.g. "linear_issue").
	ResourceType string `json:"resourceType"`

	// IDArg names a top-level field inside the tool's args envelope
	// whose string value is the SpiceDB resource ID. Mutually exclusive
	// with ResultIDField.
	// +optional
	IDArg string `json:"idArg,omitempty"`

	// ResultIDField names a top-level field on the tool result's content
	// (parsed as JSON) whose string value is the SpiceDB resource ID.
	// Set this when the agent's input identifier differs from the
	// SpiceDB resource ID (e.g. Linear `id: "L-140"` returns a UUID in
	// the response). When set, the pre-execute requester view check
	// happens AFTER the call against the response-extracted ID.
	// Mutually exclusive with IDArg.
	// +optional
	ResultIDField string `json:"resultIDField,omitempty"`

	// Permission is the SpiceDB permission checked against the requester
	// (e.g. "view").
	Permission string `json:"permission"`

	// BypassRequesterCheck, when true, skips the requester view check.
	// The taint is still recorded; this is useful for agents whose service
	// identity legitimately has broader access than the requester.
	// +optional
	BypassRequesterCheck *bool `json:"bypassRequesterCheck,omitempty"`
}

// MCPServerSpec mirrors pkg/tools/mcp/spec.Spec field-for-field. Use ToSpec() to
// obtain the runtime form. Mirroring (rather than embedding) keeps deepcopy
// generation fully local to v1alpha1.
type MCPServerSpec struct {
	// Name is the toolspec-level server name; tool names are prefixed with it.
	Name string `json:"name"`
	// Version is the spec author's version of this declaration, not the
	// server's self-reported one.
	Version string `json:"version"`

	// PinnedManifestHash, when set, asserts the canonical tools/list manifest
	// hash ("sha256:…", see pkg/authz/pinning/kinds/mcp) this server must serve.
	// Drift from it flips the PinDrift condition and is enforced per the
	// effective pinning mode. When unset, the first observed manifest is
	// recorded as the baseline (trust-on-first-use) in status.pin.
	// +optional
	PinnedManifestHash string `json:"pinnedManifestHash,omitempty"`

	// Intent is the one-line purpose shown to authors and reviewers.
	Intent string `json:"intent,omitempty"`
	// Server is where the endpoint lives and how to talk to it.
	Server MCPServerServer `json:"server"`
	// Auth is how outbound calls to the server are authenticated.
	Auth MCPServerAuth `json:"auth,omitempty"`
	// CallTimeout bounds a single tool call; zero uses the dispatcher default.
	CallTimeout metav1.Duration `json:"callTimeout,omitempty"`
	// Tools is the allowlist: a tool the server offers but that is absent here
	// is never callable.
	Tools []MCPServerTool `json:"tools"`
	// ToolResourceMap declares, per-tool, the SpiceDB resource a read
	// accesses. Consumed by the runner's information-leakage gate.
	// +optional
	ToolResourceMap []ToolResourceMapping `json:"toolResourceMap,omitempty"`
	// Generation holds authoring-time test cases for the spec generator.
	Generation *MCPServerSpecGeneration `json:"generation,omitempty"`

	// SpiceDBSchema declares the SpiceDB resource definitions this MCPServer
	// contributes. The guardian's schema composer concatenates fragments across
	// every MCPServer an AgentClass references; identical declarations dedupe,
	// conflicting ones fail the AgentClass reconcile.
	// +optional
	SpiceDBSchema *SpiceDBSchemaFragment `json:"spicedbSchema,omitempty"`

	// SiteURL is the service's user-facing homepage. identityd discovers
	// a favicon from this URL for credential-row UI surfaces (portal,
	// deep-link menu, Slack Home tab, credential-request DM). Optional;
	// unset renders the deterministic generated fallback icon.
	//
	// Must be http or https; admission rejects other schemes.
	// +optional
	SiteURL string `json:"siteURL,omitempty"`

	// MCPUIAppTools opts this MCPServer into MCP-UI "app"-visible tool calls:
	// a tool whose MCPServerTool.Visibility is EXACTLY ["app"] — present, and
	// NOT also carrying "model"; see that field's own doc for why "including
	// app" is not sufficient — is synthesized into the browser-callable
	// registry instead of the model's (pkg/agent/tool/mcp/synthesize.go).
	// Absent ⇒ disabled (the permanent default): an app-only tool is then
	// synthesized into NEITHER registry.
	// +optional
	MCPUIAppTools *MCPUIAppToolsSpec `json:"mcpUiAppTools,omitempty"`
}

// MCPUIAppToolsSpec configures MCP-UI app-visible tool calls for this MCPServer.
type MCPUIAppToolsSpec struct {
	// Enabled turns the capability on; no app-visible tool is callable without it.
	Enabled bool `json:"enabled"`

	// MaxCallsPerMin optionally caps autonomous app-tool calls per minute.
	// Enforced by a DEDICATED per-origin limiter on the app-tool call path
	// (runner.AppToolRateLimiter, wired in internal/cmd/runner from this field) —
	// deliberately NOT a toolguard rule, which would also gate this
	// MCPServer's LLM-visible tools and strip its circuit breaker. Nil ⇒
	// unlimited.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxCallsPerMin *int32 `json:"maxCallsPerMin,omitempty"`
}

// SpiceDBSchemaFragment is a SpiceDB schema fragment contributed by an MCPServer or a channel kind.
type SpiceDBSchemaFragment struct {
	// Resources lists structured SpiceDB definitions contributed by this
	// fragment. Each resource is emitted with its declared relations and
	// permissions; the composer dedupes by name across fragments.
	// +optional
	Resources []SpiceDBResource `json:"resources,omitempty"`

	// RawZed is appended verbatim to the composed schema after Resources
	// are emitted. Use this for SpiceDB schema features the structured
	// form can't represent: subject-relations (e.g. `slack_user#user`)
	// and unions (e.g. `user | agent`). The composer does not parse or
	// validate RawZed beyond schema-load time; malformed text fails the
	// SpiceDB WriteSchema call.
	// +optional
	RawZed string `json:"rawZed,omitempty"`
}

// SpiceDBResource is one SpiceDB resource definition contributed by a fragment.
type SpiceDBResource struct {
	// Name is the SpiceDB definition name; the composer dedupes on it.
	Name string `json:"name"`
	// Relations are the resource's relations, emitted verbatim.
	Relations []SpiceDBRelation `json:"relations,omitempty"`
	// Permissions are the resource's computed permissions.
	Permissions []SpiceDBPermission `json:"permissions,omitempty"`

	// Standing declares whether SpiceDB is AUTHORITATIVE for this resource type —
	// that is, whether an approver can be expected to already hold a permission on
	// an instance of it. REQUIRED, with no default.
	//
	//   - required      SpiceDB governs who may approve. The approver pool is
	//                   ApproverPermission on the named instance, and an EMPTY pool
	//                   is a final refusal — nobody can approve what nobody governs.
	//   - session-only  No local permission governs approval for this type, so the
	//                   session's own approvers decide and their decision IS the
	//                   authority. Everything downstream is unchanged: the grant is
	//                   still written, still expiring, still session-scoped and
	//                   revocable, and the tool-call Check still runs.
	//
	// There is deliberately NO default. A default is a hole you open by omission:
	// defaulting to session-only silently widens who may approve a type somebody
	// forgot to classify, and defaulting to required makes a forge-governed type
	// permanently unbindable because nothing writes a SpiceDB tuple for every git
	// remote. Neither failure announces itself, so the author states the answer.
	// Same reasoning as AP_CLUSTER_KIND, which also refuses to default.
	//
	// Choosing is a question about the RESOURCE, not about convenience: does a
	// permission on this instance already say who may speak for it? A CRM company
	// with seeded owner tuples: yes, `required`. A git remote whose permissions
	// live at the forge: no, `session-only`.
	//
	// A cluster or namespace admin can force any type to `required` with
	// SettingsLimits.RequireStandingFor, which no fragment may widen past.
	// +kubebuilder:validation:Enum=session-only;required
	// +kubebuilder:validation:Required
	Standing string `json:"standing"`

	// ApproverPermission names the permission an approver must hold on an
	// instance for their approval to count. Required when Standing is `required`,
	// and must be empty when it is `session-only` — a type nothing governs has no
	// permission to name, and accepting one there would read as governance that
	// is never consulted.
	//
	// Declared rather than assumed. This was hardcoded to "owner" at four call
	// sites, which silently made two very different types look identical to the
	// approval router: crm_company's `owner` is a real computed permission backed
	// by seeded tuples, while git_repo's `owner` is a bare relation declared only
	// so the checks are answerable and never populated by anything. The router
	// could not tell governance from an artifact of schema shape, so a push to a
	// git remote resolved to an empty owner-set and became unapprovable forever.
	//
	// Naming it also lifts the assumption that the approving permission is called
	// "owner": a type may route approval through `maintainer`, `admin`, or any
	// permission its own schema defines.
	// +optional
	ApproverPermission string `json:"approverPermission,omitempty"`
	// Display declares how an INSTANCE of this resource type presents on an
	// approval card — an icon, and how to turn its raw value (a URL, typically)
	// into a short label. Absent means the card falls back to the wire type
	// name, exactly as before this field existed.
	// +optional
	Display *SpiceDBResourceDisplay `json:"display,omitempty"`
}

// SpiceDBResourceDisplay is how a resource TYPE says its instances should
// present on an approval card. It never carries an instance's own value —
// only how to derive and label it, so an agent-authored URL can never
// smuggle in a rendering directive.
type SpiceDBResourceDisplay struct {
	// Name is the human name for this resource TYPE — "Git repository", never
	// the wire handle "git_repo". Shown when no per-instance label can be
	// derived (Label is "none", unset, or the deriver produces nothing for
	// this instance's value).
	// +optional
	// +kubebuilder:validation:MaxLength=60
	Name string `json:"name,omitempty"`

	// Icon names a glyph from a CLOSED, code-defined registry — never a URL
	// and never free-form. A vendor-specific type (github_repo) may name the
	// vendor's own mark; a host-agnostic type (git_repo) must name a generic
	// one, because claiming a vendor mark would lie the moment an instance
	// points somewhere else (a self-hosted remote). An unrecognized name
	// renders no icon — never a fallback image, never a guess.
	// +optional
	// +kubebuilder:validation:MaxLength=40
	Icon string `json:"icon,omitempty"`

	// Label names a deriver from a CLOSED, code-defined set that turns an
	// instance's raw value into a short display label — "url_path",
	// "b64url_path", "last_segment", or "none" (the set registered in
	// pkg/authz/plangate's labelDerivers, which is the authority; this list is
	// prose and has drifted from it once). Not a template and not a regex: nothing here
	// can produce a label the code did not author, and the deriver never sees
	// anything an agent did not itself write into the plan (the instance
	// value), so a declaration can shorten a value's presentation but can
	// never fabricate one. An unrecognized name derives nothing, and the card
	// falls back to Name.
	// +optional
	// +kubebuilder:validation:MaxLength=40
	Label string `json:"label,omitempty"`
}

// Standing modes for SpiceDBResource.Standing.
const (
	StandingSessionOnly = "session-only"
	StandingRequired    = "required"
)

// StandingDeclared reports whether this resource states a standing at all.
//
// Replaces an EffectiveStanding helper that resolved unset to session-only.
// That default was the hole: a type nobody classified silently became one whose
// approval the session's own approvers could grant, and nothing anywhere said
// so. Callers must now handle "undeclared" as its own outcome and refuse, which
// is why this returns a bool rather than picking a side — a fail-closed guess of
// `required` would be just as wrong in the other direction, making a
// forge-governed type permanently unbindable.
func (r SpiceDBResource) StandingDeclared() bool {
	return r.Standing == StandingRequired || r.Standing == StandingSessionOnly
}

// ValidateStanding reports why this resource's standing declaration is
// unusable, or nil when it is complete.
//
// Two rules, both fail-closed. A missing standing is refused rather than
// defaulted (see the Standing doc). And ApproverPermission must be present
// exactly when it is consulted: required-without-one leaves the router with no
// pool to resolve, and session-only-with-one reads as governance that is never
// checked — the kind of line a reviewer trusts and the code ignores.
func (r SpiceDBResource) ValidateStanding() error {
	switch r.Standing {
	case StandingRequired:
		if r.ApproverPermission == "" {
			return fmt.Errorf("resource %q: standing is %q but approverPermission is unset; name the permission an approver must hold on an instance", r.Name, StandingRequired)
		}
	case StandingSessionOnly:
		if r.ApproverPermission != "" {
			return fmt.Errorf("resource %q: standing is %q, which consults no permission, but approverPermission is %q; remove it or set standing to %q", r.Name, StandingSessionOnly, r.ApproverPermission, StandingRequired)
		}
	case "":
		return fmt.Errorf("resource %q: standing is unset; declare %q (SpiceDB governs who may approve) or %q (nothing local does). There is no default", r.Name, StandingRequired, StandingSessionOnly)
	default:
		return fmt.Errorf("resource %q: standing %q is not recognized; declare %q or %q", r.Name, r.Standing, StandingRequired, StandingSessionOnly)
	}
	return nil
}

// SpiceDBRelation is one relation on a resource.
type SpiceDBRelation struct {
	// Name is the relation name as it appears in the composed schema.
	Name string `json:"name"`
	// SubjectType must be a bare resource-type name (e.g. "user",
	// "hubspot_owner") declared in this same SpiceDBSchema or the
	// implicit "user" type. Wildcards ("user:*") are expressed via
	// the separate Wildcard field. Subject-relation forms
	// ("team#member") are NOT representable. Exactly one
	// SubjectType per Relation — SpiceDB unions like
	// `relation viewer: user | team#member` cannot be modeled.
	SubjectType string `json:"subjectType"`
	// Wildcard, when true, makes the relation accept ANY subject
	// of SubjectType — emitted as `relation <name>: <subjectType>:*`
	// in the composed schema. Use sparingly: a wildcard relation
	// effectively grants the underlying-resource permission to
	// every subject of that type, so any per-call gating must come
	// from a different layer (e.g. `stateImpact: external` on the
	// tool, which routes every call through the approval flow
	// regardless of the SpiceDB Check result).
	// +optional
	Wildcard bool `json:"wildcard,omitempty"`
}

// SpiceDBPermission is one permission on a resource. Expr is
// the right-hand-side of the SpiceDB schema's `permission <name> = ...`
// statement; the composer pastes it verbatim.
type SpiceDBPermission struct {
	// Name is the permission name.
	Name string `json:"name"`
	// Expr is the right-hand side of `permission <name> = …`, pasted verbatim.
	Expr string `json:"expr"`

	// Title is the phrase a human reads on an approval card instead of the
	// permission's handle. `perm:push:git_repo` is a WIRE FORMAT; asking
	// somebody to decide on it makes the decision slower and worse exactly
	// where care matters most.
	//
	// Declared here because this is where the permission itself is declared, so
	// one declaration serves every channel and the CLI. Absent is fine and
	// common: the card detokenizes the handle instead, which shows no plumbing
	// and invents no English.
	// +optional
	// +kubebuilder:validation:MaxLength=120
	Title string `json:"title,omitempty"`

	// PlanningNote is one line of guidance the agent reads while DECLARING a
	// plan, rendered next to the handles it may declare.
	//
	// The knowledge that shapes a good plan is toolkit-specific, so it lives
	// with the toolkit rather than in the runner's generic prompt. git_repo
	// splits its permissions across two instances — read/write key on the
	// checked-out copy, fetch/push on the remote URL — so a phase declaring
	// `write` without `read` can change files it cannot open, and the first
	// read interrupts the human with an amendment the plan should have carried
	// from the start.
	//
	// It shapes what the agent DECLARES, never what anything grants: a note is
	// prose the model reads at plan time, and no authorization decision reads
	// it. A phase that ignores its guidance still gets the gate it earned.
	// +optional
	// +kubebuilder:validation:MaxLength=300
	PlanningNote string `json:"planningNote,omitempty"`
}

// MCPServerSpecGeneration mirrors mcpspec.Generation.
type MCPServerSpecGeneration struct {
	// TestCases are the author-supplied allow/deny examples for the spec.
	TestCases []MCPServerSpecTestCase `json:"testCases,omitempty"`
}

// MCPServerSpecTestCase mirrors mcpspec.TestCase. Args is stored as
// raw JSON to sidestep deepcopy/OpenAPI generation issues with
// map[string]any — the JSON roundtrip in ToSpec() converts back to
// map[string]any on the runtime side.
type MCPServerSpecTestCase struct {
	// Intent describes what this case is meant to demonstrate.
	Intent string `json:"intent,omitempty"`
	// ToolName is the tool the case exercises.
	ToolName string `json:"toolName"`
	// Args is the call's argument envelope.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	Args json.RawMessage `json:"args,omitempty"`
	// ExpectedAllow is whether the constraints should admit these args.
	ExpectedAllow bool `json:"expectedAllow"`
}

type MCPServerServer struct {
	// URL is the MCP server endpoint. Production servers should use
	// https://; http:// is permitted for in-cluster servers addressed
	// by a Kubernetes Service DNS name (*.svc / *.svc.cluster.local)
	// and for loopback test stubs.
	//
	// Scheme/SSRF validation is intentionally NOT a CRD
	// +kubebuilder:validation:Pattern: a regex blunt enough to forbid
	// loopback/private destinations would also reject the legitimate
	// in-cluster http:// case and the envtest e2e harness (which
	// applies http://127.0.0.1:<port>).
	//
	// The authoritative runtime SSRF backstop is the guarded dialer in
	// pkg/x/safehttp: every client that fetches this URL (the MCPServer
	// controller's probe, the runner's session-start probe, the MCP
	// tool-call dispatcher, and the `oap` CLI probe paths) resolves the
	// host and refuses any private/loopback/link-local IP. A finer-
	// grained controller-side scheme check (e.g. surfacing a non-https,
	// non-in-cluster URL on the Valid condition) is RECOMMENDED as a
	// future enhancement but is not implemented today — do not rely on
	// the controller for SSRF protection; pkg/x/safehttp is the backstop.
	URL string `json:"url"`
	// Transport is the MCP wire transport to speak to this endpoint.
	Transport string `json:"transport"`
}

type MCPServerAuth struct {
	// Type is the authentication mechanism the MCPServer requires. "static":
	// the user supplies a long-lived token (PAT/API key) through the
	// /my/accounts/<credname>/link form. "oauth": the credential comes from the
	// OAuth Authorization Code flow via /link/oauth/<credname>. Empty means the
	// portal falls back to the PAT form.
	//
	// +optional
	// +kubebuilder:validation:Enum=static;oauth;federated
	Type string `json:"type,omitempty"`

	// Provider names a provider in the /providers/ library. The setup
	// engine uses this to drive `oap agent setup-identity` for this server.
	// At runtime the credential is resolved by name (spec.auth.credential)
	// from the AgentIdentity; this Provider field is metadata for setup,
	// not a runtime hook.
	// +optional
	Provider string `json:"provider,omitempty"`

	// Title is the short user-facing display name for this server's
	// credential ("Linear"). Shown as the bold header of a
	// credential-request row. Takes precedence over the provider catalog;
	// empty falls back to the catalog (via Provider) or a humanized
	// credential name.
	// +optional
	Title string `json:"title,omitempty"`

	// Description is the user-facing "what is this token" sentence for
	// this server's credential. Takes precedence over the provider
	// catalog; empty falls back to the catalog or renders no description.
	// +optional
	Description string `json:"description,omitempty"`

	// Header is the HTTP header name to set on outbound requests
	// (default "Authorization").
	// +optional
	Header string `json:"header,omitempty"`

	// ValuePrefix is prepended to the resolved access_token in the
	// header value (default "Bearer ").
	// +optional
	ValuePrefix string `json:"valuePrefix,omitempty"`

	// Credential is the name of the credential (in the agent's identity
	// catalog) that fills this server's auth header. When empty,
	// resolution falls back to the MCPServer's metadata.name.
	// +optional
	Credential string `json:"credential,omitempty"`

	// Resource is the identifier the enterprise IdP knows this server by,
	// used as the ID-JAG audience when Type=federated. Required when
	// Type=federated; ignored otherwise.
	// +optional
	Resource string `json:"resource,omitempty"`
}

type MCPServerTool struct {
	// Name is the tool name as the upstream server reports it.
	Name string `json:"name"`
	// Intent is the one-line purpose shown to authors and reviewers.
	Intent string `json:"intent,omitempty"`
	// DescriptionOverride replaces the server's own description in the prompt;
	// empty keeps what the server sent.
	DescriptionOverride string `json:"descriptionOverride,omitempty"`
	// Args is the argument allowlist and CEL constraints for this tool.
	Args MCPServerToolArgs `json:"args,omitempty"`
	// Deny lists effect and trust capabilities that refuse the call pre-dispatch.
	Deny MCPServerToolDeny `json:"deny,omitempty"`
	// Effects is the declared effect profile used for approval and gating.
	Effects MCPServerToolEffects `json:"effects,omitempty"`
	// Visibility mirrors mcpspec.Tool.Visibility (MCP Apps' _meta.ui.visibility):
	// which surfaces ("model" / "app") a tool is exposed to.
	//
	// This repo's routing is an EXCLUSIVE split, not a fan-out (see
	// pkg/agent/tool/mcp/synthesize.go): a tool is browser-callable if and
	// only if this list contains "app" and does NOT contain "model", and the
	// owning MCPServer sets mcpUiAppTools.enabled. Concretely:
	//
	//	unset / []        -> model-visible only; never reaches the browser
	//	["app"]           -> browser-callable (with the opt-in); withheld from the model
	//	["app"] no opt-in -> rejected by default: in NEITHER registry
	//	["model"]         -> model-visible only
	//	["app","model"]   -> model-visible only; NOT browser-callable
	//
	// So "unset" is not "both": it is the model surface. A tool intended for
	// an agent UI must say ["app"] and nothing else. `oap agent lint` reports
	// any other value for a tool an AgentUI binds to.
	// +optional
	Visibility []string `json:"visibility,omitempty"`
	// Trust is the SEP-1913 trust + action-security annotation snapshot.
	// Mirrors mcpspec.Trust.
	// +optional
	Trust MCPServerToolTrust `json:"trust,omitempty"`
	// Permission declares the per-tool authz policy. Optional in the schema
	// only: a tool without one fails AgentClass-time validation.
	// +optional
	Permission *authz.Permission `json:"permission,omitempty"`

	// PermissionVariants are conditional Permission blocks, each carrying a CEL
	// `When` predicate over the call's args. First match wins; Permission above
	// is the fallback when none match.
	// +optional
	PermissionVariants []authz.PermissionVariant `json:"permissionVariants,omitempty"`

	// WritesRelationships declares JIT SpiceDB relationship writes the
	// dispatcher performs after a successful tool call. `When` and `ForEach`
	// are CEL over (args, result); the tuple fields are CEL string expressions.
	// +optional
	WritesRelationships []MCPServerRelationshipWrite `json:"writesRelationships,omitempty"`

	// Observes declares facts this tool's result asserts about specific
	// resource instances, co-derived with the subjects they are about.
	// Evaluated after a SUCCESSFUL call.
	// +optional
	// +listType=atomic
	Observes []ObservesBlock `json:"observes,omitempty"`

	// Labels declares per-tool CEL blocks extracting (resourceType, id, name)
	// tuples from tool responses for approval-prompt rendering. Evaluated after
	// a SUCCESSFUL call; failures are non-fatal (logged and skipped). Labels
	// never reach any LLM context.
	// +optional
	Labels []MCPServerLabelExtract `json:"labels,omitempty"`
}

// MCPServerToolEffects mirrors mcpspec.Effects.
type MCPServerToolEffects struct {
	// Destructive means a call can remove or overwrite upstream state.
	Destructive bool `json:"destructive,omitempty"`
	// ReadOnly means a call mutates nothing upstream.
	ReadOnly bool `json:"readOnly,omitempty"`
	// Idempotent means repeating a call has the same effect as making it once.
	Idempotent bool `json:"idempotent,omitempty"`
	// OpenWorld means the call may reach systems beyond the named server.
	OpenWorld bool `json:"openWorld,omitempty"`
}

// MCPServerToolTrust mirrors mcpspec.Trust — a snapshot of SEP-1913
// trust + action-security annotations captured from the MCP server's
// tools/list response.
type MCPServerToolTrust struct {
	// MaliciousActivityHint is the server's own admission that this tool can be
	// abused; self-reported, so treat it as a signal, not a guarantee.
	// +optional
	MaliciousActivityHint bool `json:"maliciousActivityHint,omitempty"`
	// Attribution names the parties the server credits for the tool.
	// +optional
	Attribution []string `json:"attribution,omitempty"`
	// InputMetadata is the server's per-argument security annotations, verbatim.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	InputMetadata *apiextv1.JSON `json:"inputMetadata,omitempty"`
	// ReturnMetadata is the server's per-result security annotations, verbatim.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	ReturnMetadata *apiextv1.JSON `json:"returnMetadata,omitempty"`
}

// MCPServerDenyTrust mirrors mcpspec.DenyTrust. Each field opts the
// spec into pre-call denial when the corresponding Trust capability
// is asserted.
type MCPServerDenyTrust struct {
	// OutcomesIrreversible denies the call when the server declares its effects
	// cannot be undone.
	// +optional
	OutcomesIrreversible bool `json:"outcomesIrreversible,omitempty"`
	// DestinationPublic denies the call when it would write somewhere publicly
	// visible.
	// +optional
	DestinationPublic bool `json:"destinationPublic,omitempty"`
	// SourceUntrustedPublic denies the call when it would read untrusted public
	// content into the agent's context.
	// +optional
	SourceUntrustedPublic bool `json:"sourceUntrustedPublic,omitempty"`
}

// MCPServerRelationshipWrite is one JIT-write block on an MCPServer
// tool. The dispatcher invokes it after tool.Execute succeeds.
type MCPServerRelationshipWrite struct {
	// When is a CEL boolean expression with `args`, `result` in
	// scope; the block is skipped if it evaluates false.
	// When unset, the block is always taken.
	// +optional
	When string `json:"when,omitempty"`
	// ForEach is a CEL expression that must evaluate to a list.
	// One tuple is emitted per element, with `item` bound to that
	// element. When unset, exactly one tuple is emitted.
	// +optional
	ForEach string `json:"forEach,omitempty"`
	// Tuple holds the CEL expressions that compose the SpiceDB
	// relationship tuple.
	Tuple MCPServerRelationshipTuple `json:"tuple"`
	// Exclusive makes this write atomically write-once per subject: the write
	// FAILS (no tuple written) if the subject already holds `relation` on ANY
	// resource of the tuple's resource type. Used for session-pin semantics
	// (a session may be pinned to exactly one cluster). Implemented as a
	// SpiceDB MUST_NOT_MATCH precondition, so it is atomic under concurrent
	// writers. Default false preserves the plain TOUCH-upsert behavior.
	// +optional
	Exclusive bool `json:"exclusive,omitempty"`
	// RequireSlotBound refuses every tuple this block emits unless the calling
	// session holds a SLOT GRANT on the tuple's RESOURCE — the instance a human
	// (or the pool machinery acting on one's approval) named for this session.
	//
	// Declare it on a block that writes an identity or an authority tuple onto
	// an instance the tool itself names. Without it, whatever id the tool's
	// response happens to carry becomes the resource of a real SpiceDB write,
	// so a response naming somebody else's instance writes there too. With it,
	// the write can only ever land on an instance the session was already
	// bound to.
	//
	// The grant's PERMISSION is deliberately not consulted: a grant is a human
	// act naming the instance, and which permission it carries is the pool
	// machinery's concern. Any slot_grant_* on the resource binds it.
	//
	// Default false is byte-identical to the previous behaviour — an unmarked
	// block consults nothing. A marked block whose dispatcher has no checker
	// wired is REFUSED, not written: see pkg/authz/relwrites.Run.
	// +optional
	RequireSlotBound bool `json:"requireSlotBound,omitempty"`
}

// MCPServerRelationshipTuple is the (resource, relation, subject)
// triple expressed as CEL string expressions.
type MCPServerRelationshipTuple struct {
	// Resource is a CEL string expression that must evaluate to a
	// SpiceDB object reference of the form "<type>:<id>".
	Resource string `json:"resource"`
	// Relation is a CEL string expression that must evaluate to a
	// relation name declared on the Resource's type.
	Relation string `json:"relation"`
	// Subject is a CEL string expression that must evaluate to a
	// SpiceDB object reference of the form "<type>:<id>".
	Subject string `json:"subject"`
}

// ObservesBlock is one observation declaration on a tool. The dispatcher
// evaluates it after tool.Execute succeeds.
//
// One block yields the SUBJECTS and the FACTS from the same `item`. They are
// not two declarations that happen to agree: a fact and the object it describes
// come out of one payload, which is what stops a fact about one instance from
// being made to answer for another.
type ObservesBlock struct {
	// When is a CEL boolean over `args`, `result`; the block is skipped if it
	// evaluates false. Unset means always taken.
	// +optional
	When string `json:"when,omitempty"`
	// ForEach is a CEL expression yielding a list. One observation is emitted
	// per element with `item` bound to it. Unset emits exactly one, with `item`
	// bound to nil.
	// +optional
	ForEach string `json:"forEach,omitempty"`
	// Subjects are the objects this observation is about, as CEL string
	// expression pairs. At least one is REQUIRED: a block recording facts about
	// nothing would produce a session-scoped boolean that answers for every
	// instance at once.
	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	Subjects []ObserveSubject `json:"subjects"`
	// Facts maps a fact name to a CEL expression yielding its value, evaluated
	// against the same `item` as Subjects.
	// +kubebuilder:validation:MinProperties=1
	Facts map[string]string `json:"facts"`
}

// ObserveSubject is one subject as a (resourceType, resourceID) pair of CEL
// string expressions.
type ObserveSubject struct {
	// ResourceType is a CEL string expression yielding a SpiceDB definition
	// name (commonly a literal, e.g. `"github_pr"`).
	ResourceType string `json:"resourceType"`
	// ResourceID is a CEL string expression yielding the object id.
	ResourceID string `json:"resourceID"`
}

// MCPServerLabelExtract declares a per-tool labels-CEL block. Mirrors
// the writesRelationships shape: same when/forEach semantics, same
// (args, result, item) CEL bindings. Evaluated by the MCP dispatcher
// after every successful tool call; tuples land in the runner's
// in-memory LabelStore and are surfaced to channel renderers via the
// ToolApprovalRequestPayload.Labels field. Threat-model note: labels
// NEVER reach any LLM context.
type MCPServerLabelExtract struct {
	// When is a CEL boolean expression with `args`, `result` in
	// scope; the block is skipped if it evaluates false.
	// +optional
	When string `json:"when,omitempty"`
	// ForEach is a CEL expression that must evaluate to a list. One
	// label is emitted per element with `item` bound to that element.
	// When unset, exactly one label is emitted with item=nil.
	// +optional
	ForEach string `json:"forEach,omitempty"`
	// Label is the (resourceType, id, name) CEL triple. All three
	// fields are required CEL string expressions.
	Label MCPServerLabelTuple `json:"label"`
}

// MCPServerLabelTuple is the (resourceType, id, name) triple expressed
// as CEL string expressions, evaluated against (args, result, item).
type MCPServerLabelTuple struct {
	// ResourceType is a CEL string expression yielding the SpiceDB
	// definition name (e.g. `"crm_company"`).
	ResourceType string `json:"resourceType"`
	// ID is a CEL string expression yielding the resource id.
	ID string `json:"id"`
	// Name is a CEL string expression yielding the friendly label
	// shown to approvers. Capped + sanitized at extraction time.
	Name string `json:"name"`
}

type MCPServerToolArgs struct {
	// AllowedFields lists the argument keys the agent may pass to this
	// tool. Enforcement is fail-closed: an empty/unset AllowedFields
	// DENIES any argument the agent passes (an arg-less tool still
	// works). To accept free-form arguments, set UnconstrainedArgs=true
	// instead of leaving this empty.
	// +optional
	AllowedFields []string `json:"allowedFields,omitempty"`
	// UnconstrainedArgs explicitly opts this tool out of AllowedFields
	// enforcement, for a tool that legitimately accepts arbitrary
	// arguments. Required to allow free-form args — an empty AllowedFields
	// alone is fail-closed, not allow-all.
	// +optional
	UnconstrainedArgs bool `json:"unconstrainedArgs,omitempty"`
	// Constraints are CEL predicates every call's args must satisfy (AND).
	Constraints []MCPServerConstraint `json:"constraints,omitempty"`
	// SensitiveFields names arg keys to redact from logs and approval prompts.
	SensitiveFields []string `json:"sensitiveFields,omitempty"`
}

type MCPServerConstraint struct {
	// CEL is a boolean expression over the call's args; false denies the call.
	CEL string `json:"cel"`
	// Message is the denial text shown when CEL evaluates false.
	Message string `json:"message,omitempty"`
}

type MCPServerToolDeny struct {
	// Effects denies calls by declared effect (destructive, reads, writes).
	Effects MCPServerDenyEffects `json:"effects,omitempty"`
	// Trust denies calls by asserted SEP-1913 trust capability.
	// +optional
	Trust MCPServerDenyTrust `json:"trust,omitempty"`
}

type MCPServerDenyEffects struct {
	// Destructive denies any tool the server marks destructive.
	Destructive bool `json:"destructive,omitempty"`
	// Reads names resource kinds whose reads are denied.
	Reads []string `json:"reads,omitempty"`
	// Writes names resource kinds whose writes are denied.
	Writes []string `json:"writes,omitempty"`
	// Creds denies credential-touching effects.
	Creds MCPServerDenyCreds `json:"creds,omitempty"`
}

type MCPServerDenyCreds struct {
	// Writes denies any tool that would write credential material.
	Writes bool `json:"writes,omitempty"`
}

type MCPServerStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// LastValidatedAt is the last time the server spec was successfully validated.
	// +optional
	LastValidatedAt *metav1.Time `json:"lastValidatedAt,omitempty"`
	// ObservedTools is the snapshot of names returned by the server's most
	// recent successful tools/list probe. Sorted, deduplicated.
	// +optional
	ObservedTools []string `json:"observedTools,omitempty"`

	// Pin is the recorded manifest baseline in the common PinRecord shape:
	// Digest = canonical manifest hash, Version = the server's self-reported
	// serverInfo.version (audit metadata only — self-reported by the party
	// pinning defends against).
	// +optional
	Pin *PinRecord `json:"pin,omitempty"`

	// Conditions carries Valid, Reachable, PinDrift and SpiceDBSchemaValid.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// MCPServer condition types.
const (
	MCPServerConditionValid     = "Valid"
	MCPServerConditionReachable = "Reachable"
	// MCPServerConditionSpiceDBSchemaValid is guardian-owned, NOT the mcpserver
	// controller that owns Valid/Reachable above. False means this MCPServer's
	// fragment was excluded from the cluster-wide schema compose, either as
	// FragmentInvalid (bad on its own — a RawZed syntax error, a reserved
	// scaffold definition redeclared, an internal conflict) or FragmentConflict
	// (valid alone, but colliding with an already-accepted fragment; earlier
	// namespace/name wins and the later is marked here).
	//
	// Isolating the failure here rather than freezing the compose pass is what
	// stops one tenant's bad or hostile MCPServer from taking down
	// SchemaIncluded for every AgentSessionGrants in the cluster.
	MCPServerConditionSpiceDBSchemaValid = "SpiceDBSchemaValid"
)

// MCPServerReasonFragmentInvalid is the SpiceDBSchemaValid=False reason
// when spec.spiceDBSchema fails per-fragment validation ON ITS OWN
// (guardianschema.ValidateFragment) — a RawZed syntax error, a reserved
// scaffold definition redeclared, or an internal conflict.
const MCPServerReasonFragmentInvalid = "FragmentInvalid"

// MCPServerReasonFragmentConflict is the SpiceDBSchemaValid=False reason
// when this fragment is valid on its own but conflicts with an
// already-accepted fragment from ANOTHER MCPServer (same resource name,
// different body) — the N-way incremental isolation pass
// (guardianschema.PartitionCompatibleFragments) rejected it so it does
// not freeze the whole cluster-wide schema. First-in-sort-order wins the
// conflict, so the MCPServer carrying this reason is the later of the two.
const MCPServerReasonFragmentConflict = "FragmentConflict"

// MCPServerReasonFragmentValid is the SpiceDBSchemaValid=True reason —
// only stamped to clear a PREVIOUSLY-invalid fragment that has since been
// fixed; a fragment that has always been valid never has this condition
// set at all (see pkg/controllers/guardian's per-reconcile fragment
// validation pass).
const MCPServerReasonFragmentValid = "FragmentValid"

// ToSpec converts the CR spec to the pkg/tools/mcp/spec.Spec runtime form via JSON
// round-trip. The two types share an identical JSON shape by construction.
func (s *MCPServerSpec) ToSpec() (*mcpspec.Spec, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("marshal MCPServerSpec: %w", err)
	}
	var out mcpspec.Spec
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("unmarshal into mcpspec.Spec: %w", err)
	}
	return &out, nil
}

// LookupToolResourceMapping returns the mapping for tool name `name`, or nil.
func (s *MCPServerSpec) LookupToolResourceMapping(name string) *ToolResourceMapping {
	for i := range s.ToolResourceMap {
		if s.ToolResourceMap[i].Tool == name {
			return &s.ToolResourceMap[i]
		}
	}
	return nil
}

func init() {
	SchemeBuilder.Register(&MCPServer{}, &MCPServerList{})
}
