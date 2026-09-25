// Package authz is the single home for every runtime authorization decision in
// agentprimitives. Consumers (runner, channelsd, operator, authzd) reach it
// through the engine.Engine interface rather than calling in directly.
package authz

import (
	"errors"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz/precondition"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
)

// Outcome is the resolved verdict of any Check* method.
type Outcome int

const (
	OutcomeAllowed Outcome = iota
	OutcomeDenied
)

// String returns a human-readable form for test failure messages.
func (o Outcome) String() string {
	switch o {
	case OutcomeAllowed:
		return "Allowed"
	case OutcomeDenied:
		return "Denied"
	default:
		return "?"
	}
}

// StateImpact describes the policy mode for a tool / subcommand.
type StateImpact string

const (
	// Stateless: tool touches nothing observable; no Check, no
	// provenance taint. Author has explicitly opted in.
	Stateless StateImpact = "stateless"
	// Passthrough: tool touches state but doesn't map to a SpiceDB
	// resource. No Check today; future provenance treats result as
	// tainted (we can't say who can view what came back).
	Passthrough StateImpact = "passthrough"
	// Readonly: Check is the read permission on the resource being
	// fetched. Tool dispatches only if the subject has that perm.
	Readonly StateImpact = "readonly"
	// Readwrite: Check is the write permission on the resource being
	// mutated. Tool dispatches only if the subject has that perm.
	Readwrite StateImpact = "readwrite"
	// External: Check is required AND the call always routes through
	// the approval flow — every call is re-approved.
	External StateImpact = "external"
)

// CheckRequired returns true when StateImpact requires a non-nil
// Check (Readonly / Readwrite / External).
func (s StateImpact) CheckRequired() bool {
	switch s {
	case Readonly, Readwrite, External:
		return true
	}
	return false
}

// EnforceMode controls whether SpiceDB-deny is final regardless of
// the AgentClass's toolAuthMode. Defaults to Inherit.
type EnforceMode string

const (
	// EnforceInherit honors AgentClass.spec.toolAuthMode (slice-1
	// behavior). Permissive AgentClasses log denies and proceed.
	EnforceInherit EnforceMode = "inherit"
	// EnforceAlways denies on SpiceDB-deny even under permissive
	// toolAuthMode. The slice-4 "no bypass" escape hatch for
	// privacy-sensitive variant checks.
	EnforceAlways EnforceMode = "always"
)

