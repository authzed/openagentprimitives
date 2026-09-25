// pkg/platform/settings/types.go

// Package settings folds the four-tier chain — cluster, namespace, AgentClass,
// AgentSession — into one EffectiveSettings plus the Violations that fold
// produced.
//
// Two kinds of tier field compose differently. A *default* is inherited: the
// nearest tier that sets one wins, and lower tiers may override it freely. A
// *limit* is a ceiling that only ever tightens: the strictest across every tier
// wins, and a lower tier can never widen it. Allowlists intersect, denylists
// union, pinning takes the strongest mode and the highest strength floor.
//
// Resolve is pure — no I/O, no clock, no client. Callers (the AgentSession
// reconciler, the admission webhook, `oap settings`) build Inputs from CRDs, and
// every non-fatal disagreement comes back as a Violation, never as a silent
// adjustment.
package settings

import (
	"time"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Inputs is the normalized input to Resolve. Callers (controllers, webhook)
// populate it from the CRDs. A nil tier pointer means that tier's CR does not
// exist; a nil request pointer means the lower tier omitted that field.
type Inputs struct {
	// Settings tiers, top-down. nil = the CR does not exist.
	Cluster   *v1.SettingsSpec
	Namespace *v1.SettingsSpec

	// Lower-tier concrete requests.
	ClassModel  *v1.ModelConfig  // nil if the AgentClass omits its model
	ClassBudget *v1.BudgetConfig // nil if the AgentClass omits its budget
	ClassAuthz  *v1.AuthzBlock   // the AgentClass authz block (may be nil)

	// Toolkit/MCP the AgentClass references, checked against the allowlists.
	ClassToolkits []string
	ClassMCP      []MCPRequest

	// ClassSkills lists the canonical skill names the AgentClass opts into,
	// checked against the AllowedSkills/DeniedSkills ceilings.
	ClassSkills []string

	// ClassPins lists non-skill dependency declarations (images, MCP servers,
	// CLI toolkits) pre-parsed by the caller via the pinning registry. Skill
	// pins are derived from ClassSkills internally.
	ClassPins []DeclaredPin

	// SessionBudget narrows the class budget. nil for class-only resolution or
	// when the session omits a budget.
	SessionBudget *v1.BudgetConfig

	// RootBudget is the resolved budget of the delegation tree's ROOT session,
	// set only when resolving for a delegated child. It folds in as an
	// additional CEILING on every dimension of the node being resolved — each
	// node is individually capped at no more than the root's budget. This is
	// per-node, not pooled: N siblings can each independently spend up to the
	// root's budget, so the tree's total spend is not bounded by RootBudget
	// alone. True pooled accounting (a shared, decrementing budget across the
	// whole tree) is follow-on work. Nil for a root session.
	RootBudget *v1.BudgetConfig

	// ForSession is true when resolving for an about-to-run AgentSession. It
	// controls whether an unresolvable model is fatal (true) or a deferred,
	// non-fatal note (false, i.e. class-only resolution).
	ForSession bool

	// BundleSandbox carries the two lowest sandbox tiers per bundle name: what
	// the AgentClass's toolBundles entry asks for (tier 3) and what the
	// referenced SpiceboxClass itself declares (tier 4). The caller reads the
	// SpiceboxClass — Resolve performs no I/O.
	BundleSandbox map[string]BundleSandboxInputs
}

// BundleSandboxInputs are one bundle's two lowest-tier sandbox declarations.
// Either may be nil, meaning that tier expresses no preference.
type BundleSandboxInputs struct {
	// FromAgentClass is AgentClass.spec.toolBundles[].sandbox — tier 3.
	FromAgentClass *v1.SandboxBackend
	// FromSpiceboxClass is the referenced SpiceboxClass's spec.sandbox — tier 4.
	FromSpiceboxClass *v1.SandboxBackend
}

// MCPRequest is one MCP server (+ optional tools) referenced by an AgentClass.
type MCPRequest struct {
	Server string
	// Tools are the specific tools the AgentClass uses from Server. An empty
	// list means the class uses ALL of the server's tools — which is denied if
	// the effective allowlist restricts Server to a specific subset.
	Tools []string
}

// DeclaredPin is one dependency an AgentClass opts into, normalized for the
// pinning floor check. Skill declarations are derived from ClassSkills inside
// Resolve; other kinds are pre-parsed by callers via the pinning registry so
// Resolve stays pure (no registry import, no I/O).
type DeclaredPin struct {
	Kind     string // pinning registry kind name
	Name     string // the declared ref / CR name, used for bypass matching
	Strength string // a pinning.Strength value
}

// EffectiveSettings is the resolved, flattened result of folding the 4 tiers.
type EffectiveSettings struct {
	// Model is the chosen model name/provider; APIKey is set only on the
	// bring-your-own and no-catalog paths (catalog tokens use ModelTokenSource).
	Model v1.ModelConfig
	// ModelTokenSource is the catalog token's system-namespace source, or nil.
	ModelTokenSource *v1.NamespacedSecretKeyRef
	// ModelInputPerMTok/ModelOutputPerMTok are the catalog price for the chosen
	// model (USD/MTok), 0 when the catalog carries no price. The runner cost
	// estimator prefers these over its built-in table so its figure matches the
	// admin dashboard (which is catalog-authoritative).
	ModelInputPerMTok  float64
	ModelOutputPerMTok float64
	// ModelRouting is the resolved OpenRouter dynamic-routing preference, nil
	// unless the catalog entry's provider is "openrouter".
	ModelRouting *v1.OpenRouterRouting
	// Budget is the folded per-session cap; a zero dimension is unenforced.
	Budget v1.BudgetConfig
	// Authz are the human-in-the-loop windows; always populated (built-in
	// defaults apply when no tier sets one).
	Authz EffectiveAuthz

	// AllowedToolkits is the toolkit-name ceiling: nil unconstrained, non-nil
	// empty denies every toolkit.
	AllowedToolkits []string
	// AllowedMCP is the MCP server (and per-server tool) ceiling: nil
	// unconstrained, non-nil empty denies every server.
	AllowedMCP []v1.AllowedMCPServer

	// AllowedSkills is the union of allow-skill patterns across constraining tiers
	// (nil = unconstrained). Informational; enforcement is via checkSkills.
	AllowedSkills []string
	// DeniedSkills is the union of deny-skill patterns across tiers.
	DeniedSkills []string
	// Pinning preserves the per-tier pinning policies. nil when neither tier
	// set one.
	Pinning *v1.EffectivePinning

	// ToolGuard preserves the per-tier tool-guard Defaults policies plus the
	// folded (strictest) ceiling. nil when no tier set either. Rule
	// resolution happens in the runner (pkg/authz/toolguard.ResolvePolicy).
	ToolGuard *v1.EffectiveToolGuard

	// ContentInspectors is the union of content-guard inspector configs across
	// all tiers (ceiling semantics: lower tiers ADD, never remove). Empty when
	// no tier set ContentInspectors.
	ContentInspectors []v1.ContentInspectorConfig

	// Provenance maps a resolved field name to the tier that supplied it:
	// "cluster", "namespace", "class", "session", or "clamped".
	Provenance map[string]string

	// ReportSessionCost is the resolved end-of-session cost-estimate toggle
	// (default true). Read by the runner to gate the cost reporter hook.
	ReportSessionCost bool

	// NativeFileHandling is the resolved Tier-2 provider-native file handling
	// grant (default false). Security-sensitive: cluster must grant, namespace
	// may only restrict. Read by the runner to set RunnerEnv.NativeFileOptIn.
	NativeFileHandling bool

	// RequireSubagentDigestPins is the resolved delegation requirement
	// (default false): a ratchet, not a grant — any tier setting true wins,
	// and no lower tier can relax one already set. Read by the
	// SubagentRequest controller to refuse delegation through an unpinned
	// roster entry.
	RequireSubagentDigestPins bool

	// Sandbox is the resolved backend per bundle name. Every bundle present in
	// Inputs.BundleSandbox appears here, with Kind always non-empty (it falls
	// back to v1.DefaultSandboxKind).
	Sandbox map[string]v1.SandboxBackend
	// AllowedSandboxKinds is the intersection ceiling across constraining
	// tiers. nil means unconstrained; a non-nil empty slice denies everything.
	AllowedSandboxKinds []string
	// RequireStandingFor is the sorted, deduplicated union of every tier's
	// RequireStandingFor — the admin veto over SpiceDBResource.Standing. A
	// resource type named here resolves to `required` standing regardless of
	// what its schema fragment declares.
	RequireStandingFor []string
}

// EffectiveAuthz are the resolved authz timeouts.
type EffectiveAuthz struct {
	// ApprovalTimeout is how long an approval may wait on a human before the
	// gate's timeout policy fires.
	ApprovalTimeout time.Duration
	// InformationLeakageApprovalTTL is how long an approved leakage share may be
	// reused before the agent must ask again.
	InformationLeakageApprovalTTL time.Duration
	// ScopeMaxLLMLatencyMs budgets the cold-start scope extractor's LLM call.
	ScopeMaxLLMLatencyMs int32

	// PlanGate is the resolved plan-gate config — the class value (or the
	// nearest tier default) clamped up to the strictest floor. Always non-nil
	// after resolveAuthz. See the v1alpha1 copy for why the gate must read
	// this rather than AgentClass.spec.authz.
	PlanGate *v1.PlanGateConfig

	// Metaagent is the resolved metaagent trigger. Never nil after resolution:
	// an absent setting resolves to mention, the narrowest value, rather than
	// to a nil every reader has to interpret.
	Metaagent *v1.MetaagentConfig
}

// Violation is a single resolution finding. Fatal violations drive
// SettingsAccepted=False; non-fatal ones become warning conditions.
type Violation struct {
	Reason  string
	Message string
	Fatal   bool
}

// Resolution reason constants.
const (
	ReasonAuthzClamped            = "AuthzClamped"            // non-fatal — an approval window exceeded a Limits.Authz ceiling
	ReasonBudgetClamped           = "BudgetClamped"           // non-fatal
	ReasonModelForbidden          = "ModelForbidden"          // fatal — name in deniedModels
	ReasonModelNotInCatalog       = "ModelNotInCatalog"       // fatal — name absent from the effective catalog
	ReasonModelOverrideNotAllowed = "ModelOverrideNotAllowed" // fatal — BYO token without allowModelOverride
	ReasonToolkitNotAllowed       = "ToolkitNotAllowed"       // fatal
	ReasonMCPServerNotAllowed     = "MCPServerNotAllowed"     // fatal
	ReasonMCPToolNotAllowed       = "MCPToolNotAllowed"       // fatal
	ReasonModelMissingModel       = "ModelMissingModel"       // fatal only when ForSession
	ReasonModelMissingCredential  = "ModelMissingCredential"  // fatal only when ForSession
	ReasonSkillNotAllowed         = "SkillNotAllowed"         // fatal
	ReasonSkillDenied             = "SkillDenied"             // fatal
	ReasonSkillPinningRequired    = "SkillPinningRequired"    // fatal
	ReasonSkillRolling            = "SkillRolling"            // non-fatal warning
	ReasonPinningRequired         = "PinningRequired"         // fatal when mode=block, else warning
	ReasonPinRolling              = "PinRolling"              // non-fatal warning for unpinned non-skill kinds; emitted once those kinds land (reserved)
	// ReasonToolGuardRateBoundUnenforceable is non-fatal — a toolGuard
	// sliding-window bound was authored with a missing or non-positive half, so
	// it caps nothing (see v1.CallRate) and was dropped. Covers both surfaces
	// carrying the pair: a Limits.ToolGuard ceiling bound (dropped by the fold)
	// and a Defaults.ToolGuard rule's rateLimit (dropped by the mirror, which
	// must not hand the apiserver a status object its own CRD rejects).
	//
	// Warned rather than refused because the OTHER bounds still apply, and
	// because AgentSettings is namespace-writable: a fatal here would let one
	// malformed bound wedge every session in the namespace.
	ReasonToolGuardRateBoundUnenforceable = "ToolGuardRateBoundUnenforceable"
)
