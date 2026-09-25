package spec

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/authz"
	toolspec "github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
)

// Spec is the flat MCP authoring document. It mirrors pkg/tools/toolspec/spec.Spec
// in shape — same intent / version / constraints / deny.effects fields — but
// targets a JSON argument surface instead of argv.
type Spec struct {
	// Name identifies this spec; it prefixes the agent-visible tool names.
	Name string `json:"name"`
	// Version is the schema version of this document.
	Version string `json:"version"`
	// Intent is the human sentence describing what this spec is for.
	Intent string `json:"intent,omitempty"`
	// Server is the upstream MCP endpoint the tools are dispatched to.
	Server Server `json:"server"`
	// Auth is how a credential is presented to that endpoint. Zero means the
	// server is called unauthenticated.
	Auth Auth `json:"auth,omitempty"`
	// CallTimeout bounds one tools/call. Zero means the caller's default.
	CallTimeout metav1.Duration `json:"callTimeout,omitempty"`
	// Tools is the closed set of upstream tools this spec permits; anything
	// the server also offers is not reachable.
	Tools []Tool `json:"tools"`
	// Generation is authoring provenance; nil for a hand-written spec.
	Generation *Generation `json:"generation,omitempty"`
	// SpiceDBSchema mirrors MCPServerSpec.SpiceDBSchema.
	SpiceDBSchema *SpiceDBSchema `json:"spicedbSchema,omitempty"`
}

// SpiceDBSchema mirrors v1alpha1.SpiceDBSchemaFragment.
type SpiceDBSchema struct {
	// Resources are the object types this server's authz model contributes to
	// the composed cluster schema.
	Resources []SpiceDBResource `json:"resources"`
}

// SpiceDBResource mirrors v1alpha1.SpiceDBResource.
type SpiceDBResource struct {
	// Name is the SpiceDB object-type name.
	Name string `json:"name"`
	// Relations are the direct edges declared on the type.
	Relations []SpiceDBRelation `json:"relations,omitempty"`
	// Permissions are the computed permissions declared on the type.
	Permissions []SpiceDBPermission `json:"permissions,omitempty"`
}

// SpiceDBRelation mirrors v1alpha1.SpiceDBRelation.
type SpiceDBRelation struct {
	// Name is the relation name as written in the schema.
	Name string `json:"name"`
	// SubjectType is the object type allowed on the subject side.
	SubjectType string `json:"subjectType"`
}

// SpiceDBPermission mirrors v1alpha1.SpiceDBPermission.
type SpiceDBPermission struct {
	// Name is the permission name as written in the schema.
	Name string `json:"name"`
	// Expr is the SpiceDB permission expression, e.g. "owner + parent->view".
	Expr string `json:"expr"`
}

type Server struct {
	// URL is the MCP endpoint tools/call is POSTed to.
	URL string `json:"url"`
	// Transport is the wire protocol; see TransportStreamableHTTP.
	Transport string `json:"transport"`
}

type Auth struct {
	// Provider names the identity setup flow that obtains the credential.
	Provider string `json:"provider,omitempty"`
	// Header is the request header the credential is sent in. Empty means no
	// auth is sent at all.
	Header string `json:"header,omitempty"`
	// ValuePrefix is prepended to the credential value, e.g. "Bearer ".
	ValuePrefix string `json:"valuePrefix,omitempty"`
	// Credential is the name of the credential that fills this server's auth header.
	Credential string `json:"credential,omitempty"`
}