// PermissionCheck names the SpiceDB resource and permission to
// evaluate. Required when StateImpact is Readonly / Readwrite /
// External; forbidden when Stateless / Passthrough.
type PermissionCheck struct {
	// ResourceType is the SpiceDB definition the Check runs against
	// (e.g. "github_repo", "channel"). Static — no template interpolation.
	//
	// Pattern-validated at admission because this value is a component of a
	// permsurface.Handle, which appears in approved plan ceilings and audit
	// records. Mirrors BoundEntityType.ResourceType.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*(/[a-z][a-z0-9_]*)*$`
	ResourceType string `json:"resourceType"`

	// Permission is the SpiceDB permission (or relation) name to
	// Check (e.g. "read", "write", "admin"). Static.
	//
	// Pattern-validated at admission for the same reason as ResourceType.
	// Mirrors BoundEntityType.Permission.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*$`
	Permission string `json:"permission"`

	// ResourceIDTemplate is a string with {arg} placeholders that
	// interpolate from the tool call's args (e.g. "{owner}/{repo}").
	// Resolved at runtime by ResolveTemplate; validated at
	// AgentClass-reconcile time so every {arg} matches a parameter
	// the tool declares.
	ResourceIDTemplate string `json:"resourceIDTemplate,omitempty"`

	// ResourceIDExpr is a CEL string expression evaluated against
	// `args` that yields the SpiceDB resource id. Exactly one of
	// ResourceIDTemplate or ResourceIDExpr must be set: the template
	// form for simple `{arg}` interpolation, the Expr form for
	// nested-arg extraction.
	// +optional
	ResourceIDExpr string `json:"resourceIDExpr,omitempty"`

	// ResourceIDTransforms names registered transforms applied IN
	// ORDER to the resolved template string before SpiceDB sees it:
	// lowercase, remove_spaces, spicedb_object_id, basename, sha256.
	// See transforms.go.
	ResourceIDTransforms []string `json:"resourceIDTransforms,omitempty"`

	// ResourceIDHint is shown to the AGENT when the resource id cannot be
	// resolved from the call's arguments.
	//
	// That failure is usually the caller's to fix — it named no resource, or
	// named it in a form the check cannot read — and the agent is the only
	// party who can retry. Without a hint the message is the raw resolution
	// error prefixed "internal:", which reads as a system fault and tells it
	// not to bother.
	//
	// Written by whoever authors the check, because only they know what the
	// call should have looked like. Example, for a git push whose remote must
	// be a URL so the repository can be authorized: "name the remote as a full
	// https:// URL, not a shorthand like `origin`".
	// +optional
	ResourceIDHint string `json:"resourceIDHint,omitempty"`

	// GrantBindsArgs, when set, restricts the slice-2 grant's
	// arguments_hash caveat binding to only the listed top-level arg
	// keys. Default (nil/empty) hashes the full arg map, meaning the
	// grant satisfies only the exact same call. Setting it to e.g.
	// ["repo"] lets a single approval cover every subsequent call
	// against the same repo regardless of other args (pr number,
	// etc.) — useful for readonly tools where the resource is the
	// load-bearing input.
	//
	// The hash is an HMAC-SHA256 under the per-session args-hash key
	// minted by the AgentSession reconciler into the per-session
	// Secret and read by the runner at startup. Caveat contexts
	// therefore cannot be predicted or minted outside the
	// controller+runner trust domain — neither the agent nor a
	// confused deputy with SpiceDB write access can forge a valid
	// binding for arguments that were never requested. Note: hashes
	// of previously-requested (including denied) calls remain
	// observable in session status and channel payloads, so a
	// SpiceDB-write attacker could still replay those; the keying
	// narrows that surface to exactly the calls a human has already
	// seen. See pkg/authz/guardian/grants.ArgsHashFiltered for the
	// filtering contract.
	//
	// NOTE: the hash no longer BINDS the grant. A slot grant is keyed on
	// (instance, permission), which is what makes it safe to reuse across
	// calls: a grant shaped for one permission cannot be spent on another
	// against the same id. The filtered hash is still computed and published
	// on the approval request (ToolApprovalDetails.ArgsHash) so an approver —
	// and the audit log — can see exactly which call was asked about.
	// +optional
	GrantBindsArgs []string `json:"grantBindsArgs,omitempty"`

	// ExtractionPrompt, when set, is an English fragment the runner
	// includes in the extraction LLM's system prompt for this resource
	// type. Tool spec authors write this once per toolspec/MCPServer
	// file. The runner aggregates ExtractionPrompts across all tools
	// whose Check targets the same resourceType (dedup'd, concatenated).
	// The AgentClass's BoundEntityType.ExtractionPrompt overrides
	// entirely if set.
	// +optional
	ExtractionPrompt string `json:"extractionPrompt,omitempty"`

	// EnforceMode controls deny-finality under permissive toolAuthMode.
	// Empty → EnforceInherit (slice-1 default).
	// +optional
	EnforceMode EnforceMode `json:"enforceMode,omitempty"`

	// RouteViaSessionGrant is GONE. It existed because the approval flow wrote
	// its grant tuple from the SESSION to the resource, so the direct
	// `<resourceType>:<id>#<permission>@user:<subject>` Check could never pass
	// post-approval — the walk re-checked the permission for a user who lacked
	// it by construction. Routing around that check required a wildcard leaf in
	// the resource's permission expression, and the MCPServer controller had to
	// police the invariant at admission.
	//
	// Approvals now write the tuple on the RESOURCE pointing at the session, so
	// the tool's NATURAL Check resolves the session's member set and passes
	// per-requester. There is nothing to route around, no wildcard to require,
	// and no invariant to enforce. A spec that still sets the field is rejected
	// by the CRD rather than silently ignored.
}

// Permission carries StateImpact + optional PermissionCheck.
type Permission struct {
	StateImpact StateImpact      `json:"stateImpact"`
	Check       *PermissionCheck `json:"check,omitempty"`

	// ToolName lets Layer 2's CheckScope evaluate the tool allow/deny
	// axis. Wire from the registered tool name. Empty string skips
	// the Layer 2 tool check (existing behavior).
	ToolName string `json:"toolName,omitempty"`
}

// PermissionVariant is one branch of a conditional Permission. The
// dispatcher evaluates each variant's When against the tool-call
// args in order; the first that returns true wins.
type PermissionVariant struct {
	// When is a CEL boolean expression with `args` in scope.
	When string `json:"when"`
	// Check is the Permission applied when When matches. Same shape
	// as the slice-1 singular Permission's Check, plus the new
	// EnforceMode and ResourceIDExpr fields.
	Check Permission `json:"check"`
}

