package v1alpha1

import (
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Well-known names for the singleton settings resources. The settings webhook
// enforces that only these names may exist; the resolver and controllers look
// them up by these names.
const (
	ClusterAgentSettingsName = "cluster"
	AgentSettingsName        = "default"
)

// SettingsSpec is the spec shared by ClusterAgentSettings and AgentSettings.
// It carries two field groups with opposite merge semantics: Limits (ceilings
// that narrow downward via min/intersection) and Defaults (fallbacks used when
// a lower tier omits the field).
type SettingsSpec struct {
	// Limits are governance ceilings. Absent = this tier imposes no ceiling.
	// +optional
	Limits *SettingsLimits `json:"limits,omitempty"`

	// Defaults are fallback values used when a lower tier omits the field.
	// +optional
	Defaults *SettingsDefaults `json:"defaults,omitempty"`

	// ModelCatalog is the registry of permitted models — the model allowlist.
	// The cluster tier authors entries with central TokenRefs and marks one
	// Default; lower tiers may list name-only entries to narrow the usable set
	// (intersection).
	// +optional
	ModelCatalog *[]ModelCatalogEntry `json:"modelCatalog,omitempty"`

	// ClassUserPreferences carries admin globals for classes' userPreferences,
	// keyed by class name (same namespace), then preference key. NAMESPACE
	// TIER ONLY: the ClusterAgentSettings webhook rejects a non-empty value —
	// namespaced class names make a cluster-tier global ambiguous.
	// +optional
	ClassUserPreferences map[string]map[string]PreferenceGlobal `json:"classUserPreferences,omitempty"`
}

// SettingsLimits are the ceiling/allowlist fields. They narrow downward: budget
// dimensions via min(), allowlists via intersection.
type SettingsLimits struct {
	// Budget caps the agent budget dimensions. Each dimension is optional.
	// +optional
	Budget *SettingsBudgetCeiling `json:"budget,omitempty"`

	// Authz caps the human-in-the-loop approval windows. Ceiling semantics:
	// each field is a MAXIMUM, the shortest across tiers wins, and a lower tier
	// — including the AgentClass — may ask for a shorter window but never a
	// longer one. Distinct from Defaults.Authz, which merely supplies a value
	// the class is free to override.
	// +optional
	Authz *SettingsAuthzCeiling `json:"authz,omitempty"`

	// DeniedModels rejects catalog models by name even when present in the
	// catalog. UNION across tiers (a deny anywhere wins). Same pattern as
	// DeniedSkills.
	// +optional
	DeniedModels []string `json:"deniedModels,omitempty"`

	// AllowModelOverride permits an AgentClass to bring its own model apiKey
	// (instead of referencing the catalog). Top-down: the cluster must grant it;
	// lower tiers can only further restrict. nil/false ⇒ catalog-only.
	// +optional
	AllowModelOverride *bool `json:"allowModelOverride,omitempty"`

	// MinPlanGateMode is the plan-gate strictness an AgentClass may not go
	// below. Distinct from DefaultAuthz.PlanGate, which only says what a class
	// gets when it declares nothing: a class may freely declare a laxer mode
	// than the default, and this is what forbids it.
	//
	// Ordered disabled < logging < enforcing. Tiers ratchet UP only — a
	// namespace declaring a laxer floor than the cluster's does not loosen it.
	// Empty ⇒ no floor.
	// +kubebuilder:validation:Enum=disabled;logging;enforcing
	// +optional
	MinPlanGateMode string `json:"minPlanGateMode,omitempty"`

	// AllowedToolkits restricts which SpiceboxToolkit names may be used.
	// Same tri-state semantics as AllowedMCPServers.
	// +optional
	AllowedToolkits *[]string `json:"allowedToolkits,omitempty"`

	// AllowedMCPServers restricts which MCPServer CRs (by name) and, optionally,
	// which of their tools may be used. nil = no constraint; non-nil empty =
	// deny-all.
	// +optional
	AllowedMCPServers *[]AllowedMCPServer `json:"allowedMCPServers,omitempty"`

	// AllowedSkills restricts which skills (by canonical name) may be used.
	// Same tri-state semantics as AllowedMCPServers. Entries may be exact canonical
	// names or trailing-wildcard patterns ("github.com/org/**", "repo//x@*").
	// The effective ceiling requires a match in every constraining tier.
	// +optional
	AllowedSkills *[]string `json:"allowedSkills,omitempty"`

	// DeniedSkills lists skill patterns that are rejected even if allowed. The
	// effective deny set is the UNION across tiers (a deny anywhere wins). Same
	// pattern syntax as AllowedSkills.
	// +optional
	DeniedSkills []string `json:"deniedSkills,omitempty"`

	// Pinning is the unified per-kind dependency-pinning ceiling: minimum pin
	// strengths and enforcement modes per kind, plus tier-scoped bypasses.
	// +optional
	Pinning *PinningPolicy `json:"pinning,omitempty"`

	// ToolGuard is the hard ceiling on tool circuit-breaker / rate-limit
	// policy: bounds lower tiers cannot escape (strictest across tiers).
	// +optional
	ToolGuard *ToolGuardCeiling `json:"toolGuard,omitempty"`

	// ContentInspectors are content-guard plugins (referenced by registry ID)
	// enforced on tool I/O. Ceiling semantics: lower tiers ADD inspectors but
	// cannot remove or weaken a higher tier's. Default-off: nil ⇒ no inspection.
	// +optional
	ContentInspectors *[]ContentInspectorConfig `json:"contentInspectors,omitempty"`

	// NativeFileHandling is a security-sensitive Tier-2 grant: it lets the
	// files modality route large artifact bytes through the provider's
	// code-execution sandbox instead of the Tier-1 fetch_artifact tool
	// (content-guard still inspects at the bridge). Top-down: the cluster must
	// grant it; lower tiers can only further restrict. nil/false ⇒ off.
	// +optional
	NativeFileHandling *bool `json:"nativeFileHandling,omitempty"`

	// RequireSubagentDigestPins is a security requirement, not a grant: when
	// true, delegation through an UNPINNED spec.subagents roster entry is
	// refused — every delegation target must be pinned name@sha256:<digest>
	// and match its installed bundle digest. Requirement semantics fold
	// downward: any tier may add the requirement; no lower tier can remove
	// one set above it. nil/false ⇒ not required.
	// +optional
	RequireSubagentDigestPins *bool `json:"requireSubagentDigestPins,omitempty"`

	// AllowedSandboxKinds constrains which sandbox backends may be used at all.
	// Tri-state, like the other allowlists here: nil imposes no ceiling; a
	// non-nil list (including an empty one, which denies everything)
	// constrains. The effective set is the intersection across constraining
	// tiers, so a lower tier can narrow but never widen.
	//
	// This is how a cluster admin refuses a backend outright — which matters
	// once a backend can run workloads, and their credentials, outside the
	// cluster.
	// +optional
	AllowedSandboxKinds *[]string `json:"allowedSandboxKinds,omitempty"`
	// RequireStandingFor names resource types that MUST use `required` standing,
	// whatever their schema fragment declares. It is the admin veto over
	// SpiceDBResource.Standing.
	//
	// A CEILING, not a default — the same shape as MinPlanGateMode. Tiers UNION
	// their sets: a namespace may add types the cluster did not name, and can
	// never remove one it did. Empty means no veto.
	// +optional
	// +listType=atomic
	RequireStandingFor []string `json:"requireStandingFor,omitempty"`

	// BuilderClasses is the cluster-admin sanction list for the agent-builder
	// workshop (spec §1.1): only a class named here, in this exact namespace,
	// may be provisioned a workshop — and only when it also references the
	// named SidecarToolbox. CLUSTER TIER ONLY: the AgentSession reconciler
	// reads this from ClusterAgentSettings alone, so a namespace tenant's
	// AgentSettings carrying it is inert (a namespace must not self-sanction).
	// +optional
	BuilderClasses *[]BuilderClassRef `json:"builderClasses,omitempty"`

	// MaxWorkshopsPerStarter caps live workshops per starting person,
	// cluster-wide default 3 when unset. Cluster tier only, like BuilderClasses.
	// +optional
	MaxWorkshopsPerStarter *int32 `json:"maxWorkshopsPerStarter,omitempty"`
}

// SettingsBudgetCeiling caps budget dimensions. Each field is optional; an
// unset (zero) field contributes no ceiling for that dimension.
type SettingsBudgetCeiling struct {
	// MaxTurns caps agent turns per session. Zero = no ceiling on turns.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxTurns int32 `json:"maxTurns,omitempty"`
	// MaxTokens caps total tokens per session. Zero = no ceiling on tokens.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxTokens int64 `json:"maxTokens,omitempty"`
	// MaxDuration is a duration (e.g. "2h"). Zero = no ceiling on duration.
	// +optional
	MaxDuration metav1.Duration `json:"maxDuration,omitempty"`
	// SessionExpiration is a duration (e.g. "24h"). Zero = no ceiling on the
	// wall-clock lifetime cap.
	// +optional
	SessionExpiration metav1.Duration `json:"sessionExpiration,omitempty"`
	// MaxDelegatedAgents caps the total number of sessions in one delegation
	// tree. Zero = no ceiling imposed by this tier (the controller's own
	// built-in bound still applies — see BudgetConfig.MaxDelegatedAgents).
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxDelegatedAgents int32 `json:"maxDelegatedAgents,omitempty"`
}

// SettingsAuthzCeiling caps how long a human-in-the-loop authz window may stay
// open. Each field is optional; an unset (zero) field contributes no ceiling
// for that window. A window that a lower tier asks to be LONGER than the
// tightest ceiling is clamped down to it by the resolver, which records the
// clamp on status.effectiveSettings (provenance "clamped") and raises a
// non-fatal AuthzClamped violation.
//
// Only the two windows that bound granted authority are capped. The cold-start
// scopeMaxLlmLatencyMs is a latency budget for the extractor LLM, not a window
// in which authority is held, so it stays a default-only knob.
type SettingsAuthzCeiling struct {
	// MaxApprovalTimeout caps AuthzBlock.ApprovalTimeout: how long a tool-call,
	// leakage-share, or cold-start scope approval may wait for a human before
	// the gate's timeout policy fires. Zero = no ceiling on the wait.
	// +optional
	MaxApprovalTimeout metav1.Duration `json:"maxApprovalTimeout,omitempty"`

	// MaxInformationLeakageApprovalTTL caps
	// InformationLeakagePolicy.ApprovalTTL: how long an approved leakage share
	// may be reused before the agent must ask again. Zero = no ceiling on
	// reuse.
	// +optional
	MaxInformationLeakageApprovalTTL metav1.Duration `json:"maxInformationLeakageApprovalTTL,omitempty"`
}

// AllowedMCPServer names a permitted MCPServer (by CR name) and optionally
// restricts which of its tools may be used. A nil/empty Tools list, or a list
// containing "*", means all of that server's tools are allowed.
type AllowedMCPServer struct {
	// Name is the permitted MCPServer CR name.
	Name string `json:"name"`
	// Tools narrows to specific tool names; nil, empty, or ["*"] allows all.
	// +optional
	// +listType=atomic
	Tools []string `json:"tools,omitempty"`
}

// ModelCatalogEntry is one permitted model in the cluster-tier model catalog.
// At the cluster tier TokenRef is required (the central credential); at the
// namespace tier entries are name-only narrowing references and TokenRef/Default
// must be unset (enforced by the settings webhook).
type ModelCatalogEntry struct {
	// Name is the model identifier, e.g. "claude-opus-4-8".
	Name string `json:"name"`

	// Provider is the LLM provider serving this model.
	// +kubebuilder:validation:Enum=anthropic;openai;openrouter;test
	// +optional
	Provider string `json:"provider,omitempty"`

	// TokenRef is the central API-token Secret (cluster tier only). Required at
	// the cluster tier; forbidden below it.
	// +optional
	TokenRef *NamespacedSecretKeyRef `json:"tokenRef,omitempty"`

	// Default marks the inherited default entry. At most one cluster entry may
	// set it; `oap init` sets it.
	// +optional
	Default bool `json:"default,omitempty"`

	// InputPerMTok / OutputPerMTok are best-effort USD list prices per million
	// input / output tokens, surfaced by the admin dashboard's cost estimator
	// (labelled "estimated"). Optional; unset ⇒ no catalog price for this model
	// and the dashboard shows "est. (no price)".
	// +kubebuilder:validation:Minimum=0
	// +optional
	InputPerMTok float64 `json:"inputPerMTok,omitempty"`
	// OutputPerMTok is the output half of the same best-effort list price.
	// +kubebuilder:validation:Minimum=0
	// +optional
	OutputPerMTok float64 `json:"outputPerMTok,omitempty"`

	// Routing configures OpenRouter dynamic model selection (auto/fallback/provider
	// routing). Only consulted when Provider == "openrouter". Optional.
	// +optional
	Routing *OpenRouterRouting `json:"routing,omitempty"`
}

// HasPrice reports whether this entry carries a usable per-token price.
func (e ModelCatalogEntry) HasPrice() bool {
	return e.InputPerMTok > 0 || e.OutputPerMTok > 0
}

// NamespacedSecretKeyRef is a SecretKeyRef that names its namespace explicitly.
// Used for catalog tokens, which live in the operator/system namespace and so
// cannot be resolved "in the same namespace" like a tenant SecretKeyRef.
type NamespacedSecretKeyRef struct {
	// Namespace is the Secret's namespace.
	Namespace string `json:"namespace"`
	// Name is the Secret's name.
	Name string `json:"name"`
	// Key is the data key within the Secret holding the value.
	Key string `json:"key"`
}

// ContentInspectorConfig references a registered content-guard inspector by ID
// and carries its opaque per-instance config (parsed by the inspector).
type ContentInspectorConfig struct {
	// ID must resolve in the contentguard registry (e.g. "url-allowlist").
	// +kubebuilder:validation:MinLength=1
	ID string `json:"id"`
	// Config is inspector-specific configuration, validated by the inspector's
	// Configure at admission (fail-closed) and at session start.
	Config apiextv1.JSON `json:"config"`
}

// PreferenceGlobal is one platform-admin value for a class preference key.
type PreferenceGlobal struct {
	// Value is the admin-set value; validated against the class's
	// userPreferences schema by the settings controller (status violation on
	// mismatch) and ignored WITH a snapshot violation at resolve time if it
	// no longer type-checks.
	// +kubebuilder:pruning:PreserveUnknownFields
	Value apiextv1.JSON `json:"value"`

	// Lock mandates the value: the user layer is ignored for this key and
	// set_preference refuses it, naming the admin policy.
	// +optional
	Lock bool `json:"lock,omitempty"`
}

// SettingsDefaults are fallback values used when a lower tier omits the field.
type SettingsDefaults struct {
	// Model defaults the agent model when an AgentClass omits its own.
	// +optional
	Model *DefaultModel `json:"model,omitempty"`

	// Budget defaults the agent budget when an AgentClass omits its own.
	// +optional
	Budget *BudgetConfig `json:"budget,omitempty"`

	// Authz defaults the per-class approval/authz timeouts.
	// +optional
	Authz *DefaultAuthz `json:"authz,omitempty"`

	// ToolGuard is this tier's default tool-guard policy, consulted when the
	// AgentClass has no matching rule (class → namespace → cluster → built-in).
	// +optional
	ToolGuard *ToolGuardPolicy `json:"toolGuard,omitempty"`

	// ReportSessionCost toggles the end-of-session cost estimate (a closing
	// channel message + status.estimatedCost). Optional, on by default: nil at
	// every tier ⇒ on. A lower tier's explicit value wins (namespace over
	// cluster). A behavioral default, so it lives in Defaults, not Limits.
	// +optional
	ReportSessionCost *bool `json:"reportSessionCost,omitempty"`

	// Sandbox is the default sandbox backend for agents in scope. A lower tier
	// that names its own backend overrides this; a bundle that names none
	// inherits it.
	// +optional
	Sandbox *SandboxBackend `json:"sandbox,omitempty"`
}

// DefaultModel is a fallback model. Unlike ModelConfig, APIKey is optional: the
// cluster tier can default provider+name only, because the apiKey is a
// namespaced Secret reference. The namespace tier (or the AgentClass) supplies
// the apiKey.
type DefaultModel struct {
	// Provider is the LLM provider serving this model.
	// +kubebuilder:validation:Enum=anthropic;openai;openrouter;test
	Provider string `json:"provider"`
	// Name is the provider's model identifier.
	Name string `json:"name"`
	// FromCatalog lets a tier default to a catalog entry by name (alternative to
	// inline Provider/Name).
	// +optional
	FromCatalog string `json:"fromCatalog,omitempty"`
	// APIKey is the same-namespace Secret holding the provider token; unset
	// leaves the key to a lower tier or to the catalog's central TokenRef.
	// +optional
	APIKey *SecretKeyRef `json:"apiKey,omitempty"`
}

// DefaultAuthz supplies fallback authz timeouts mirrored from AgentClass authz.
// Each field defaults the correspondingly-named knob on an AgentClass when that
// class omits it.
type DefaultAuthz struct {
	// ApprovalTimeout defaults AuthzBlock.ApprovalTimeout.
	// +optional
	ApprovalTimeout *metav1.Duration `json:"approvalTimeout,omitempty"`
	// InformationLeakageApprovalTTL defaults InformationLeakagePolicy.ApprovalTTL.
	// +optional
	InformationLeakageApprovalTTL *metav1.Duration `json:"informationLeakageApprovalTTL,omitempty"`
	// ScopeMaxLLMLatencyMs defaults ScopeSpec.MaxLLMLatencyMs.
	// +optional
	ScopeMaxLLMLatencyMs *int32 `json:"scopeMaxLlmLatencyMs,omitempty"`

	// PlanGate defaults AuthzBlock.PlanGate — what a class gets when it says
	// nothing. A class may still declare a laxer mode; forbidding that is
	// SettingsLimits.MinPlanGateMode's job, and is a separate deliberate act.
	// +optional
	PlanGate *PlanGateConfig `json:"planGate,omitempty"`

	// Metaagent defaults AuthzBlock.Metaagent — what a class gets when it says
	// nothing. A class may still opt itself into inline; forbidding that would
	// need a SettingsLimits floor, which does not exist for this yet.
	// +optional
	Metaagent *MetaagentConfig `json:"metaagent,omitempty"`
}

// SettingsStatus is the shared status for both settings CRDs.
type SettingsStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ObservedLimits is the durable trigger state for tool-origin revocation:
	// the allowlists the operator has already published revokes for. The
	// settings RevokePublisher diffs spec.limits against this record, so an
	// entry withdrawn while the operator was down — or one whose revoke failed
	// to publish — is still emitted on the next reconcile, instead of living
	// only in a process-scoped map that dies with the pod.
	//
	// nil means "never observed": prime without emitting, so a fresh install
	// does not revoke origins nobody ever granted.
	// +optional
	ObservedLimits *ObservedSettingsLimits `json:"observedLimits,omitempty"`

	// Conditions carries SelfConsistent; see its type constant.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// ObservedSettingsLimits is the operator's record of the allowlists it last
// diffed for tool-origin revocation. Each list preserves the spec's tri-state:
// a nil pointer records "the list was absent" (allow-all, nothing to revoke
// against), a pointer to an empty list records deny-all, and a populated list
// records the allowlist. Collapsing nil and [] would turn a widening to
// allow-all into a revoke of every origin.
//
// Names are sorted so an unchanged spec produces a byte-identical record.
type ObservedSettingsLimits struct {
	// AllowedMCPServers is the set of names last observed in
	// spec.limits.allowedMCPServers.
	// +optional
	AllowedMCPServers *[]string `json:"allowedMCPServers,omitempty"`

	// AllowedToolkits is the set of names last observed in
	// spec.limits.allowedToolkits.
	// +optional
	AllowedToolkits *[]string `json:"allowedToolkits,omitempty"`
}

// BuilderClassRef names one sanctioned builder AgentClass and the exact
// SidecarToolbox CR (by name, in the class's namespace) that receives the
// workshop identity. All three fields are required: a sanction that did not
// name the sidecar would leave "which sidecar gets the token" to a naming
// convention, which is a guess where a boundary needs a declaration.
type BuilderClassRef struct {
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	SidecarToolbox string `json:"sidecarToolbox"`
}

// BuilderClassFor returns the sanction entry for ns/name, or nil. Nil receiver
// and absent list both answer nil — the fail-closed reading of "no sanction".
func (l *SettingsLimits) BuilderClassFor(ns, name string) *BuilderClassRef {
	if l == nil || l.BuilderClasses == nil {
		return nil
	}
	for i := range *l.BuilderClasses {
		if (*l.BuilderClasses)[i].Namespace == ns && (*l.BuilderClasses)[i].Name == name {
			return &(*l.BuilderClasses)[i]
		}
	}
	return nil
}

// DefaultMaxWorkshopsPerStarter is the per-starter live-workshop ceiling in
// force when spec.limits.maxWorkshopsPerStarter is unset.
const DefaultMaxWorkshopsPerStarter = int32(3)

// MaxWorkshopsPerStarterOrDefault returns the per-starter live-workshop
// ceiling. Nil receiver and unset field both answer the default.
//
// THE accessor, for the same reason WorkshopLimitBody is one constant: two
// gates count against this ceiling — the browser start route refuses at it,
// the operator's ensureWorkshop boot-fails at it — and a second reading of
// "unset means 3" in either package is a number that can drift, leaving the
// route admitting a start the operator immediately kills.
func (l *SettingsLimits) MaxWorkshopsPerStarterOrDefault() int32 {
	if l == nil || l.MaxWorkshopsPerStarter == nil {
		return DefaultMaxWorkshopsPerStarter
	}
	return *l.MaxWorkshopsPerStarter
}