type Tool struct {
	// Name must match the upstream tool's name exactly; it is the key the
	// validator and dispatcher look the tool up by.
	Name string `json:"name"`
	// Intent is the human sentence saying why this tool is permitted.
	Intent string `json:"intent,omitempty"`
	// DescriptionOverride replaces the server's own description in the prompt
	// the model sees. Empty keeps the server's text.
	DescriptionOverride string `json:"descriptionOverride,omitempty"`
	// Args bounds the JSON argument surface a call may present.
	Args Args `json:"args,omitempty"`
	// Deny rules refuse the call before it is dispatched.
	Deny Deny `json:"deny,omitempty"`
	// Effects is the annotation snapshot Deny.Effects is evaluated against.
	Effects Effects `json:"effects,omitempty"`
	// Trust is the SEP-1913 annotation snapshot Deny.Trust is evaluated
	// against.
	Trust Trust `json:"trust,omitempty"`
	// Visibility mirrors MCP Apps' _meta.ui.visibility as recovered into
	// probe.Tool.Annotations.Visibility: which surfaces ("model" / "app")
	// the tool is exposed to. Empty means the server did not assert it —
	// this repo's one reader, pkg/agent/tool/mcp/synthesize.go's exclusive
	// app/model split, treats that the same as ["model"]: model-visible
	// only, never browser-callable. Populated by the authoring flow (see
	// MCPServerTool); not written by this package.
	Visibility []string `json:"visibility,omitempty"`
	// Permission mirrors MCPServerTool.Permission — the per-tool authz
	// policy used when no PermissionVariants match (or as the only check
	// when no variants are declared).
	Permission *authz.Permission `json:"permission,omitempty"`
	// PermissionVariants mirrors MCPServerTool.PermissionVariants.
	PermissionVariants []authz.PermissionVariant `json:"permissionVariants,omitempty"`
	// WritesRelationships mirrors MCPServerTool.WritesRelationships.
	WritesRelationships []RelationshipWrite `json:"writesRelationships,omitempty"`
	// Observes mirrors MCPServerTool.Observes. Json tags must match
	// v1alpha1.ObservesBlock exactly: MCPServerSpec.ToSpec() is a JSON
	// round-trip, and a field with no matching tag here is silently dropped
	// rather than surfaced as an error.
	Observes []ObservesSpec `json:"observes,omitempty"`
}

// RelationshipWrite mirrors MCPServerRelationshipWrite.
type RelationshipWrite struct {
	// When is a CEL guard; the block is skipped when it is false. Unset means
	// the block always runs.
	When string `json:"when,omitempty"`
	// ForEach is a CEL expression yielding a list, one tuple per element with
	// `item` bound. Unset emits exactly one tuple.
	ForEach string `json:"forEach,omitempty"`
	// Tuple is the relationship to write, as CEL string expressions.
	Tuple RelationshipTuple `json:"tuple"`
}

// RelationshipTuple mirrors MCPServerRelationshipTuple.
type RelationshipTuple struct {
	// Resource is a CEL string expression yielding "<type>:<id>".
	Resource string `json:"resource"`
	// Relation is a CEL string expression yielding a relation name on
	// Resource's type.
	Relation string `json:"relation"`
	// Subject is a CEL string expression yielding "<type>:<id>".
	Subject string `json:"subject"`
}

// ObservesSpec mirrors v1alpha1.ObservesBlock (via MCPServerTool.Observes).
type ObservesSpec struct {
	// When is a CEL guard; the block is skipped when it is false. Unset means
	// the block always runs.
	When string `json:"when,omitempty"`
	// ForEach is a CEL expression yielding a list, one observation per
	// element with `item` bound. Unset emits exactly one observation.
	ForEach string `json:"forEach,omitempty"`
	// Subjects are the objects this observation is about, as CEL string
	// expression pairs. At least one is required.
	Subjects []ObserveSubjectSpec `json:"subjects"`
	// Facts maps a fact name to a CEL expression yielding its value,
	// evaluated against the same `item` as Subjects.
	Facts map[string]string `json:"facts"`
}

// ObserveSubjectSpec mirrors v1alpha1.ObserveSubject.
type ObserveSubjectSpec struct {
	// ResourceType is a CEL string expression yielding a SpiceDB definition
	// name (commonly a literal, e.g. `"github_pr"`).
	ResourceType string `json:"resourceType"`
	// ResourceID is a CEL string expression yielding the object id.
	ResourceID string `json:"resourceID"`
}

type Args struct {
	// AllowedFields is the closed set of top-level argument names a call may
	// present. Empty is FAIL-CLOSED (every argument denied), not allow-all —
	// see UnconstrainedArgs.
	AllowedFields []string `json:"allowedFields,omitempty"`
	// UnconstrainedArgs opts a tool out of allowedFields enforcement
	// entirely — for a tool deliberately authored to accept free-form
	// arguments. It MUST be set explicitly: an empty AllowedFields is
	// fail-closed (any arg is denied), not allow-all. See
	// validator.checkAllowedFields.
	UnconstrainedArgs bool `json:"unconstrainedArgs,omitempty"`
	// Constraints are CEL predicates the call's arguments must satisfy.
	Constraints []toolspec.Constraint `json:"constraints,omitempty"`
	// SensitiveFields are argument paths (dotted, array indices allowed) whose
	// values are replaced by a token everywhere they could be surfaced.
	SensitiveFields []string `json:"sensitiveFields,omitempty"`
}