// Result is what a Checker returns. Outcome != Allowed produces a
// user-visible Message that the runner uses as the IsError tool
// result content.
type Result struct {
	Outcome Outcome
	Message string
	// EnforceOverride, when EnforceAlways, tells the runner this
	// Result's Outcome must be honored regardless of the
	// AgentClass's toolAuthMode (i.e. permissive cannot bypass).
	// Empty defaults to EnforceInherit (slice-1 behavior). Stamped
	// by Check when the resolved PermissionCheck has
	// EnforceMode == EnforceAlways.
	EnforceOverride EnforceMode

	// UnresolvedResource marks a denial that happened because the CALL never
	// identified the resource it acts on — a guard expression that refused it,
	// or a template whose argument was absent — rather than because the subject
	// lacks the permission.
	//
	// The distinction decides who is asked next. An ordinary denial escalates to
	// a human approval card, which is right: a person can grant what is missing.
	// This one cannot be escalated. The card would name the resource type with
	// an EMPTY object id, no human can tell what they are approving, and
	// approving it changes nothing — the retried call fails at resolution again,
	// before SpiceDB is consulted. The only party who can fix it is the agent,
	// by re-issuing the call with the argument supplied (see ResourceIDHint),
	// so the denial goes straight back as the tool result.
	UnresolvedResource bool

	// Precondition, when non-nil, says this denial is explained by a SLOT
	// PRECONDITION: the class declared a gate over the instance this call
	// names, that gate did not say yes, so the candidate never entered its
	// slot and the check could not have passed.
	//
	// "Did not say yes" covers two shapes, distinguished by the struct's
	// Unevaluatable field: a computed verdict (Refused / Undetermined), and a
	// gate that could not be evaluated at all. Both deny and both stamp
	// EnforceAlways, because a gate that cannot read its facts is a gate that
	// said no; only the sentence handed to the agent differs.
	//
	// A pointer, and the discriminator is the pointer rather than the verdict
	// it carries. precondition.Undetermined is deliberately the ZERO Verdict —
	// a verdict nobody set must be the one that never binds — so a bare Verdict
	// field could not tell "no precondition governs this call" from "some fact
	// has not been observed yet", and every ordinary permission denial in the
	// system would read as the latter.
	//
	// Set by the dispatch-time recomputation, never by a backend Checker: the
	// verdict is a pure function of durable facts and a declared expression, so
	// it is recomputed rather than cached. A cache would be a second source of
	// truth that can disagree with the bind-time filter, and the disagreement
	// would be invisible — the stored verdict would be what explains the denial
	// while the live one is what caused it.
	Precondition *PreconditionDenial
}

// PreconditionDenial is the recomputed slot-precondition verdict behind a
// denial, together with the message its author wrote for that verdict — or,
// when Unevaluatable is set, the report that no verdict could be computed at
// all.
//
// Verdict is never Satisfied: a satisfied precondition explains no denial, and
// the recomputation returns nil rather than constructing one of these.
type PreconditionDenial struct {
	// Verdict is what the precondition evaluated to for the instance this call
	// named — Refused (the facts decided against it) or Undetermined (some
	// referenced fact has not been recorded). Meaningless when Unevaluatable
	// is set: read that field first.
	Verdict precondition.Verdict
	// Message is what the agent is told: the author's undeterminedHint or
	// refusalMessage, selected by Verdict — or, for an Unevaluatable one, the
	// sentence naming what went wrong, which claims no verdict.
	Message string
	// Unevaluatable is set when the verdict could not be computed at all — an
	// unreadable fact store, an id that would not resolve, a predicate that fails
	// only at eval. It is NOT a verdict: Verdict stays zero (Undetermined) and
	// must not be read as one. It exists so the caller can tell "could not look"
	// apart from "no gate here", because those two are otherwise the same nil and
	// the difference decides whether a permissive-mode call proceeds.
	Unevaluatable bool
	// Approvers are the subject-set expressions the waiver card for a Refused
	// verdict routes to, carried verbatim from the refusing precondition.Rule.
	// Empty means the slot's resolved standing decides (approverSetFor); a
	// non-empty set names the risk-answerer directly, which a userless session's
	// empty resource-owner set often needs. Never set on an Unevaluatable denial:
	// no rule decided, so there is nobody a rule named to route to.
	Approvers []string
}

// IsError returns true when the runner should treat the result as a
// tool-call error.
func (r Result) IsError() bool { return r.Outcome != OutcomeAllowed }

