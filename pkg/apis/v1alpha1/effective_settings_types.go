package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// EffectiveSettings is the resolved 4-tier settings snapshot stamped onto
// AgentClass/AgentSession status. It mirrors pkg/platform/settings.EffectiveSettings in a
// CRD-serializable shape (metav1.Duration instead of time.Duration).
type EffectiveSettings struct {
	// Model is the resolved model the session runs against.
	// +optional
	Model ModelConfig `json:"model,omitempty"`
	// ModelTokenSource is the resolved central token location (system namespace)
	// when the effective model came from the catalog. nil for bring-your-own /
	// legacy. The operator materializes this into the per-session Secret.
	// +optional
	ModelTokenSource *NamespacedSecretKeyRef `json:"modelTokenSource,omitempty"`
	// ModelInputPerMTok/ModelOutputPerMTok are the resolved catalog price for the
	// chosen model (USD/MTok), 0 when the catalog carries no price. The runner
	// cost estimator prefers these over the built-in table so its figure matches
	// the admin dashboard (which is catalog-authoritative).
	// +optional
	ModelInputPerMTok float64 `json:"modelInputPerMTok,omitempty"`
	// ModelOutputPerMTok is the output half of the same catalog price.
	// +optional
	ModelOutputPerMTok float64 `json:"modelOutputPerMTok,omitempty"`
	// ModelRouting is the effective OpenRouter dynamic-routing preference: the
	// catalog entry's, with the AgentClass's routingMetadata merged over it.
	// Only set when Model.Provider == "openrouter".
	// +optional
	ModelRouting *OpenRouterRouting `json:"modelRouting,omitempty"`
	// Budget is the resolved per-session budget, clamped to every tier ceiling.
	// +optional
	Budget BudgetConfig `json:"budget,omitempty"`
	// Authz is the resolved set of human-in-the-loop timeouts.
	// +optional
	Authz EffectiveAuthz `json:"authz,omitempty"`
	// AllowedToolkits is the effective toolkit ceiling; nil is unconstrained.
	// +optional
	AllowedToolkits []string `json:"allowedToolkits,omitempty"`
	// AllowedMCP is the effective MCPServer ceiling; nil is unconstrained.
	// +optional
	AllowedMCP []AllowedMCPServer `json:"allowedMCP,omitempty"`
	// AllowedSkills is the union of allow-skill patterns across constraining
	// tiers (nil = unconstrained). Informational; the gate is the resolver.
	// +optional
	AllowedSkills []string `json:"allowedSkills,omitempty"`
	// DeniedSkills is the union of deny-skill patterns across tiers.
	// +optional
	DeniedSkills []string `json:"deniedSkills,omitempty"`
	// Pinning preserves the per-tier pinning policies. Per-item evaluation
	// happens via settings.PinRequirementFor — bypasses are tier-scoped, so
	// the policies cannot be pre-folded.
	// +optional
	Pinning *EffectivePinning `json:"pinning,omitempty"`
	// ToolGuard preserves the per-tier tool-guard policies + folded ceiling.
	// Per-tool rule resolution happens in the runner at session start.
	// +optional
	ToolGuard *EffectiveToolGuard `json:"toolGuard,omitempty"`
	// ContentInspectors is the resolved union of content-guard inspectors across
	// tiers (ceiling: lower tiers add, never weaken). Empty ⇒ no inspection.
	// +optional
	ContentInspectors []ContentInspectorConfig `json:"contentInspectors,omitempty"`
	// Provenance maps a resolved field to the tier that supplied it
	// (cluster|namespace|class|session|clamped).
	// +optional
	Provenance map[string]string `json:"provenance,omitempty"`
	// ReportSessionCost is the resolved end-of-session cost-estimate toggle.
	ReportSessionCost bool `json:"reportSessionCost"`
	// NativeFileHandling is the resolved Tier-2 provider-native file handling
	// grant (security-sensitive; default false). See SettingsLimits.NativeFileHandling.
	NativeFileHandling bool `json:"nativeFileHandling"`
	// RequireSubagentDigestPins is the resolved delegation requirement
	// (default false): when true, the SubagentRequest controller refuses
	// delegation through any unpinned roster entry. See
	// SettingsLimits.RequireSubagentDigestPins.
	RequireSubagentDigestPins bool `json:"requireSubagentDigestPins"`
	// Sandbox is the resolved sandbox backend per tool-bundle name. Recorded so
	// `oap` can show which backend a session landed on, and Provenance says which
	// tier chose it.
	// +optional
	Sandbox map[string]SandboxBackend `json:"sandbox,omitempty"`
	// AllowedSandboxKinds is the effective ceiling across constraining tiers.
	// Empty means unconstrained.
	// +optional
	AllowedSandboxKinds []string `json:"allowedSandboxKinds,omitempty"`

	// RequireStandingFor is the union of every tier's RequireStandingFor,
	// sorted and deduplicated. A resource type named here resolves to
	// `required` standing regardless of what its schema fragment declares.
	// +optional
	// +listType=atomic
	RequireStandingFor []string `json:"requireStandingFor,omitempty"`
}