type Deny struct {
	// Effects denies by the tool's MCP annotation snapshot.
	Effects toolspec.DenyEffects `json:"effects,omitempty"`
	// Trust denies by the tool's SEP-1913 trust snapshot.
	Trust DenyTrust `json:"trust,omitempty"`
}

// Effects is the snapshot of MCP tool annotations captured from the
// server's tools/list response. Empty Effects (all false) is the
// well-defined default — servers that emit no annotations yield an
// empty struct. The validator's deny.effects phase enforces a denial
// only when the corresponding flag is true here.
type Effects struct {
	Destructive bool `json:"destructive,omitempty"` // annotations.destructiveHint
	ReadOnly    bool `json:"readOnly,omitempty"`    // annotations.readOnlyHint
	Idempotent  bool `json:"idempotent,omitempty"`  // annotations.idempotentHint
	OpenWorld   bool `json:"openWorld,omitempty"`   // annotations.openWorldHint
}

// Generation holds spec-author metadata used by tooling. Currently:
// TestCases that `oap tools mcp test` replays through the validator.
type Generation struct {
	TestCases []TestCase `json:"testCases,omitempty"`
}

// TestCase is one synthetic invocation the spec author or gen-agent
// recorded. oap tools mcp test runs validator.Check on each and compares
// Decision.Allow to ExpectedAllow.
type TestCase struct {
	// Intent is the human sentence this invocation represents.
	Intent string `json:"intent,omitempty"`
	// ToolName is the tool the synthetic call targets.
	ToolName string `json:"toolName"`
	// Args are the arguments the synthetic call presents.
	Args map[string]any `json:"args,omitempty"`
	// ExpectedAllow is the outcome the author intends.
	ExpectedAllow bool `json:"expectedAllow"`
}

// TransportStreamableHTTP is the only transport supported in v1.
const TransportStreamableHTTP = "streamable-http"

// Trust is a snapshot of SEP-1913 trust + action-security annotations
// captured from the MCP server's tools/list response. Recorded so
// deny.trust.* rules can enforce on it; surfaced by `oap tools mcp check`.
//
// See https://github.com/modelcontextprotocol/modelcontextprotocol/pull/1913
type Trust struct {
	// MaliciousActivityHint records that the server declares it MAY flag
	// malicious activity. Never a static deny — see DenyTrust.
	MaliciousActivityHint bool `json:"maliciousActivityHint,omitempty"`
	// Attribution is who the server credits the tool's behavior to.
	Attribution []string `json:"attribution,omitempty"`
	// InputMetadata is the server's raw declaration about what the call does
	// (outcomes, destination). Nil means the server asserted nothing.
	InputMetadata json.RawMessage `json:"inputMetadata,omitempty"`
	// ReturnMetadata is the server's raw declaration about where the result
	// comes from (source). Nil means the server asserted nothing.
	ReturnMetadata json.RawMessage `json:"returnMetadata,omitempty"`
}

// DenyTrust opts into pre-call denial based on SEP-1913 capability
// declarations. The validator denies when both Tool.Trust meets the
// condition AND the corresponding flag is true. Mirrors DenyEffects.
//
// MaliciousActivityHint is deliberately NOT here: at tools/list time
// it's a capability declaration ("MAY flag malicious activity"), so
// static deny would refuse tools with security awareness — backwards.
// Response-time handling lives in pkg/agent/tool/mcp/dispatch.go.
type DenyTrust struct {
	// Deny when trust.inputMetadata.outcomes contains "irreversible".
	OutcomesIrreversible bool `json:"outcomesIrreversible,omitempty"`
	// Deny when trust.inputMetadata.destination contains "public".
	DestinationPublic bool `json:"destinationPublic,omitempty"`
	// Deny when trust.returnMetadata.source contains "untrustedPublic".
	SourceUntrustedPublic bool `json:"sourceUntrustedPublic,omitempty"`
}