// SessionGrantCheck, when set on Inputs, asks the backend for a second
// decision against the per-session grant recorded for
// <AgentSessionRef> bound to <ArgumentsHash>. Used after slice-2
// approval so the same call (denied at base permission) passes via the
// grant.
type SessionGrantCheck struct {
	AgentSessionRef string
	ArgumentsHash   string
}

// Inputs collects the runtime parameters a tool-call Check needs. Every
// field is backend-neutral: the SpiceDB connection and its ZedToken
// freshness cache live on the backend Checker
// (pkg/authz/spicedb/toolcheck.Checker), not here, so a second backend
// could satisfy Checker without a new Inputs shape.
type Inputs struct {
	Args     map[string]any
	Subject  string   // single-subject mode (currentRequester or startedBy)
	Subjects []string // both-mode (set when AgentClass.spec.toolAuthSubject="both")

	// AgentSessionRef is "<namespace>/<name>" of the session making this call,
	// set so an EXTERNAL permission can be satisfied by a slot grant that session
	// already holds.
	//
	// External denies outright without it, which is the safe default: the grant
	// leg is asked about the SESSION (agentsession:<ns>/<name>), and with no
	// session there is nothing to ask. Deliberately NOT the composed permission,
	// which is `slot_grant_<p>->interact + owner` and cannot tell "a human
	// approved this instance for this session" from "the requester owns it".
	AgentSessionRef string

	// SlotResourceTypes are the resource types the AgentClass declares in
	// spec.authz.slots — class-level, not plan-level, so it is the same list
	// whether or not the plan gate is on.
	//
	// An External check consults the slot-grant leg only for a type named here.
	// The list is the difference between "no human has approved this instance"
	// and "this type has no instance axis at all": the grant relation only
	// exists in the composed schema for a declared slot, so asking about it on
	// any other type is a call that can only error.
	SlotResourceTypes []string

	// SessionGrantCheck, when non-nil, triggers a fallback grant lookup
	// on the agentsession after a base-permission denial — used after
	// slice-2 approval to re-validate the same call against the JIT
	// grant. nil skips the fallback (slice-1 behavior).
	SessionGrantCheck *SessionGrantCheck

	// RequireFreshest forces a FullyConsistent Check instead of the
	// MinimizeLatency / AtLeastAsFresh default. Set it on a path that must
	// observe a write another process just made — the post-approval re-check
	// is the one caller today. Do NOT set it on ordinary per-call checks: it
	// costs a quorum read on every dispatch.
	RequireFreshest bool

	// SessionScope (Layer 2) — evaluated by CheckScope before any
	// backend Check. Zero value (empty Scope) is a pass-through (no
	// narrowing); a populated scope can deny on tool allow/deny and on arg
	// constraints. Wire from sessionscope.Get.
	//
	// It canNOT deny on resource-not-in-scope: that axis lives in
	// scope.CheckScopeWithRefs, which has no caller, so SessionScope.Resources
	// is inert here. See the NOT WIRED note on CheckScopeWithRefs.
	SessionScope scope.Scope
}

// SessionRef is the canonical (namespace, name) coordinate for an
// AgentSession. Used wherever the Engine talks about a specific session
// without pulling in pkg/apis types.
type SessionRef struct {
	Namespace string
	Name      string
}

func (r SessionRef) String() string { return r.Namespace + "/" + r.Name }

// ResourceRef is a generic (kind, id) used by information-flow checks.
// Empty in this PR; the CheckInformationFlow stub takes it for forward-shape
// commitment.
type ResourceRef struct {
	Kind string
	ID   string
}

// MCPToolRef forward-declared shell. Fields land with CheckMCPTrust in a
// follow-on PR; the empty struct keeps the Engine interface compilable today.
type MCPToolRef struct{}

// Relation is a SpiceDB tuple shape used by Grant / Revoke.
type Relation struct {
	ResourceType string
	ResourceID   string
	Relation     string
	SubjectType  string
	SubjectID    string
	SubjectRel   string
	// ExpiresAt stamps the tuple's expiry. Zero writes no expiry, which the
	// schema accepts only for relations that do not declare `with expiration`.
	ExpiresAt time.Time
}

// ErrNotYetMigrated is returned by Engine methods whose implementation lands
// in a follow-on PR. Callers must NOT invoke these methods in this PR; tests
// guard against accidental invocation.
var ErrNotYetMigrated = errors.New("authz: not yet migrated to pkg/authz")