// EffectiveAuthz is the resolved authz timeout set.
type EffectiveAuthz struct {
	// ApprovalTimeout is how long a decision may wait for a human before the
	// gate's timeout policy fires.
	// +optional
	ApprovalTimeout metav1.Duration `json:"approvalTimeout,omitempty"`
	// InformationLeakageApprovalTTL is how long an approved share may be reused
	// before the agent must ask again.
	// +optional
	InformationLeakageApprovalTTL metav1.Duration `json:"informationLeakageApprovalTTL,omitempty"`
	// ScopeMaxLLMLatencyMs bounds the cold-start scope extractor's LLM call, in
	// milliseconds. A latency budget, not a window in which authority is held.
	// +optional
	ScopeMaxLLMLatencyMs int32 `json:"scopeMaxLlmLatencyMs,omitempty"`

	// PlanGate is the resolved plan-gate config: the class value (or the
	// nearest tier default) clamped up to the strictest MinPlanGateMode any
	// tier declares. Always non-nil after resolution.
	//
	// This is the value the gate READS. Unlike every other authz setting —
	// which the runtime reads straight off AgentClass.spec.authz — the plan
	// gate must go through the resolved path, or a class could opt out past a
	// cluster floor by being the only value anyone consults.
	// +optional
	PlanGate *PlanGateConfig `json:"planGate,omitempty"`

	// Metaagent is the resolved metaagent trigger: the class value, else the
	// nearest tier default, else mention. Never nil after resolution — an
	// absent setting resolves to the narrowest value rather than to a nil the
	// reader has to interpret.
	// +optional
	Metaagent *MetaagentConfig `json:"metaagent,omitempty"`
}

// planGateModeDisabled is the one mode in PlanGateConfig.Mode's enum that turns
// the gate off. The literal lives here, beside the enum marker that defines it,
// so no consumer has to spell it.
const planGateModeDisabled = "disabled"

// ResolvedPlanGateMode returns the resolved plan-gate mode, or "" when the
// session has no resolved settings or no plan-gate block.
//
// This, and deliberately NOT AgentClass.spec.authz.planGate.Mode. Every other
// authz setting is read straight off the class spec, but the plan-gate mode is
// ceiling-clamped in the settings fold: a class may make itself stricter, and
// may only go below a tier floor when no floor is set. Reading the class spec
// would consult the UNCLAMPED value and let a class opt out past a cluster
// floor by being the only value anyone looks at.
//
// Nil-safe on both hops. Callers hold an *EffectiveSettings read off
// AgentSession.status, which is empty until the reconciler has resolved it, and
// PlanGate is nil for a session resolved before the field existed. A missing
// snapshot resolving to "" is the correct failure direction: the gate is off by
// default, and a caller that somehow has no resolved settings has larger
// problems than one absent hook.
func (es *EffectiveSettings) ResolvedPlanGateMode() string {
	if es == nil || es.Authz.PlanGate == nil {
		return ""
	}
	return es.Authz.PlanGate.Mode
}

// PlanGateActive reports whether the plan gate RUNS for this session.
//
// A method rather than a comparison each caller writes, because the answer gates
// which meta tools a session is offered — select_phase and complete_phase exist
// only when there is a frozen plan to index into — and a caller that forgets to
// ask gets the fail-closed answer in silence. `oap session capture` did exactly
// that: it left capability.RunnerEnv.PlanGateActive unset, so every captured
// plan-gated session had select_phase missing from its meta-tool list and was
// refused as an unreplayable sandbox call. The runner and the e2e in-process
// factory had each transcribed the same expression separately; a third copy in
// the CLI is what this method exists to prevent.
//
// "" (unresolved) counts as inactive, matching the runner: a session whose
// settings have not resolved has no frozen plan to index into either.
func (es *EffectiveSettings) PlanGateActive() bool {
	mode := es.ResolvedPlanGateMode()
	return mode != "" && mode != planGateModeDisabled
}
