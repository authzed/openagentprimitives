package v1alpha1

import (
	"regexp"
	"slices"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,spicebox},shortName=agcls
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Valid",type="string",JSONPath=".status.conditions[?(@.type=='Valid')].status"
// +kubebuilder:printcolumn:name="OAP",type="string",JSONPath=".status.oapInstall.version"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient
//
// AgentClass is the reviewable template an agent is defined by: model, system
// prompt, tools, skills, identity, channels, budget and authz policy. It never
// runs on its own -- an AgentSession is one instantiation of it.
//
// Namespaced. Reconciled by pkg/controllers/agentclass, which validates the
// spec and confirms referenced objects exist, then reports Valid=True/False.
// The AgentSession reconciler gates on Valid=True, so an invalid class fails a
// session closed rather than starting it degraded.
type AgentClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentClassSpec   `json:"spec,omitempty"`
	Status AgentClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentClass `json:"items"`
}

// The emptiness test is size(...) > 0 and NOT `!= <two single quotes>`, which
// is the obvious spelling and does not survive the toolchain: gofmt applies
// typographic conversion to doc comments, and a kubebuilder marker attached to
// a type IS a doc comment — so a pair of single quotes is rewritten to a curly
// quote on the next gofmt, the CEL no longer lexes, and every CRD install fails.
// Keep this expression quote-free. TestCRDValidationRulesParse guards the class.
// +kubebuilder:validation:XValidation:rule="!(has(self.identityMode) && (self.identityMode == 'ask' || self.identityMode == 'dynamic')) || (has(self.agentIdentity) && size(self.agentIdentity) > 0)",message="spec.identityMode ask|dynamic requires spec.agentIdentity to be set"
// +kubebuilder:validation:XValidation:rule="!(has(self.identityMode) && self.identityMode == 'dynamic') || (has(self.identityRecommendation) && has(self.identityRecommendation.prompt))",message="spec.identityMode=dynamic requires spec.identityRecommendation.prompt"
type AgentClassSpec struct {
	// DisplayName is the human-friendly label surfaced to end users
	// (channel thread messages, approver prompts, audit logs). When
	// unset, callers fall back to the AgentClass's metadata.name.
	// Examples: "MarketingBot", "HubSpot Companies Agent".
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Description is a human-readable summary surfaced in events / status.
	// +optional
	Description string `json:"description,omitempty"`

	// Model is optional: when omitted, the class inherits the resolved tier
	// default model (see status.effectiveSettings.model). At least one of the
	// class or a settings tier must supply a model before a session runs.
	// +optional
	Model *ModelConfig `json:"model,omitempty"`

	// Harness names the agent harness that runs this class's outer loop.
	// Absent means "ap-native", the built-in runner loop. A named harness
	// must be registered in the operator binary; an unknown value marks the
	// AgentClass Valid=False rather than failing at pod-create time.
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	Harness string `json:"harness,omitempty"`

	SystemPrompt PromptSource `json:"systemPrompt"`

	// AgentIdentity in same namespace; default for all bundles.
	// +optional
	AgentIdentity string `json:"agentIdentity,omitempty"`

	// IdentityMode selects how the agent's tool credentials are sourced.
	//
	//   - agent (default): tools use the credentials bound to
	//     spec.agentIdentity. Today's behavior.
	//   - userPassthrough: tools use the credentials of the user who
	//     started the session, drawn from that user's UserIdentity
	//     catalog. spec.agentIdentity is ignored for credentialed tools.
	//     The session parks in AwaitingCredentials until the starter has
	//     linked everything the agent needs.
	//   - ask: interactive. At session start, the initiating user is asked
	//     to choose between acting as the agent (spec.agentIdentity) or as
	//     themselves (userPassthrough). Requires spec.agentIdentity. The
	//     session parks in AwaitingIdentityChoice until the user answers.
	//   - dynamic: interactive. An isolated recommender LLM (configured via
	//     spec.identityRecommendation) proposes agent or userPassthrough
	//     for the session, but the initiating user always confirms — the
	//     recommendation is advisory only. Requires spec.agentIdentity and
	//     spec.identityRecommendation.prompt. Also parks in
	//     AwaitingIdentityChoice until confirmed.
	// +kubebuilder:validation:Enum=agent;userPassthrough;ask;dynamic
	// +kubebuilder:default=agent
	// +optional
	IdentityMode string `json:"identityMode,omitempty"`

	// CredentialLinkTimeout caps how long a userPassthrough session may sit
	// in AwaitingCredentials before the operator fails it. Measured from
	// max(parkedAt, lastInteractionAt). Default 30m.
	// +kubebuilder:default="30m"
	// +optional
	CredentialLinkTimeout *metav1.Duration `json:"credentialLinkTimeout,omitempty"`

	// IdentityRecommendation configures the dynamic-mode recommender LLM.
	// Required when identityMode=dynamic; ignored otherwise.
	// +optional
	IdentityRecommendation *IdentityRecommendationConfig `json:"identityRecommendation,omitempty"`

	// IdentityChoiceTimeout caps how long an ask|dynamic session may sit in
	// AwaitingIdentityChoice before the operator fails it. Measured from
	// status.identityChoiceParkedAt. Default 30m.
	// +kubebuilder:default="30m"
	// +optional
	IdentityChoiceTimeout *metav1.Duration `json:"identityChoiceTimeout,omitempty"`

	// ToolBundles is optional. An empty/missing list means the agent runs
	// with only meta tools (e.g. agent_work_complete). Plan 1 supports only
	// the empty case; Plan 2 wires sandbox tool dispatch.
	// +optional
	ToolBundles []ToolBundle `json:"toolBundles,omitempty"`

	// MCPServers references MCPServer CRs in the same namespace whose
	// allowlisted tools should be exposed to the LLM during a session.
	// +optional
	MCPServers []AgentClassMCPServerRef `json:"mcpServers,omitempty"`

	// CredentialExplanations lets the AgentClass explain, per credential,
	// why this agent needs it. Keyed by the FINAL (post-credentialRemap)
	// credential name. Optional; credentials without an entry fall back to
	// the LLM explainer (if configured) or a generic sentence for the why.
	// Title/description are always sourced from the tool/provider catalog,
	// never here.
	// +optional
	CredentialExplanations []CredentialExplanationSpec `json:"credentialExplanations,omitempty"`

	// SidecarToolboxes references SidecarToolbox CRs in the same namespace
	// whose tools should be exposed to the LLM. Each ref's Name is the
	// LLM-facing prefix; pattern [a-z0-9_-]{1,32}.
	// +optional
	SidecarToolboxes []AgentClassSidecarToolboxRef `json:"sidecarToolboxes,omitempty"`

	// WorkspaceSource optionally binds a single WorkspaceSource whose
	// materialized base seeds each session's /workspace as a per-session
	// copy-on-write overlay. The referenced CR must be Valid. A single source
	// per class (it seeds the one /workspace); multi-source is a later phase.
	// +optional
	WorkspaceSource *AgentClassWorkspaceSourceRef `json:"workspaceSource,omitempty"`

	// ToolSessionLog controls whether an interactive tool's parsed
	// stream events (e.g. Claude Code's stream-json) are persisted to
	// the tool_session memory Kind so `oap agent logs --follow-live`
	// can show them. The runner-to-channel (Slack) stream is
	// unaffected regardless.
	//   off        — do not persist.
	//   highSignal — persist tool_use/result events, skip text deltas.
	//   full       — persist every event.
	// +kubebuilder:validation:Enum=off;highSignal;full
	// +kubebuilder:default=highSignal
	// +optional
	ToolSessionLog string `json:"toolSessionLog,omitempty"`

	// Budget is optional: when omitted, the class inherits the resolved tier
	// default budget (see status.effectiveSettings.budget).
	// +optional
	Budget *BudgetConfig `json:"budget,omitempty"`

	// BoundEntities is the PRE-RENAME spelling of spec.authz.slots.
	//
	// Still deserialized on purpose. This is an AUTHZ field: dropping it from
	// the schema outright would make an upgraded AgentClass silently lose its
	// instance constraints — nothing would fail, the agent would just be less
	// constrained than its author wrote. Instead the reconciler rejects a class
	// that still sets it, naming the new field, so the loss is impossible to
	// miss. Loud beats silent when what is being dropped is a constraint.
	//
	// Deprecated: move to spec.authz.slots.
	// +optional
	// +listType=atomic
	BoundEntities []AuthzSlot `json:"boundEntities,omitempty"`

	// Channels governs channel-attached behavior. Optional — kubectl-driven
	// sessions ignore this entirely.
	// +optional
	Channels *ChannelsConfig `json:"channels,omitempty"`

	// Authz consolidates tool-call, session-interact, and information-leakage
	// authorization policy. Optional; absent means legacy defaults apply.
	// +optional
	Authz *AuthzBlock `json:"authz,omitempty"`

	// Skills lists the agent skills this class opts into. Subject to the
	// tiered AllowedSkills/DeniedSkills ceilings, which match on Ref; a
	// disallowed skill makes the class SettingsAccepted=False.
	// +optional
	Skills []AgentSkill `json:"skills,omitempty"`

	// Subagents is the closed set of AgentClass names this class may delegate
	// to, in its own namespace. Membership is checked at delegation time and the
	// whole graph is DAG-validated at admission, so a cycle is a validation
	// error rather than a runtime loop — the only kind of bound that holds.
	//
	// Absent or empty means this class delegates to nobody, which is the
	// default: delegation is opt-in.
	//
	// Membership alone permits single_turn delegation ONLY: a headless child
	// that takes spec.prompt in and hands status.result back. Widening a member
	// to one of the conversational modes is a separate, explicit declaration in
	// subagentModes, so a roster written before those modes existed cannot
	// silently gain the ability to spawn a child with a two-way channel.
	//
	// An entry may optionally carry a digest pin: "name@sha256:<64 lowercase hex>".
	// Pinned entries name the exact digest of the AgentClass to delegate to;
	// the SubagentRequest controller verifies the pin matches at delegation time.
	// Only a properly formed @sha256:<64 hex> suffix is recognized as a pin;
	// anything else stays part of the literal name (and the pattern below refuses it).
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(@sha256:[a-f0-9]{64})?$`
	Subagents []string `json:"subagents,omitempty"`

	// SubagentModes raises the delegation-mode ceiling for individual roster
	// members, keyed by the AgentClass name exactly as it appears in subagents.
	// Values are SubagentRequest modes -- single_turn, task, chat -- documented
	// on SubagentRequestSpec.Mode, which is where what each one exposes is
	// described.
	//
	// The ceiling sits HERE, on the delegating parent, and not on the child's
	// own class: two parents may legitimately be trusted with different modes
	// for the same child.
	//
	// A delegating agent names the mode it wants in its delegate call, and the
	// SubagentRequest controller refuses any mode this map does not permit --
	// it may pick NARROWER than permitted, never wider, and a refusal is never
	// downgraded to single_turn and run anyway. single_turn is permitted for
	// every roster member without being listed, because it provisions strictly
	// less than the other two (no channel at all). Nothing else is implied:
	// chat does not imply task.
	//
	// A key naming a class absent from subagents, or a value that is not one of
	// the three modes, is a configuration error rather than a silent no-op --
	// it marks the class Valid=False with reason RosterInvalid. There is no
	// apiserver-level Enum on the values: controller-gen cannot put an items
	// enum inside a map's additionalProperties ("must apply
	// kubebuilder:validation:items:Enum to an array value, found object"), so
	// the AgentClass controller is the one authority on what a valid value is.
	// +optional
	SubagentModes map[string][]string `json:"subagentModes,omitempty"`

	// ToolGuard is this class's tool circuit-breaker / rate-limit rules,
	// consulted first in the tier walk (overrides namespace/cluster defaults
	// and the built-in rule for matching tools).
	// +optional
	ToolGuard *ToolGuardPolicy `json:"toolGuard,omitempty"`

	// Capabilities grants optional meta-tool capabilities to the agent. A key
	// present grants that capability; the value is its per-capability config
	// (may be empty {}). Default-on capabilities (planning, channel
	// interaction, …) are active without being listed; list one with
	// {"enabled": false} to disable it. Unknown or unavailable grants are
	// ignored gracefully and surfaced on status (CapabilitiesValid), never
	// blocking readiness.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Capabilities map[string]apiextensionsv1.JSON `json:"capabilities,omitempty"`

	// Config is a map of installer-bound values exposed to this class's
	// toolspec constraint CEL as the `config` root (see the toolspec
	// validator). It is generic and toolkit-agnostic: it never mentions repos
	// or any tool-specific shape — a constraint decides what a value means.
	// Each value is a JSON scalar or array. Validated against ConfigSchema by
	// the AgentClass controller; a config that violates its schema, or that a
	// bound toolspec's CEL references a key not declared here, marks the class
	// Valid=False rather than failing at tool-call time.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Config map[string]apiextensionsv1.JSON `json:"config,omitempty"`

	// ConfigSchema declares each Config key's contract, so Config can be
	// validated at reconcile and every `config.X` a bound toolspec constraint
	// references is known to exist. Mirrors the oap install-question shape, so a
	// bundle's install question maps 1:1 onto a key.
	// +optional
	ConfigSchema []ConfigKeySchema `json:"configSchema,omitempty"`

	// UserPreferences declares the per-user preferences this class offers.
	// Empty means the feature is absent for this class: no preferences tools
	// are offered and no preference endpoints are active.
	// +optional
	UserPreferences []UserPreferenceSchema `json:"userPreferences,omitempty"`

	// CompletionRequirements are the conditions a session of this class must
	// satisfy before its agent may declare a round of work complete. Each entry
	// is a key registered in the completion-requirement registry
	// (pkg/agent/completion) — "artifact-delivered", "plan-steps-complete",
	// "trigger-status-concluded".
	//
	// This is the OPERATOR's guarantee that no session of this class finishes
	// without producing what the class exists to produce, and it deliberately
	// does not depend on the agent choosing to declare anything: an agent that
	// forgets its own obligation is precisely the case it covers.
	//
	// NOT the same axis as a plan phase's `requires`, which orders phase ENTRY
	// and never reads an agent's assertion that it finished something. These
	// are read at exactly that assertion.
	//
	// An unmet requirement makes agent_work_complete refuse, naming what is
	// missing. The agent may still finish by supplying a `bypass_reason`, which
	// is recorded on status.completionBypasses and posted to the session's
	// channel — a bypass with no escape would turn a degraded result into a
	// wedged session, and one nobody sees would be an off switch.
	//
	// Not enum-validated: the registry is open by design, and a key this build
	// does not know fails closed at call time (refuse, name it, stay
	// bypassable) rather than being silently ignored at admission.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:items:Pattern=`^[a-z][a-z0-9-]*$`
	CompletionRequirements []string `json:"completionRequirements,omitempty"`

	// AgentUI references the AgentUI this class serves and carries the
	// DEPLOYMENT half of the tool grant. It lives here, not on AgentUI,
	// because the bundle author writes the request and the deployer writes the
	// authorization — different objects, so no two field managers contend.
	// Absent ⇒ no UI-callable tools.
	// +optional
	AgentUI *AgentClassUIGrant `json:"agentUI,omitempty"`
}

// ToolSessionLog modes — values for AgentClassSpec.ToolSessionLog.
const (
	ToolSessionLogOff        = "off"
	ToolSessionLogHighSignal = "highSignal"
	ToolSessionLogFull       = "full"
)

// IdentityRecommendationConfig is the dynamic-mode recommender knob.
type IdentityRecommendationConfig struct {
	// Prompt is short class-authored guidance handed to the recommender LLM,
	// e.g. "Prefer the agent's own identity in busy multi-person threads;
	// prefer acting on the user's behalf when a single user works from fresh
	// context." The recommender's output is advisory only — the user always
	// confirms — so this prompt never authorizes anything by itself.
	// +kubebuilder:validation:MinLength=1
	Prompt string `json:"prompt"`
}

type ModelConfig struct {
	// +kubebuilder:validation:Enum=anthropic;openai;openrouter;test
	// +optional
	Provider string `json:"provider,omitempty"`
	// +optional
	Name string `json:"name,omitempty"`
	// FromCatalog references a model catalog entry by name. Mutually exclusive
	// with an inline Provider/Name. When set, Provider and the token come from
	// the catalog entry.
	// +optional
	FromCatalog string `json:"fromCatalog,omitempty"`
	// APIKey is a bring-your-own token. Only honored when allowModelOverride is
	// granted (else a fatal violation). Catalog references leave this empty.
	// +optional
	APIKey SecretKeyRef `json:"apiKey,omitempty"`
	// RoutingMetadata refines dynamic model selection for this agent. Merged over
	// the resolved catalog entry's routing with narrowing-only semantics: an
	// AgentClass may constrain, never loosen, admin policy. Inert unless the
	// resolved model performs dynamic selection (Provider == "openrouter").
	// +optional
	RoutingMetadata *OpenRouterRouting `json:"routingMetadata,omitempty"`
}

// PromptSource carries exactly one of inline or configMapRef.
type PromptSource struct {
	// +optional
	Inline string `json:"inline,omitempty"`
	// +optional
	ConfigMapRef *ConfigMapKeyRef `json:"configMapRef,omitempty"`
}

type ToolBundle struct {
	// Name is the LLM-facing prefix; pattern [a-z0-9_-]{1,32}.
	Name string `json:"name"`
	// Class names a SpiceboxClass (cluster-scoped).
	Class string `json:"class"`
	// Toolspecs lists SpiceboxToolspec names that scope this bundle.
	Toolspecs []string `json:"toolspecs"`
	// AgentIdentity overrides spec.agentIdentity for this bundle.
	// +optional
	AgentIdentity string `json:"agentIdentity,omitempty"`
	// CredentialRemap remaps a tool-declared credential name to a
	// differently-named credential in the agent's identity catalog —
	// the opt-in escape hatch when two tools declare the same credential
	// name but need different tokens. Keyed by the tool's declared name.
	// +optional
	CredentialRemap map[string]string `json:"credentialRemap,omitempty"`
	// Sandbox overrides the sandbox backend for this bundle, taking precedence
	// over the referenced SpiceboxClass's own setting: the class states the
	// bundle's default, and the agent consuming it may have a reason to run it
	// elsewhere. Unset inherits the class's setting, or the tier defaults.
	// +optional
	Sandbox *SandboxBackend `json:"sandbox,omitempty"`
	// StageSkills names which of this AgentClass's sandbox-targeted skills get
	// staged to disk, by their local AgentSkill.Name. "*" means all of them.
	// Absent means none — the default, since most agents consume a skill
	// through load_skill and never need it on disk.
	//
	// Enforced CLASS-WIDE, not per-bundle: the union of every ToolBundle's
	// StageSkills in this class is staged onto EVERY bundle's sandbox session,
	// so a bundle whose own StageSkills names nothing still receives whatever
	// another bundle in the class opted in. A per-bundle mount, scoped to only
	// the bundle that named it, is a possible future direction and not
	// current behaviour.
	// +optional
	StageSkills []string `json:"stageSkills,omitempty"`
}

// AgentSkill is one skill this class opts into: a local handle plus a
// reference to the shared skill, the same shape as ToolBundle{Name, Class}.
type AgentSkill struct {
	// Name is the local handle, unique within this class, and constrained to
	// [a-z0-9_-]{1,32} (validateSkillsSpec) because it is also the directory
	// name under the mount path when this skill is staged — which is why a
	// sandbox-targeted skill's Name must equal its SKILL.md frontmatter name;
	// Claude Code discovers a skill only when those agree.
	Name string `json:"name"`
	// Ref is the skill's canonical name, "<repo-locator>//<subpath>@<ref>".
	Ref string `json:"ref"`
	// Target says who consumes this skill.
	// +kubebuilder:default=agent
	// +optional
	Target SkillTarget `json:"target,omitempty"`
}

// SkillTarget names the consumer of a skill.
// +kubebuilder:validation:Enum=agent;sandbox;both
type SkillTarget string

const (
	// SkillTargetAgent is the default and the pre-existing behaviour: the
	// description goes in the system prompt and the body is available via
	// load_skill. Nothing is staged and no ConfigMap is created.
	SkillTargetAgent SkillTarget = "agent"
	// SkillTargetSandbox stages the skill on disk and suppresses it from the
	// outer agent's prompt entirely.
	SkillTargetSandbox SkillTarget = "sandbox"
	// SkillTargetBoth does both.
	SkillTargetBoth SkillTarget = "both"
)

type BudgetConfig struct {
	// +kubebuilder:validation:Minimum=1
	MaxTurns int32 `json:"maxTurns"`
	// +kubebuilder:validation:Minimum=1
	MaxTokens int64 `json:"maxTokens"`
	// MaxDuration is the cumulative ACTIVE RUN-TIME budget (e.g. "30m") — the
	// wall time the agent actually spent working, excluding time parked waiting
	// on a human (the next user message or a tool-call approval). It persists
	// across sleep/resume via status.runDuration; it is NOT wall-clock since
	// session start. For a wall-clock lifetime cap, use SessionExpiration.
	// Zero = no run-time cap.
	MaxDuration metav1.Duration `json:"maxDuration"`
	// SessionExpiration is a hard wall-clock lifetime cap measured from
	// status.startedAt (e.g. "24h"), regardless of how much the agent ran.
	// The operator fails the session once exceeded, even while it is idle or
	// asleep. Zero = never expires (no default). Distinct from MaxDuration,
	// which counts only active run-time.
	// +optional
	SessionExpiration metav1.Duration `json:"sessionExpiration,omitempty"`
	// MaxDelegatedAgents bounds the TOTAL number of sessions in one delegation
	// tree, counting the root. It is a property of the tree rather than of any
	// one session: every other dimension here caps a single session's spend,
	// and a tree of N sessions each individually within budget still spends
	// N times it.
	//
	// Resolved from the ROOT session's effective settings and enforced by the
	// SubagentRequest controller before a child is created.
	//
	// Zero means UNSET, not unlimited. A consumer must apply its own built-in
	// bound rather than reading zero as "no cap" — that reading is right for a
	// DURATION (as MaxDuration uses it) and wrong for a COUNT, where it would
	// license exactly the runaway fan-out this field exists to prevent.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxDelegatedAgents int32 `json:"maxDelegatedAgents,omitempty"`
}

// ConfigKeySchema declares one AgentClass.spec.config key's contract. The
// AgentClass controller validates spec.config against these, and cross-checks
// that every `config.X` a bound toolspec constraint references is declared.
type ConfigKeySchema struct {
	// Name is the config key: the map key in spec.config and the identifier a
	// toolspec constraint reads as `config.<Name>`.
	Name string `json:"name"`

	// Type is the value's shape.
	// +kubebuilder:validation:Enum=string;stringList;int;bool;enum
	Type string `json:"type"`

	// Enum is the permitted set: the allowed value when Type=enum, or the
	// allowed item vocabulary when Type=stringList. Empty ⇒ open.
	// +optional
	Enum []string `json:"enum,omitempty"`

	// Pattern is a regexp every string value (or stringList item) must match.
	// +optional
	Pattern string `json:"pattern,omitempty"`

	// Required fails validation when the key is absent from spec.config.
	// +optional
	Required bool `json:"required,omitempty"`
}

// PreferenceEnumValue is one permitted value of an enum-typed preference.
type PreferenceEnumValue struct {
	// Value is the stored token; unique within the enum.
	// +kubebuilder:validation:MinLength=1
	Value string `json:"value"`

	// Description is user-facing copy for this choice, surfaced by the
	// preferences tools and any schema-driven form.
	// +optional
	Description string `json:"description,omitempty"`
}

// UserPreferenceSchema declares one per-user preference this class offers.
// Values are saved per user (memory data plane), resolved per turn author,
// and read by the agent through get_preferences. NEVER declare a secret as
// a preference — credentials belong to the identity plane (UserIdentity).
type UserPreferenceSchema struct {
	// Name is the preference key, unique within the list; the identifier the
	// tools and (phase 2) toolspec CEL read as `prefs.<Name>`.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Type is the value's shape.
	// +kubebuilder:validation:Enum=string;stringList;int;bool;enum
	Type string `json:"type"`

	// Enum is the permitted value set; only for Type=enum.
	// +optional
	Enum []PreferenceEnumValue `json:"enum,omitempty"`

	// Pattern is a regexp every string value (or stringList item) must match.
	// Only for Type=string and Type=stringList.
	// +optional
	Pattern string `json:"pattern,omitempty"`

	// Default is the class-authored fallback used when neither an admin
	// global nor a user value is set; type-checked at reconcile.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Default *apiextensionsv1.JSON `json:"default,omitempty"`

	// Description is user-facing copy shown by the tools and forms.
	// +optional
	Description string `json:"description,omitempty"`

	// Visibility controls who may read this preference's resolved value.
	// "self" (the default): only the user themself — resolved for the
	// session's current turn author. "class": any session of this class may
	// also read it for a NAMED user (get_preferences' user argument), so the
	// agent can honor it when addressing that user (e.g. a notification
	// opt-out). Marking a preference "class" is the class author's
	// declaration that its value is safe to show anyone who can talk to
	// this agent.
	// +kubebuilder:validation:Enum=self;class
	// +optional
	Visibility string `json:"visibility,omitempty"`
}

type AgentClassStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// BoundChannels lists Channels in the same namespace that target this
	// AgentClass. Reconciled by the AgentClass controller.
	// +optional
	BoundChannels []BoundChannelRef `json:"boundChannels,omitempty"`

	// AgentSessionGrantsRef names the owned AgentSessionGrants CR in the
	// same namespace. Empty until the AgentClass reconciler has resolved
	// tools and created/updated the CR.
	// +optional
	AgentSessionGrantsRef string `json:"agentSessionGrantsRef,omitempty"`

	// EffectiveSettings is the resolved 4-tier settings snapshot (cluster →
	// namespace → class). Stamped by the AgentClass reconciler.
	// +optional
	EffectiveSettings *EffectiveSettings `json:"effectiveSettings,omitempty"`

	// OapInstall records provenance for an AgentClass installed from a
	// `.oap` bundle: the source it came from, its content digest, and when
	// it was installed. Absent for AgentClasses created by other means
	// (kubectl apply, wizard, etc.).
	// +optional
	OapInstall *OapInstallStatus `json:"oapInstall,omitempty"`

	// ResolvedSlots publishes, per declared slot, how a value becomes the
	// SpiceDB object id — derived by the reconciler from the tools, never
	// authored. Anything writing a slot grant ahead of the call (thread
	// seeding, an approval, a class default) must mint the identical id, and
	// a second authored copy of the chain would drift silently: the grant
	// would be written, never matched, and nothing would error.
	// +optional
	// +listType=atomic
	ResolvedSlots []ResolvedSlot `json:"resolvedSlots,omitempty"`

	// ResolvedPermissionTitles publishes, per (resourceType, permission) pair,
	// the human phrase declared on the SpiceDBPermission that names it — so the
	// runner can render an approval card without re-reading every MCPServer and
	// SpiceboxToolkit schema fragment. An OBSERVATION: the reconciler derives
	// this from the tools, never authors it. A pair with no declared Title is
	// OMITTED rather than published with an empty one, so the card can tell
	// "nobody wrote a title" (detokenize the handle) from "somebody declared it
	// blank" (which this field never represents).
	// +optional
	// +listType=atomic
	ResolvedPermissionTitles []ResolvedPermissionTitle `json:"resolvedPermissionTitles,omitempty"`

	// ResolvedResourceDisplays publishes, per resource type, the declared
	// icon/label/name presentation — the sibling of ResolvedPermissionTitles
	// for RESOURCE INSTANCE lines instead of permission lines. An OBSERVATION:
	// the reconciler derives this from the tools, never authors it. A type
	// with no declared Display is OMITTED rather than published empty, so the
	// card can tell "nobody declared a display" (fall back to the wire type
	// name) from "somebody declared one with blank fields".
	// +optional
	// +listType=atomic
	ResolvedResourceDisplays []ResolvedResourceDisplay `json:"resolvedResourceDisplays,omitempty"`

	// ResolvedResourceStandings publishes, per RESOURCE TYPE, how approval for it
	// is governed: `required` plus the permission an approver must hold, or
	// `session-only` when nothing local governs it.
	//
	// Keyed by resource type rather than by slot, unlike ResolvedSlots[].Standing.
	// A tool's permission check can name a type the class never declared as a
	// slot, and the approval router needs an answer for THAT type too — with no
	// entry it must refuse rather than fall back, which is the whole point of
	// removing the default.
	// +optional
	// +listType=atomic
	ResolvedResourceStandings []ResolvedResourceStanding `json:"resolvedResourceStandings,omitempty"`

	// FactSources publishes, per fact this class's slot preconditions read,
	// which tool can record it and what gates that tool — so an author can see
	// that a precondition is answerable, and see it BEFORE a session sits on an
	// undetermined verdict with nothing anywhere reporting a fault.
	//
	// Deliberately a warning surface rather than a gate. The impossible case
	// admission CAN prove — every producer of a fact gated by the very slot the
	// precondition holds shut — is refused with Valid=False. A fact NOTHING
	// declares a producer for is permanently undetermined too, but it is
	// indistinguishable at admission from one a channel kind or an
	// out-of-class tool legitimately supplies, so it lands here as
	// `state: absent` instead of failing the class.
	//
	// Read each entry's `state`, not its prose: an empty producedBy is both
	// `absent` and `envelope`, and an empty gatedBy is both `ungated` and
	// `unresolved`.
	//
	// An OBSERVATION: derived by the reconciler from the slots and the tools,
	// written set-on-change. Never SSA-applied — a volatile field a client
	// applied would churn ownership on every reconcile.
	// +optional
	// +listType=atomic
	FactSources []FactSource `json:"factSources,omitempty"`

	// UserlessInput reports whether a session of this class can be BORN with no
	// human on it: at least one bound Channel with an inbound role (input or
	// both) is of a kind whose Kind.UserAttributable() is false AND whose
	// Kind.SpawnsSessionOnInbound() is true — github and bento today, a webhook
	// payload and a cron tick, neither of which names a person who asked for
	// anything.
	//
	// Both halves, because every rule below is about a session the inbound
	// BROUGHT INTO EXISTENCE. A kind that spawns nothing (`agent`, the
	// conversational-delegation transport the operator binds to a child's class
	// for the life of a SubagentRequest) carries no human either, but every
	// session on it was pre-created with its own attribution and its own
	// standing — so it is NOT counted here, and none of the rules fire for it.
	//
	// One fact, several unrelated-looking rules. There is no user to attribute
	// a session to (spec.authz.session.interactPermission becomes required, and
	// so does the Channel's own spec.authzSubject); with none declared, the
	// membership of the class's single role=output Channel is derived as the
	// effective one (status.derivedSessionInteractPermission) and written to
	// SpiceDB per session; there is no inbound message to reply to (a
	// role=output Channel must carry its own destination, since nothing inbound
	// supplies one); and a session with no human on any leg acts as the service
	// subject its Channel declared. Published as an OBSERVATION so each of those
	// consumers reads the answer rather than re-listing this class's Channels
	// and re-deriving the predicate — a second copy of the walk is a second
	// place for the next rule to be forgotten, and a second place for one
	// consumer to disagree with the others about which Channels count.
	//
	// Derived by the AgentClass reconciler from the same Channel list that
	// produces status.boundChannels, on every reconcile, BEFORE the Valid
	// condition is decided. So a consumer that has observed Valid=True on this
	// class has observed a derived value: `false` there means "derived false",
	// not "not yet derived".
	// +optional
	UserlessInput bool `json:"userlessInput,omitempty"`

	// DerivedSessionInteractPermission is the subject-set the reconciler chose
	// as this class's interact policy because the class DECLARED none and its
	// input carries no human — the membership of the single role=output
	// Channel bound to it (slack_channel:<id>#member for a Slack destination).
	//
	// Present only when the derivation actually fired: the class left
	// spec.authz.session.interactPermission empty, status.userlessInput is
	// true, and exactly one bound role=output Channel yielded a membership
	// subject-set. A class that DECLARED the field leaves this empty — an
	// authored value is never mirrored here, so a non-empty value always reads
	// as "nobody wrote this; it was derived, and here is from what".
	//
	// It exists because the alternative was a fresh webhook-driven install
	// sitting at Valid=False until a human hand-patched the one value the
	// install already knew: the Slack channel the agent posts into is
	// per-install, so no bundle can carry it, and the people in that channel
	// are exactly the people who may interact with the sessions it announces.
	// Published on status rather than left implicit so an operator can see
	// WHICH subject-set is in force, and that it was not something they wrote,
	// without reading code.
	// +optional
	DerivedSessionInteractPermission string `json:"derivedSessionInteractPermission,omitempty"`
}

// EffectiveSessionInteractPermission is the subject-set that governs
// agentsession#participant for sessions of this class: the authored
// spec.authz.session.interactPermission when there is one, otherwise the value
// the reconciler derived onto status.
//
// THE accessor. An authored value always wins — derivation fills an absence,
// it never overrides an intent — and keeping that precedence in one place is
// what stops a consumer from reading only half of it and disagreeing about who
// may interact with a session.
func (ac *AgentClass) EffectiveSessionInteractPermission() string {
	if ac == nil {
		return ""
	}
	// onlyStartersInteract is a RESTRICTION and beats both arms below: neither a
	// declared widening nor a channel-derived membership may reopen the set.
	if ac.Spec.GetAuthz().GetSession().OnlyStartersInteract {
		return ac.StarterSubjectSet()
	}
	if declared := ac.Spec.GetAuthz().GetSession().InteractPermission; declared != "" {
		return declared
	}
	return ac.Status.DerivedSessionInteractPermission
}

// ResolvedPermissionTitle publishes the human phrase declared for one
// (resourceType, permission) pair, so the runner can render a card without
// re-reading every schema fragment. An OBSERVATION: the controller owns it.
type ResolvedPermissionTitle struct {
	// ResourceType is the SpiceDB resource type the permission is declared on.
	ResourceType string `json:"resourceType"`

	// Permission is the permission handle (SpiceDBPermission.Name).
	Permission string `json:"permission"`

	// Title is the phrase a human reads on an approval card, copied from the
	// declaring SpiceDBPermission.Title.
	Title string `json:"title"`

	// PlanningNote is the guidance the agent reads while declaring a plan,
	// copied from the declaring SpiceDBPermission.PlanningNote.
	//
	// It rides this entry rather than a list of its own because the key is
	// identical — (resourceType, permission) — and a second list would mean a
	// second walk of every toolkit and MCPServer producing the same keys, free
	// to drift from this one. The two strings differ in AUDIENCE, not in what
	// identifies them: Title is read by a human deciding, PlanningNote by the
	// agent planning.
	// +optional
	PlanningNote string `json:"planningNote,omitempty"`
}

// ResolvedResourceDisplay publishes the presentation declared for one
// resource type — SpiceDBResource.Display, flattened with its type name — so
// the runner can render a resource-instance line without re-reading every
// schema fragment. An OBSERVATION: the controller owns it.
type ResolvedResourceDisplay struct {
	// ResourceType is the SpiceDB resource type the display is declared on.
	ResourceType string `json:"resourceType"`

	// Name mirrors SpiceDBResourceDisplay.Name.
	Name string `json:"name,omitempty"`

	// Icon mirrors SpiceDBResourceDisplay.Icon.
	Icon string `json:"icon,omitempty"`

	// Label mirrors SpiceDBResourceDisplay.Label.
	Label string `json:"label,omitempty"`
}

// ResolvedSlot is the reconciler's answer to "how do I name an instance of this
// slot type", published for every writer of a slot grant.
type ResolvedSlot struct {
	// ResourceType is the SpiceDB resource type the slot declares.
	ResourceType string `json:"resourceType"`

	// Permission is the permission a grant on this slot confers.
	Permission string `json:"permission"`

	// ValueTransforms is the transform chain a free-form value passes through
	// to become the object id, in order, as declared by the tools that key this
	// type through resourceIDExpr. Empty means this slot is not value-keyed:
	// its ids are already distinct resources and are used as-is.
	// +optional
	// +listType=atomic
	ValueTransforms []string `json:"valueTransforms,omitempty"`

	// Standing is how an approval on this slot gets its authority:
	// `session-only` (the approver's decision is the authority) or `required`
	// (the approver must already hold the permission on the named instance).
	// Resolved from the type's schema fragments and the admin veto — an
	// OBSERVATION, so it lives here rather than on spec.
	// +optional
	Standing string `json:"standing,omitempty"`

	// ApproverPermission is the permission an approver must hold on the named
	// instance for their approval to count, published only when Standing is
	// `required`. Empty for `session-only`, where no local permission is
	// consulted and the session's own approvers decide.
	//
	// Published rather than assumed: the router used to hardcode `#owner`, which
	// made a type whose `owner` is a real computed permission indistinguishable
	// from one whose `owner` is a bare relation nothing ever writes.
	// +optional
	ApproverPermission string `json:"approverPermission,omitempty"`
}

// FactSource publishes, for ONE fact this class's slot preconditions read,
// where that fact can come from.
//
// A precondition is only as answerable as its facts. `facts.observed.x` is
// recorded by a tool's `observes` block; `facts.envelope.x` is derived by
// platform code from a signed payload. Either can be referenced by a predicate
// that nothing on this class will ever supply, and the failure of that is
// SILENT: every evaluation returns undetermined, the slot never binds, and the
// agent is handed a hint for a fact it cannot establish. Nothing errors.
//
// So the dependency is published rather than left implicit. An entry with an
// empty ProducedBy is the visible form of "nobody on this class records this" —
// which is a warning, not a refusal, because a fact may legitimately arrive
// from a channel kind or a tool that is not part of this class's spec. The
// refusal case is narrower and is decided elsewhere: a fact whose every
// producer is gated by the very slot the precondition holds shut.
//
// An OBSERVATION derived by the reconciler from the class's slots and tools,
// never authored, and written set-on-change.
type FactSource struct {
	// Provenance is the fact's namespace: `envelope` (derived by platform code
	// from a signed provider payload) or `observed` (derived from a response to
	// a call the agent shaped).
	Provenance string `json:"provenance"`

	// Name is the fact name, as `facts.<provenance>.<name>` addresses it.
	Name string `json:"name"`

	// State is the machine-readable answer to "what kind of entry is this",
	// and it is the field a consumer branches on. Detail says the same thing
	// in prose for a human; prose is not a discriminator, and a consumer
	// substring-matching Detail would break the first time the wording is
	// improved.
	//
	// It exists because two of the five states share an empty GatedBy and two
	// share an empty ProducedBy, so neither field can separate them:
	//
	//   gated      — a producer, and every call of it is gated (GatedBy lists
	//                the types).
	//   ungated    — a producer no permission check gates, so the call that
	//                records the fact is always allowed.
	//   unresolved — a real producer whose gate could NOT be determined. Not
	//                the same as ungated, and never treated as blocked.
	//   absent     — nothing on this class declares a producer for an observed
	//                fact. A warning, not a refusal.
	//   envelope   — an envelope fact, which no tool ever records. Normal, and
	//                deliberately not the `absent` warning.
	// +kubebuilder:validation:Enum=gated;ungated;unresolved;absent;envelope
	State string `json:"state"`

	// ProducedBy names the tool whose `observes` block records this fact.
	// EMPTY means nothing on this class declares a producer — see Detail for
	// which of the two reasons applies.
	//
	// One entry per producer: a fact several tools record yields several
	// entries, because a reader deciding whether a precondition is answerable
	// needs to know every call that could answer it, not just one.
	// +optional
	ProducedBy string `json:"producedBy,omitempty"`

	// GatedBy lists, sorted, every resource type a permission check on
	// ProducedBy names — that is, which slots must already be bound before the
	// call that records this fact is allowed. Empty when ProducedBy is
	// ungated, or when its gate could not be resolved; Detail says which.
	// +optional
	// +listType=atomic
	GatedBy []string `json:"gatedBy,omitempty"`

	// Detail explains, in prose meant for a person, an entry that is not a
	// plain gated producer: why nothing records the fact, why the producer is
	// ungated, or which gate could not be determined. Empty for the ordinary
	// case, where State, ProducedBy and GatedBy already say everything.
	//
	// Human-facing only. Branch on State.
	// +optional
	Detail string `json:"detail,omitempty"`
}

// The values of FactSource.State. Each is a state the reconciler actually
// produces — the CRD enum on the field is exactly this set, so a new one is
// added in both places or not at all.
const (
	// FactSourceStateGated is a producer every call of which is gated; GatedBy
	// names the types.
	FactSourceStateGated = "gated"
	// FactSourceStateUngated is a producer no permission check gates.
	FactSourceStateUngated = "ungated"
	// FactSourceStateUnresolved is a real producer whose gate could not be
	// determined. Distinct from ungated, and never treated as blocked.
	FactSourceStateUnresolved = "unresolved"
	// FactSourceStateAbsent is an observed fact nothing on this class records.
	FactSourceStateAbsent = "absent"
	// FactSourceStateEnvelope is an envelope fact, which no tool ever records.
	FactSourceStateEnvelope = "envelope"
)

// OapInstallStatus is the provenance record for an AgentClass installed
// from a `.oap` bundle.
type OapInstallStatus struct {
	// SourceRef identifies where the bundle came from (e.g. an OCI
	// reference or a local file path), in a form meaningful to SourceKind.
	// +optional
	SourceRef string `json:"sourceRef,omitempty"`

	// Digest is the content digest of the installed `.oap` bundle
	// (e.g. "sha256:...").
	Digest string `json:"digest"`

	// Version is the bundle's declared version, when the bundle manifest
	// carries one.
	// +optional
	Version string `json:"version,omitempty"`

	// SourceKind identifies the kind of SourceRef: "registry" (an OCI
	// reference) or "file" (a local .oap file or folder-source directory).
	SourceKind string `json:"sourceKind"`

	// InstalledAt is when this AgentClass was installed from the bundle.
	// +optional
	InstalledAt metav1.Time `json:"installedAt,omitempty"`
}

// AgentClassMCPServerRef references an MCPServer CR in the same namespace.
type AgentClassMCPServerRef struct {
	// Name is the LLM-prefix; pattern [a-z0-9_-]{1,32}.
	Name string `json:"name"`
	// Ref names the MCPServer CR in the same namespace.
	Ref string `json:"ref"`
	// CredentialRemap remaps a tool-declared credential name to a
	// differently-named credential in the agent's identity catalog —
	// the opt-in escape hatch when two tools declare the same credential
	// name but need different tokens. Keyed by the tool's declared name.
	// +optional
	CredentialRemap map[string]string `json:"credentialRemap,omitempty"`
}

// CredentialExplanationSpec is one AgentClass-authored reason a
// userPassthrough agent needs a credential. Surfaced as the "why" line
// on the credential-request prompt.
type CredentialExplanationSpec struct {
	// Credential is the final (post-remap) credential name this entry
	// describes, e.g. "github-token".
	Credential string `json:"credential"`

	// Reason is the agent-specific "why we need this" sentence rendered
	// under the credential's title.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// AgentClassSidecarToolboxRef references a SidecarToolbox CR in the same namespace.
type AgentClassSidecarToolboxRef struct {
	// Name is the LLM-prefix; pattern [a-z0-9_-]{1,32}.
	Name string `json:"name"`
	// Ref names the SidecarToolbox CR in the same namespace.
	Ref string `json:"ref"`
}

// AgentClassWorkspaceSourceRef references a WorkspaceSource CR in the same namespace.
type AgentClassWorkspaceSourceRef struct {
	// Ref names the WorkspaceSource CR in the AgentClass's namespace.
	Ref string `json:"ref"`
}

// AgentClassUIGrant authorizes browser-reachable tool calls for one AgentUI.
//
// GrantedTools is a deliberate act by whoever deploys the class. It is applied
// as a pure function of that decision — no wall-clock, no counters — so a
// byte-identical re-apply is an SSA no-op.
type AgentClassUIGrant struct {
	// Ref is the AgentUI name in the same namespace.
	Ref string `json:"ref"`

	// GrantedTools authorizes these tool names for direct browser invocation.
	// This is the ONLY place a deployment says yes. Note the asymmetry that
	// makes it matter: a readonly tool auto-runs at click frequency with no
	// human in the loop, while a side-effecting one already takes an approval
	// on every call — so this grant carries the most weight for readonly tools.
	//
	// The item pattern is the SAME one AgentUI.spec.tools carries — see that
	// field for why both are pinned to synthesize.NormalizeName's output
	// alphabet. A grant and a request that cannot be spelled differently
	// cannot silently disagree. As there: this holds for objects written AFTER
	// this upgrade only, since a CRD pattern is not retroactive, so the
	// runner's normalize step MUST NOT be deleted as redundant.
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9_-]{1,128}$`
	// +optional
	GrantedTools []string `json:"grantedTools,omitempty"`
}

func init() {
	SchemeBuilder.Register(&AgentClass{}, &AgentClassList{})
}

// ChannelsConfig governs how a channel-attached agent behaves.
type ChannelsConfig struct {
	// IdleTTL is how long an idle, channel-attached runner blocks in
	// await_user_message before exiting cleanly to phase=Idle.
	// Default 5m. Zero disables — the runner exits as soon as the
	// agent yields, useful for one-shot cron-driven agents.
	// +kubebuilder:default="5m"
	// +optional
	IdleTTL metav1.Duration `json:"idleTTL,omitempty"`

	// ArchiveAfter is how long an Idle, channel-attached session waits before
	// the operator transitions it to phase=Succeeded. After archive, the next
	// channel inbound for the same Channel + key spawns a fresh AgentSession
	// that inherits the prior session's memory turn-by-turn at creation.
	// Defaults to the operator's --default-channel-archive-after flag
	// (operator default: 4h). Set to 0 to disable archival (Idle indefinitely;
	// useful for tests).
	// +optional
	ArchiveAfter metav1.Duration `json:"archiveAfter,omitempty"`

	// StorageRetention is how long a TERMINAL (Succeeded/Failed) session's
	// workspace and snapshot-store PVCs are kept past FinishedAt before the
	// operator deletes them. The volumes are re-creatable scratch — a woken
	// or restarted session provisions fresh, empty ones on demand — and the
	// session record itself (transcript, memory, relationships) is untouched.
	// Defaults to the operator's --session-storage-retention flag (operator
	// default: 72h; flag 0 disables the sweep cluster-wide). A per-class
	// value here overrides the flag when > 0, same precedence ArchiveAfter
	// follows.
	// +optional
	StorageRetention metav1.Duration `json:"storageRetention,omitempty"`

	// SleepAfter is how long an Idle, channel-attached session keeps its pods
	// warm before the operator reaps ALL of them (sandboxes, runner, detector)
	// while keeping the session Idle and wakeable. On the next inbound the pods
	// are re-created lazily, re-mounting the shared workspace PVC. Must be well
	// under ArchiveAfter. Defaults to the operator's --default-session-sleep-after
	// flag (operator default: 10m). Set to 0 to keep pods warm for the whole Idle
	// window (never sleep).
	// +optional
	SleepAfter metav1.Duration `json:"sleepAfter,omitempty"`

	// ShowAssistantStream renders the agent's live LLM stream (free-form
	// text deltas + tool-use markers) into the channel placeholder while
	// the agent is working. Default false. With this off, the only things
	// that reach the channel between user turns are update_status (the
	// rolling caption / thinking-bubble) and update_plan (the structured
	// checklist). Turn on for debugging — the stream is verbose and
	// includes the agent's internal narrative + every tool call.
	// +kubebuilder:default=false
	// +optional
	ShowAssistantStream bool `json:"showAssistantStream,omitempty"`
}

// BoundChannelRef points at a Channel that targets this AgentClass.
// Reconciled by the AgentClass controller; not an authoritative source
// (the Channel CR is — this is a status mirror).
type BoundChannelRef struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// AuthzSlotMembershipDefault mirrors AuthzSlot.Membership's
// +kubebuilder:default marker. Slots is +listType=atomic, so SSA merges a
// re-applied slot as a REPLACE of the whole list, not field by field — a
// re-apply of a slot the bundle leaves membership unset on does NOT pick up
// the value the apiserver already defaulted onto the live object. An
// installer that completes membership with this constant before applying
// therefore writes what the apiserver would have defaulted anyway, which is
// what keeps a byte-identical re-install a true SSA no-op. See
// pkg/platform/oap/install/apply.go's completion of spec.authz.slots.
const AuthzSlotMembershipDefault = "frozen"

// AuthzSlotOccupancyDefault mirrors AuthzSlot.Occupancy's +kubebuilder:default.
// Completed by the installer before SSA apply for the same reason membership is
// (slots is +listType=atomic) — see pkg/platform/oap/install/apply.go.
const AuthzSlotOccupancyDefault = "single"

// AuthzSlotRebindDefault mirrors AuthzSlot.Rebind's +kubebuilder:default.
const AuthzSlotRebindDefault = "approval"

// AuthzSlot declares a TYPE of resource this agent operates on, gated by a
// permission — the instance axis of the two-axis ceiling: the permission
// Check'd at bind time, optional default IDs to bind at session start, and
// the tool args this binding auto-fills.
//
// Renamed from BoundEntityType: "slot" is what the plan gate, the approval
// flow and the SpiceDB grant all call it, and one concept carrying two names is
// a standing source of misreading.
type AuthzSlot struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*(/[a-z][a-z0-9_]*)*$`
	ResourceType string `json:"resourceType"`

	// +kubebuilder:validation:MinLength=1
	Description string `json:"description"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*$`
	Permission string `json:"permission"`

	// Permissions are the ADDITIONAL permissions a grant on this slot may
	// confer, beyond Permission.
	//
	// A slot used to be one (resourceType, permission) pair fixed at declaration
	// time. That could not represent a real type: git_repo is reached by read,
	// write, fetch AND push, and a plan that later needs `read` on a slot
	// declared for `push` had nowhere to bind it. The approval was recorded, no
	// grant could be written for it, and the very next call escalated again —
	// a human clicking Approve on the same amendment forever.
	//
	// Every permission named here gets its own `slot_grant_<permission>`
	// relation in the composed SpiceDB schema, which is what makes a grant for
	// it writable at all. Declaring a permission does NOT grant it: a grant is
	// still written only for what an approval actually named, still scoped to
	// the session, still expiring and revocable. This is the CEILING of what an
	// approval on this slot may ever bind.
	// +optional
	// +listType=atomic
	Permissions []string `json:"permissions,omitempty"`

	// +optional
	// +listType=atomic
	Defaults []string `json:"defaults,omitempty"`

	// +optional
	ExtractionPrompt string `json:"extractionPrompt,omitempty"`

	// +optional
	// +listType=atomic
	AutoFillArgs []AuthzSlotAutoFillArg `json:"autoFillArgs,omitempty"`

	// FillFrom names how an instance may come to occupy this slot. Unset means
	// today's behavior: class-pinned defaults bind at session start and the
	// extractor may propose instances — for every source EXCEPT trigger
	// (below), an unset list narrows nothing.
	//
	//   - default        class-pinned IDs, bound at session start
	//   - query/extract  the user names an instance in a message, unprompted
	//   - ask            the agent prompts and waits, and the user names it in
	//                    the reply. Binds through the SAME path as query — the
	//                    two differ in who started the exchange, not in how the
	//                    value binds — so declaring ask needs no extra tool: the
	//                    agent asks with respond_to_user and waits with
	//                    await_user_message.
	//   - channel_thread the channel seeds candidates from the thread it was
	//                    minted from, governed by AutoGrantFrom
	//   - metaagent      ambient scope intent, human-approved. NOT yet
	//                    implemented as a binding path: a slot naming only
	//                    metaagent binds nothing today.
	//   - observed       a tool result recorded a fact about the instance. NOT
	//                    yet implemented as a binding path: a slot naming only
	//                    observed binds nothing today.
	//   - trigger        the pipeline binds the instances the signed webhook
	//                    delivery names, at session mint. The ONE source that
	//                    is NEVER implied by an unset list: naming it puts a
	//                    session-mint SpiceDB grant behind no human and no
	//                    second gate, so a slot must name "trigger" here
	//                    explicitly, or it binds nothing from a trigger
	//                    however permissive (or absent) its other fillFrom
	//                    entries read.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:items:Enum=default;query;extract;ask;channel_thread;metaagent;observed;trigger
	FillFrom []string `json:"fillFrom,omitempty"`

	// AutoGrantFrom names WHOSE contributions to a thread may bind this slot
	// without an approval. It governs channel_thread seeding only.
	//
	// A thread is multi-author, so "found in the thread" would otherwise mean
	// "anyone's value" — an SSRF/exfil surface. Unset means owner: only the
	// session owner's contributions auto-bind, and everyone else's route to
	// approval. participants trusts any thread member and is only defensible
	// for a tightly-controlled channel. none trusts nobody, so every value the
	// thread offers goes in front of a human.
	//
	// "Trust nobody" is the explicit value `none` rather than an empty list
	// because an empty list is NOT representable here: omitempty drops it on
	// serialization, so `autoGrantFrom: []` reads back as unset and would
	// silently become owner — a policy written to be restrictive quietly
	// widening. When values disagree, the most restrictive wins.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:items:Enum=owner;participants;none
	AutoGrantFrom []string `json:"autoGrantFrom,omitempty"`

	// TriggerInstance is a CEL expression over the verified webhook delivery
	// ({event: string, payload: dyn}) yielding the resource ID this slot
	// binds at trigger time. The resource TYPE is always this slot's
	// resourceType — the expression yields only the id half. When set it is
	// authoritative for this slot; when unset, a channel kind that derives
	// instances from the delivery (TriggerSlotProvider) supplies them.
	// Compiled and validated at class admission; at delivery time an eval
	// error, non-string, or empty result binds nothing. Meaningful only when
	// fillFrom includes "trigger".
	// +optional
	TriggerInstance string `json:"triggerInstance,omitempty"`

	// Membership decides whether a grant follows the session's member set as it
	// grows, or is pinned to the set present when it was approved.
	//
	// Defaults to frozen. The appealing argument for dynamic is that the
	// approver grants to a SESSION and trusts its governance — but that only
	// holds while session membership is hard to obtain, and it is not: thread
	// adoption admits recent authors, and a channel-kind link can make one
	// tuple mean an entire channel. Following a set that grows on its own is
	// therefore an explicit choice, not the quiet default.
	//
	// Set dynamic where the session really is a small owned thread and the
	// approval card has shown the approver the concrete subject set.
	// +optional
	// +kubebuilder:validation:Enum=dynamic;frozen
	// +kubebuilder:default=frozen
	Membership string `json:"membership,omitempty"`

	// Occupancy decides how many instances may occupy this slot at once.
	//
	// single (default): at most one instance occupies the slot for the
	// session's life, recorded as a non-expiring slot_pin relationship. A
	// bind to a DIFFERENT instance is refused rather than silently added.
	// A second permission on the SAME instance, and a re-grant of the same
	// instance after its grants expired, are not drift and still bind.
	//
	// multi: the slot binds a set; each addition is gated as it is today.
	// No pin is written.
	//
	// UPGRADE NOTE. Before this field existed every slot behaved as multi, and a
	// class written then has no occupancy set, so the apiserver now defaults it
	// to single. A slot listing two or more defaults is then refused
	// (SlotDeclarationInvalid), and a thread seed or trigger binding several
	// instances of one type is refused rather than binding them all. Set
	// occupancy: multi on such slots before upgrading.
	// +optional
	// +kubebuilder:validation:Enum=single;multi
	// +kubebuilder:default=single
	Occupancy string `json:"occupancy,omitempty"`

	// Rebind decides what a bind to a DIFFERENT instance does on a filled
	// single-occupancy slot. Meaningful only when occupancy is single
	// (as triggerInstance is meaningful only with fillFrom "trigger").
	//
	// approval (default): refused, and the refusal names the route — an
	// approved plan amendment naming the new instance moves the pin and
	// revokes the old instance's grants.
	// never: refused unconditionally; retargeting requires a new session.
	// +optional
	// +kubebuilder:validation:Enum=approval;never
	// +kubebuilder:default=approval
	Rebind string `json:"rebind,omitempty"`

	// Requires are predicates over the facts a candidate arrived with, all of
	// which must be Satisfied before it may occupy this slot. Any Refused or
	// Undetermined verdict holds the slot closed.
	// +optional
	// +listType=atomic
	Requires []SlotPrecondition `json:"requires,omitempty"`
}

// SlotPrecondition is a predicate over the facts a candidate instance arrived
// with, which must hold before that instance may occupy the slot.
//
// Evaluation is TRI-STATE, and the third state is the point. Satisfied binds.
// Refused holds the slot closed and tells the agent why. UNDETERMINED — some
// referenced fact has not been recorded yet — also holds it closed, but is a
// different thing: nobody has answered the question, so the agent is told what
// to establish rather than that it was refused.
//
// Undetermined never binds. That is what makes the verdict monotonic: because
// facts are write-once, a precondition can move undetermined→satisfied or
// undetermined→refused, never satisfied→refused. So a bound slot cannot be
// invalidated later, and the gate is order-independent — "call the tool early
// and you are gated, late and you are not" is a real bug class in
// dispatch-time gates and this shape rules it out.
type SlotPrecondition struct {
	// CEL is a boolean predicate over `facts` and `slot`.
	//
	// `facts` is addressed as facts.<provenance>.<name>, where provenance is
	// `envelope` (derived by platform code from a signed provider payload) or
	// `observed` (derived from a response to a call the agent shaped). They are
	// deliberately separate namespaces: an author gating on the first must not
	// silently receive the second.
	//
	// `slot` carries {resourceType, resourceID} of the candidate being decided.
	//
	// has() is NOT available over facts. It would let an author turn "not yet
	// known" into a decidable boolean, collapsing the tri-state.
	// +kubebuilder:validation:MinLength=1
	CEL string `json:"cel"`

	// UndeterminedHint is shown to the AGENT while some referenced fact has not
	// been recorded. It is the only party who can fix that, by making the call
	// that establishes it — so this should name that call.
	//
	// Required, and the reason is recorded on PermissionCheck.ResourceIDHint:
	// without one the agent sees a raw denial that reads as a system fault and
	// tells it not to bother retrying.
	// +kubebuilder:validation:MinLength=1
	UndeterminedHint string `json:"undeterminedHint"`

	// RefusalMessage explains, in plain language, what was refused and why.
	//
	// It cannot be derived from CEL, and it must not be confused with the
	// agent's own justification for a call — that is text the agent authored,
	// and this is a gate the agent is subject to.
	// +kubebuilder:validation:MinLength=1
	RefusalMessage string `json:"refusalMessage"`

	// Approvers routes the WAIVER card for a Refused verdict, as SpiceDB subject-set
	// expressions (e.g. "agentsession:{ns}/{name}#approve"). The card asks a RISK
	// question ("accept running untrusted code from a fork?"), which is not the same
	// as "who may grant read on this resource" — and the resource's owner set is
	// often empty for a userless session, which turns an appealable gate into an
	// unappealable crash. Unset defaults to the slot's resolved standing.
	// +optional
	// +listType=atomic
	Approvers []string `json:"approvers,omitempty"`
}

// AuthzSlotAutoFillArg fills a tool argument from a bound slot instance.
type AuthzSlotAutoFillArg struct {
	// +kubebuilder:validation:MinLength=1
	ArgName string `json:"argName"`

	// +optional
	ToolNamePattern string `json:"toolNamePattern,omitempty"`
}

// AuthzBlock groups the three authorization panes on an AgentClass:
// tool-call authz, session-interact authz, and information-leakage authz.
type AuthzBlock struct {
	// ToolCalls authorizes per-tool dispatch.
	// +optional
	ToolCalls *ToolCallsAuthz `json:"toolCalls,omitempty"`

	// Session authorizes who may interact with sessions of this class.
	// +optional
	Session *SessionAuthz `json:"session,omitempty"`

	// InformationLeakage gates outbound channel messages against tainted
	// resources accessed by the agent during a session.
	// +optional
	InformationLeakage *InformationLeakagePolicy `json:"informationLeakage,omitempty"`

	// Scope enables dynamic per-session permission scope. Off by
	// default. When Enabled=false, tool dispatch behavior is
	// identical to today (no Layer 2 check, no Layer 3 SpiceDB
	// disallow tuples).
	// +optional
	Scope *ScopeSpec `json:"scope,omitempty"`

	// ApprovalTimeout is the consolidated approval-WAIT deadline for every
	// human-in-the-loop authz gate on this class: the tool-call approval, the
	// information-leakage share approval, and the cold-start scope approval all
	// source their wait from this single value. Defaults to 10 minutes — long
	// enough for an approver to click the Slack block. This is the approval-WAIT
	// timeout only; it does NOT cover liveness watchdogs (budget.maxDuration,
	// per-turn, silence, maxLlmLatencyMs).
	// +optional
	ApprovalTimeout *metav1.Duration `json:"approvalTimeout,omitempty"`

	// OwnerCeiling optionally bounds what bound Channels may declare as the
	// session owner. Absent (the common case) ⇒ no ceiling.
	// +optional
	OwnerCeiling *OwnerCeiling `json:"ownerCeiling,omitempty"`

	// Slots declares the resource TYPES this agent operates on, each gated by a
	// permission — the instance axis of the two-axis ceiling. At session start
	// defaults bind; per user message the runner extracts instances of these
	// types, Checks each against SpiceDB, and adds authorized ones to the
	// session's bound set. Bindings auto-fill matching tool args.
	//
	// Renamed and relocated from spec.boundEntities so the CRD, the plan gate,
	// the approval flow and the SpiceDB grant all call it the same thing.
	// +optional
	// +listType=atomic
	Slots []AuthzSlot `json:"slots,omitempty"`

	// PlanGate gates permissioned tool calls on membership in an approved
	// plan's active phase. Off by default. A class may set a mode STRICTER
	// than the tier default freely; it may only go below the tier's
	// SettingsLimits.MinPlanGateMode if no floor is set.
	// +optional
	PlanGate *PlanGateConfig `json:"planGate,omitempty"`

	// Trifecta refuses a DELEGATION whose closure combines untrusted input,
	// sensitive access and consequential action. Off by default.
	//
	// A property of the closure rather than of this class: a class that reads
	// untrusted content is fine, one that can write is fine, and the
	// delegation joining them is what this refuses.
	// +optional
	Trifecta *TrifectaConfig `json:"trifecta,omitempty"`

	// CrossAgentThreads lets another agent's messages reach this one inside a
	// human's thread. Off by default.
	// +optional
	CrossAgentThreads *CrossAgentThreadsConfig `json:"crossAgentThreads,omitempty"`

	// Metaagent configures WHEN the metaagent classifies a turn. Defaults to
	// mention-only; inline is opt-in per the field comment.
	// +optional
	Metaagent *MetaagentConfig `json:"metaagent,omitempty"`
}

// CrossAgentThreadsConfig lets one agent see and answer another inside a
// human's thread, bounded so the two cannot sustain a loop between themselves.
//
// Two controls, and the first ALONE is the documented attack: mention-gating
// supplies intent, and an injected "always @codebot when you reply" turns it
// into the loop. WakeBudget is the structural bound behind it.
type CrossAgentThreadsConfig struct {
	// Enabled admits ANOTHER agent's messages. It never admits this agent's
	// own posts, in any configuration — that is a self-loop, and the listener
	// decides self before it decides bot for exactly that reason.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// WakeBudget is how many agent-driven wakes this session accepts before a
	// HUMAN must speak again. Each human turn refills it; each agent-driven
	// wake spends one.
	//
	// ZERO — including unset — means agent→agent wakes are OFF, never
	// unlimited. The unsafe reading of an unset budget here is "no ceiling",
	// which is precisely the loop this bounds.
	//
	// Seeing is not bounded by it. A message from another agent ALWAYS appends
	// to this session's inbox; the budget governs only whether it also WAKES
	// the agent. So a session out of credit still sees the whole conversation
	// and answers when a person next speaks.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=32
	WakeBudget int `json:"wakeBudget,omitempty"`
}

// TrifectaConfig configures the closure trifecta check. Modes follow the same
// informationLeakage.mode / toolCalls.mode / planGate.mode convention, so an
// operator has one mental model across every gate.
type TrifectaConfig struct {
	// Mode selects how much of the check runs.
	//
	//   disabled  — off entirely.
	//   logging   — legs are derived and the verdict recorded, nothing refused.
	//               Two-leg near-misses are the dataset this mode exists for.
	//   enforcing — a three-leg delegation is refused.
	//
	// Its OWN mode, and deliberately not reached through toolCalls.mode: a
	// control that disappears when an operator disables a different permission
	// check is not a control. An operator turning off tool-call authz has not
	// thereby decided that untrusted content may drive a write.
	// +kubebuilder:validation:Enum=disabled;logging;enforcing
	// +optional
	Mode string `json:"mode,omitempty"`

	// NeverConsequential declares that this class must never hold leg C — it
	// may read, but must never be able to act.
	//
	// A DECLARED ceiling validated at admission against the DERIVED permission
	// surface, mirroring how budget already works: the class declares, the
	// controller derives, and a contradiction marks the class Valid=False
	// rather than being silently ignored. A class that declares this and holds
	// a readwrite or external handle is a configuration mistake its author
	// wants to hear about at apply time, not a delegation that quietly fails
	// later.
	// +optional
	NeverConsequential bool `json:"neverConsequential,omitempty"`
}

// PlanGateConfig configures the plan gate. Modes follow the existing
// informationLeakage.mode / toolCalls.mode convention.
type PlanGateConfig struct {
	// Mode selects how much of the gate runs.
	//
	//   disabled  — off entirely; no prompt change, no plan schema surfaced.
	//   logging   — the gate runs in full: enumeration, ceiling computation,
	//               approver resolution, severity, card rendering and the
	//               append-only write. What it holds back is CEILINGS and
	//               APPROVALS — the card is logged instead of published, so
	//               nothing is approved and no grant is written, and a call that
	//               overruns a declared phase is RECORDED rather than refused.
	//
	//               A call made with NO plan at all is still refused (see
	//               RequirePlan). That is not an exception to the mode so much as
	//               its precondition: without a declared plan there are no
	//               ceilings to record and the mode measures nothing.
	//   enforcing — the card is published and the call blocks on the outcome.
	//
	// +kubebuilder:validation:Enum=disabled;logging;enforcing
	// +kubebuilder:default=disabled
	// +optional
	Mode string `json:"mode,omitempty"`

	// RequirePlan denies every permissioned call made before a plan is declared.
	// Without it, an agent that simply never calls update_plan falls through the
	// implicit-phase rule and is unconstrained — i.e. "don't plan" is a total
	// bypass.
	//
	// It bites in LOGGING mode too, which is the one place this differs from
	// everything else the gate does. "You must declare a plan" and "you must stay
	// within its ceiling" are separable claims; only the second is what logging
	// mode holds back. Welding them together made logging unable to produce the
	// dataset it exists for — agents told they MUST plan simply did not, because
	// in logging nothing depended on it, and no wording fixes a consequence the
	// code does not implement.
	//
	// So: with RequirePlan, a session under logging still refuses calls made with
	// no plan at all, and still records-but-permits calls that overrun a declared
	// ceiling. That yields real phases, ceilings, severity and approver data
	// without switching on the enforcement the data is meant to justify.
	// UNSET derives from Mode: required whenever the gate runs at all. Turning
	// the gate on IS the intent to make agents plan, and leaving this an
	// independent opt-in defaulting false meant a cluster could run
	// `mode: logging`, look gated, and collect nothing — which is what happened
	// across five live sessions, every call landing on the implicit phase
	// because nothing required a plan.
	//
	// Explicit `false` remains an opt-out, because gate-on-but-observe-only is a
	// real rollout stage: it records which handles calls actually resolve to
	// without changing what the agent may do. Explicit `true` under
	// `mode: disabled` is still false — a gate that does not run cannot refuse
	// anything, and honouring it would promise a denial that never arrives.
	//
	// Pointer rather than a kubebuilder default: a static default cannot express
	// "depends on another field", and it would erase the difference between
	// unset and a deliberate false.
	//
	// UPGRADE NOTE. This field previously carried `+kubebuilder:default=false`,
	// so every AgentClass created under the older CRD has an explicit `false`
	// PERSISTED — which now reads as the observe-only opt-out rather than as
	// unset. Such a class keeps the old behaviour after an upgrade and will not
	// require a plan. Re-applying the class (or deleting the field) is what
	// restores derivation; there is no way to distinguish a stamped default from
	// a deliberate choice after the fact.
	// +optional
	RequirePlan *bool `json:"requirePlan,omitempty"`

	// Limits bound plan size. The defaults are deliberately expansive: they
	// exist to stop a runaway or hostile plan from DoS-ing the approver and
	// producing an unrenderable card, NOT to shape normal authoring.
	// +optional
	Limits *PlanLimits `json:"limits,omitempty"`

	// Rendering bounds what a single approval card shows or auto-approves.
	// +optional
	Rendering *PlanRendering `json:"rendering,omitempty"`

	// Examples are worked plans this class's author supplies, rendered after the
	// one the runtime derives from the session's own permission surface.
	//
	// Optional, and most classes want none: the derived example already speaks
	// the agent's own handles and shows the shape that costs one approval. These
	// are for a class whose GOOD plan is not obvious from its surface alone —
	// where the order of phases matters, or where two resource types must be
	// named together, or where the natural split is by audience rather than by
	// read-then-write.
	//
	// They are TRUSTED text, at the same level as spec.systemPrompt: an operator
	// authors them, never the agent. But unlike the system prompt they are
	// checked — every handle an example names must exist on this class's own
	// surface, or the class goes Valid=False naming the offender. The prompt
	// tells agents that a handle off the declarable list is silently dropped,
	// and an EXAMPLE carrying such a handle would teach exactly that failure to
	// every plan the agent writes. Measured, planners transcribe these examples
	// closely, so a wrong one is not inert.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=5
	Examples []PlanExample `json:"examples,omitempty"`
}

// PlanExample is one worked plan an AgentClass author supplies to guide
// planning, in the same shape update_plan takes.
type PlanExample struct {
	// Task is the one-line request this plan answers — the "when you are asked
	// to X" half. Without it an example shows a shape with no occasion, and the
	// agent cannot tell which of several examples applies.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=200
	Task string `json:"task"`

	// Phases are the plan itself. Rendered as the JSON an agent would pass to
	// update_plan, because that is the form it copies.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=6
	// +listType=atomic
	Phases []PlanExamplePhase `json:"phases"`
}

// PlanExamplePhase is one phase of an authored example.
type PlanExamplePhase struct {
	// ID is the phase's identifier, as update_plan takes it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=60
	ID string `json:"id"`

	// Label is the human phrase for the phase.
	// +optional
	// +kubebuilder:validation:MaxLength=120
	Label string `json:"label,omitempty"`

	// Permissions are the wire-format handles this phase declares
	// ("perm:<permission>:<resourceType>" or "tool:<name>"). Each is checked
	// against the class's surface at admission.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=12
	Permissions []string `json:"permissions,omitempty"`

	// Slots are the resource TYPES this phase names an instance of. Types only:
	// an authored example must not carry a concrete instance id, because a
	// planner that copies one would address an object belonging to whoever the
	// example was written about.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=6
	Slots []string `json:"slots,omitempty"`
}

// PlanLimits bound plan size. Exceeding one is a tool error naming the limit —
// the agent re-plans smaller — never a silent truncation, which would drop
// declared reach while leaving the plan looking complete.
type PlanLimits struct {
	// +kubebuilder:default=50
	// +optional
	MaxPhases int32 `json:"maxPhases,omitempty"`

	// MaxPermissionsPerPhase is naturally bounded by the permission surface (a
	// phase cannot declare a handle that does not exist), so this is a backstop
	// rather than a real constraint.
	// +kubebuilder:default=64
	// +optional
	MaxPermissionsPerPhase int32 `json:"maxPermissionsPerPhase,omitempty"`

	// MaxSlotsPerPhase bounds the instance axis, which is genuinely unbounded
	// (a thread can carry many URLs), hence the larger default.
	// +kubebuilder:default=128
	// +optional
	MaxSlotsPerPhase int32 `json:"maxSlotsPerPhase,omitempty"`
}

// PlanRendering bounds presentation and auto-approval — never the ceiling
// itself. Exceeding MaxSingleCardHandles splits a card; it never drops a handle.
// Severity overrides these in one direction only: an Elevated or Severe card
// renders every handle regardless, because the fold is exactly wrong on the
// cards that matter most.
type PlanRendering struct {
	// MaxNamedApprovers is the largest approver population rendered by name
	// before falling back to a count or a channel-supplied description.
	// +kubebuilder:default=5
	// +optional
	MaxNamedApprovers int32 `json:"maxNamedApprovers,omitempty"`

	// MaxSingleCardHandles bounds a phase ceiling shown on one card; a phase
	// exceeding it is split rather than folded behind "+N more", so the
	// approver sees what they approve.
	// +kubebuilder:default=16
	// +optional
	MaxSingleCardHandles int32 `json:"maxSingleCardHandles,omitempty"`

	// MaxAutoApproveHandles bounds the UNION of all handles auto-approved
	// without a human across a session — the tier-0 budget.
	// +kubebuilder:default=8
	// +optional
	MaxAutoApproveHandles int32 `json:"maxAutoApproveHandles,omitempty"`
}

// ResolvedApprovalTimeout returns ApprovalTimeout, or 10 minutes when the block
// or the field is nil. This is the single source for every approval-WAIT
// deadline (tool-call, information-leakage share, cold-start scope).
func (a *AuthzBlock) ResolvedApprovalTimeout() time.Duration {
	if a == nil || a.ApprovalTimeout == nil {
		return 10 * time.Minute
	}
	return a.ApprovalTimeout.Duration
}

// ToolCallsAuthz configures per-tool-call authorization.
type ToolCallsAuthz struct {
	// +kubebuilder:validation:Enum=currentRequester;startedBy;both
	// +kubebuilder:default=currentRequester
	// +optional
	Subject string `json:"subject,omitempty"`

	// +kubebuilder:validation:Enum=permissive;enforcing;disabled
	// +kubebuilder:default=enforcing
	// +optional
	Mode string `json:"mode,omitempty"`
}

// SessionAuthz configures who may START a session of this class and who may
// INTERACT with one once it exists.
type SessionAuthz struct {
	// InteractPermission is a SpiceDB subject-set ("<type>:<id>#<relation>",
	// e.g. "group:engineering#member") written as agentsession#participant on
	// every new session of this class. It WIDENS interact to that population;
	// it never restricts the starter, who always holds interact as started_by.
	// Mutually exclusive with OnlyStartersInteract.
	// +optional
	InteractPermission string `json:"interactPermission,omitempty"`

	// AllowedStarters lists who may start a session of this class, as SpiceDB
	// subject refs: "user:<canonical>" or a subject-set "group:<id>#member".
	// Written by the AgentClass controller as agentclass#starter tuples; the
	// set on the class IS the set in SpiceDB (entries removed here are deleted
	// there). Empty means no start gate — anyone the channel admits may start,
	// as before.
	//
	// Enforced at the AgentSession reconciler for EVERY entry path (channel,
	// browser, delegation, restart): a session whose started_by does not hold
	// the gate permission is failed before any runner pod exists, and the
	// person is told. Values are canonical ids, never raw emails — a bundle
	// install question canonicalizes on the way in.
	//
	// A start gate makes a class HUMAN-ENTRY-ONLY: every arm of the gate
	// resolves a person, so a session nobody started — a cron, a webhook — is
	// refused structurally. A class that declares an allowlist therefore cannot
	// be bound to an input Channel that carries no person; the controller
	// refuses that combination rather than let every such session fail.
	//
	// Removal is PROSPECTIVE. Dropping someone from this list deletes their
	// standing to start NEW sessions; it does not end the ones already running,
	// which keep their verdict as a status condition. Who may keep TALKING to a
	// live session is OnlyStartersInteract's question, not this field's.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	AllowedStarters []string `json:"allowedStarters,omitempty"`

	// PlatformAdminsMayStart, when false, makes the gate EXPLICIT: only
	// AllowedStarters may start, and the platform-admin arm of
	// agentclass#start_session does not apply (agentclass#start_explicit is
	// checked instead). Admins who want to use such an agent list themselves.
	// Default true. Meaningful only with a non-empty AllowedStarters — the
	// controller refuses false with an empty list, since nobody could start.
	// +optional
	PlatformAdminsMayStart *bool `json:"platformAdminsMayStart,omitempty"`

	// OnlyStartersInteract fixes the session interact set to this class's
	// starters: the applied interact permission becomes
	// "agentclass:<ns>/<name>#starter", so the existing Interact gate admits
	// exactly AllowedStarters and nobody else — no channel-derived membership,
	// no declared widening. Requires a non-empty AllowedStarters and an empty
	// InteractPermission. Rename-safe: the subject-set is computed from the
	// class's own name at use time, never stored.
	// +optional
	OnlyStartersInteract bool `json:"onlyStartersInteract,omitempty"`

	// ArtifactVisibility declares who may VIEW the artifacts produced by this
	// class's sessions. "session" (the default, and what an empty value means)
	// keeps today's audience: the session's interact set plus platform admins.
	// "organization" opts the class's sessions into the org-wide audience —
	// any user who can authenticate against the cluster's IdP may view the
	// artifacts (view only: never the conversation, the plan, or send
	// standing, and a session's deny list still beats it).
	//
	// Level-triggered, both ways: the AgentSession reconciler re-levels the
	// agentsession#artifact_org_viewer wildcard tuple from this value on every
	// reconcile — including reconciles of completed sessions — so flipping to
	// "organization" exposes existing sessions' artifacts, and flipping back
	// revokes org-wide access everywhere.
	// +kubebuilder:validation:Enum=session;organization
	// +optional
	ArtifactVisibility string `json:"artifactVisibility,omitempty"`
}

// The ArtifactVisibility values. An absent field means ArtifactVisibilitySession.
const (
	ArtifactVisibilitySession      = "session"
	ArtifactVisibilityOrganization = "organization"
)

// OrgWideArtifactVisibility reports whether sessions of this class widen
// their artifacts' view audience to the whole IdP-authenticated organization.
// Nil-safe; only an explicit "organization" opts in.
func (s *SessionAuthz) OrgWideArtifactVisibility() bool {
	return s != nil && s.ArtifactVisibility == ArtifactVisibilityOrganization
}

// OwnerCeiling caps Channel-declared owner policy for a class.
type OwnerCeiling struct {
	// StarterOnly forbids Channels from declaring explicit/foreign-group
	// owners — owner must resolve to the session's starting user.
	// +optional
	StarterOnly bool `json:"starterOnly,omitempty"`

	// Fixed pins the owner to this subject/subject-set ref regardless of
	// Channel policy. Mutually exclusive with StarterOnly.
	// +optional
	Fixed string `json:"fixed,omitempty"`
}

// GetOwnerCeiling returns the class's OwnerCeiling or nil, nil-safe across an
// absent Authz block.
func (s *AgentClassSpec) GetOwnerCeiling() *OwnerCeiling {
	if s == nil || s.Authz == nil {
		return nil
	}
	return s.Authz.OwnerCeiling
}

// rosterPinRE matches the one pin form a roster entry may carry. Anything
// else after an "@" is NOT a pin: it stays part of the literal entry, and the
// CRD-level item pattern refuses it at admission — SplitRosterEntry never
// guesses at partially-formed pins.
var rosterPinRE = regexp.MustCompile(`^(.+)@(sha256:[a-f0-9]{64})$`)

// SplitRosterEntry splits a spec.subagents entry into its class name and
// optional digest pin: "reviewer@sha256:<hex>" → ("reviewer", "sha256:<hex>", true).
func SplitRosterEntry(entry string) (name, digest string, pinned bool) {
	if m := rosterPinRE.FindStringSubmatch(entry); m != nil {
		return m[1], m[2], true
	}
	return entry, "", false
}

// RosterNames returns the deduplicated roster in declaration order. Nil for a
// class that declares none. Pinned entries have their pins stripped.
func (s *AgentClassSpec) RosterNames() []string {
	if s == nil || len(s.Subagents) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(s.Subagents))
	out := make([]string, 0, len(s.Subagents))
	for _, n := range s.Subagents {
		name, _, _ := SplitRosterEntry(n)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// RosterPins returns, per roster name, the distinct digest pins declared for
// it, in declaration order. A name with no pinned entry is absent. More than
// one distinct pin for a name is contradictory; the SubagentRequest
// controller refuses delegation to such a name rather than choosing one.
func (s *AgentClassSpec) RosterPins() map[string][]string {
	if s == nil || len(s.Subagents) == 0 {
		return nil
	}
	pins := map[string][]string{}
	for _, entry := range s.Subagents {
		name, digest, pinned := SplitRosterEntry(entry)
		if !pinned || slices.Contains(pins[name], digest) {
			continue
		}
		pins[name] = append(pins[name], digest)
	}
	return pins
}

// PermittedSubagentModes returns the SubagentRequest modes this class may
// delegate to name in, single_turn first and declared widenings after it in
// declaration order.
//
// Nil -- nothing permitted at all -- for a name that is not on the roster, so a
// mode check fails closed on its own rather than depending on a membership
// check having run first.
//
// single_turn is always present for a roster member. It is the one mode that
// provisions strictly less than the others (no Channel at all), so it is what
// "the agent may choose narrower" means concretely. Nothing else is implied --
// chat does not imply task -- because the difference between those two is an
// attack-surface declaration, and inferring one from the other would grant a
// surface the operator never wrote.
//
// A declared mode that is not one of the three known values is DROPPED here
// rather than returned: an unrecognized string must never widen anything. It
// is not silently tolerated overall -- the AgentClass controller refuses the
// same value at validation time (Valid=False/RosterInvalid), which is where an
// operator sees it.
func (s *AgentClassSpec) PermittedSubagentModes(name string) []string {
	if s == nil || name == "" {
		return nil
	}
	if !slices.Contains(s.RosterNames(), name) {
		return nil
	}
	out := []string{SubagentModeSingleTurn}
	for _, m := range s.SubagentModes[name] {
		if !IsSubagentMode(m) || slices.Contains(out, m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// InformationLeakagePolicy configures the information-leakage gate. Fields
// are optional; absent fields use the defaults documented on the Resolved*
// helpers (mode defaults to "disabled" when the entire block is nil; to
// "enforcing" when the block exists but Mode is unset is NOT desired —
// callers that want enforcing must set it explicitly).
type InformationLeakagePolicy struct {
	// Mode controls how the gate reacts when a potential leak is detected.
	// +kubebuilder:validation:Enum=enforcing;logging;disabled
	// +kubebuilder:default=enforcing
	// +optional
	Mode string `json:"mode,omitempty"`

	// ApprovalTTL is the lifetime of a SpiceDB grant written on approve.
	// +optional
	ApprovalTTL *metav1.Duration `json:"approvalTTL,omitempty"`

	// OnUnsupportedChannel determines binding-time behavior when a Channel's
	// kind has no AudienceResolver capability.
	// +kubebuilder:validation:Enum=blockBinding;logOnly;bypass
	// +kubebuilder:default=blockBinding
	// +optional
	OnUnsupportedChannel string `json:"onUnsupportedChannel,omitempty"`

	// SingleUserBypass allows SingleUser-capability channels to skip the
	// gate (the audience is trivially the requester themselves).
	// +optional
	SingleUserBypass *bool `json:"singleUserBypass,omitempty"`

	// LoggingNoticeToRequester sends an ephemeral notice to the requester
	// on every would-block event while Mode=logging.
	// +optional
	LoggingNoticeToRequester *bool `json:"loggingNoticeToRequester,omitempty"`
}

// ResolvedMode returns Mode, or "disabled" when the policy block is nil or
// Mode is unset.
func (p *InformationLeakagePolicy) ResolvedMode() string {
	if p == nil || p.Mode == "" {
		return "disabled"
	}
	return p.Mode
}

// ResolvedApprovalTTL returns ApprovalTTL, or 10 minutes when unset.
func (p *InformationLeakagePolicy) ResolvedApprovalTTL() time.Duration {
	if p == nil || p.ApprovalTTL == nil {
		return 10 * time.Minute
	}
	return p.ApprovalTTL.Duration
}

// ResolvedOnUnsupportedChannel returns OnUnsupportedChannel, or "blockBinding"
// when unset.
func (p *InformationLeakagePolicy) ResolvedOnUnsupportedChannel() string {
	if p == nil || p.OnUnsupportedChannel == "" {
		return "blockBinding"
	}
	return p.OnUnsupportedChannel
}

// ResolvedSingleUserBypass returns SingleUserBypass, or true when unset.
func (p *InformationLeakagePolicy) ResolvedSingleUserBypass() bool {
	if p == nil || p.SingleUserBypass == nil {
		return true
	}
	return *p.SingleUserBypass
}

// ResolvedLoggingNoticeToRequester returns LoggingNoticeToRequester, or false
// when unset: the below-threshold FYI notice is opt-in (an agent must set
// loggingNoticeToRequester=true to receive it), so migrating the notice onto
// the generic interaction model does not change default behavior.
func (p *InformationLeakagePolicy) ResolvedLoggingNoticeToRequester() bool {
	if p == nil || p.LoggingNoticeToRequester == nil {
		return false
	}
	return *p.LoggingNoticeToRequester
}

// ScopeSpec configures the dynamic per-session scope behavior for
// AgentSessions of this class. Off by default; when enabled, the runner
// evaluates a Layer 2 SessionScope memory doc before SpiceDB Checks,
// and SpiceDB gains per-session disallow relations that always
// override allow paths.
type ScopeSpec struct {
	// Enabled turns the feature on for sessions of this class.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// ColdStart governs how a genuinely-new session's first user message is
	// handled:
	//   extractAndApprove   — extract {scope, cleanedTask}; post the approval
	//                         block; agent blocks until the approver resolves.
	//   extractAndAutoApply — extract; apply scope; run the cleaned task; no
	//                         approval click.
	//   off                 — no cold-start extraction; run the raw first turn
	//                         unmodified (mid-session @metaagent still works).
	// Ignored when Enabled=false.
	// +kubebuilder:validation:Enum=extractAndApprove;extractAndAutoApply;off
	// +kubebuilder:default=extractAndApprove
	// +optional
	ColdStart string `json:"coldStart,omitempty"`

	// MaxLLMLatencyMs caps the combined extractor+composer LLM latency
	// before authzd gives up and posts a CannotAddressMessage.
	// +kubebuilder:default=5000
	// +optional
	MaxLLMLatencyMs int32 `json:"maxLlmLatencyMs,omitempty"`

	// ComposerModelOverride and ExtractorModelOverride allow per-class
	// LLM model selection; empty falls back to the authzd default.
	// +optional
	ComposerModelOverride string `json:"composerModelOverride,omitempty"`
	// +optional
	ExtractorModelOverride string `json:"extractorModelOverride,omitempty"`
}

// GetAuthz returns Spec.Authz or a zero-value pointer; never returns nil.
func (s *AgentClassSpec) GetAuthz() *AuthzBlock {
	if s.Authz == nil {
		return &AuthzBlock{}
	}
	return s.Authz
}

// GetScope returns Spec.Authz.Scope or a zero-value pointer; never
// returns nil. Callers can read fields directly.
func (s *AgentClassSpec) GetScope() *ScopeSpec {
	a := s.GetAuthz()
	if a.Scope == nil {
		return &ScopeSpec{}
	}
	return a.Scope
}

// GetToolCalls returns Authz.ToolCalls or a zero-value pointer; never returns nil.
func (b *AuthzBlock) GetToolCalls() *ToolCallsAuthz {
	if b == nil || b.ToolCalls == nil {
		return &ToolCallsAuthz{}
	}
	return b.ToolCalls
}

// GetSession returns Authz.Session or a zero-value pointer; never returns nil.
func (b *AuthzBlock) GetSession() *SessionAuthz {
	if b == nil || b.Session == nil {
		return &SessionAuthz{}
	}
	return b.Session
}

// BoundEntityType and EntityAutoFillArg are the pre-rename names for AuthzSlot and
// AuthzSlotAutoFillArg.
//
// Kept as aliases so the rename lands as ONE reviewable diff rather than 200
// mechanical edits mixed into it. They are type aliases, not new types, so the
// two spellings are the same type to the compiler and no conversion is needed.
//
// Deprecated: use AuthzSlot / AuthzSlotAutoFillArg.
type (
	BoundEntityType   = AuthzSlot
	EntityAutoFillArg = AuthzSlotAutoFillArg
)

// GetSlots returns the instance-axis slot declarations.
//
// The ONE place any consumer reads them, and it deliberately does not fall back
// to the deprecated spec.boundEntities: a class still using the old spelling is
// rejected by the reconciler (ReasonBoundEntitiesRenamed), so falling back here
// would resurrect exactly the silent behavior that rejection exists to prevent.
// A reader that cannot reach the old field cannot accidentally honour it.
func (s *AgentClassSpec) GetSlots() []AuthzSlot {
	if s == nil || s.Authz == nil {
		return nil
	}
	return s.Authz.Slots
}

// SlotResourceTypes lists the resource types this class opted into on the
// instance axis — what a plan phase's slot requests are validated against.
//
// Nil means the class does not use the instance axis, so every slot a phase
// requests is undeclared and gets dropped at freeze. That direction is
// deliberate: a slot request becomes a SpiceDB grant when the plan is approved,
// and a class that never declared the type never opted into having grants
// written against it.
func (s *AgentClassSpec) SlotResourceTypes() []string {
	slots := s.GetSlots()
	if len(slots) == 0 {
		return nil
	}
	out := make([]string, 0, len(slots))
	for _, sl := range slots {
		if sl.ResourceType != "" {
			out = append(out, sl.ResourceType)
		}
	}
	return out
}

// SlotPermissions maps each declared slot resource type to the permission a
// grant on it confers.
//
// Standing on a slot is a question about the PAIR, never the type alone:
// holding `read` on a repository says nothing about a slot whose grant confers
// `write`, and checking the type without its permission would call the second
// delegable because the approver happened to hold the first.
func (s *AgentClassSpec) SlotPermissions() map[string]string {
	slots := s.GetSlots()
	if len(slots) == 0 {
		return nil
	}
	out := make(map[string]string, len(slots))
	for _, sl := range slots {
		if sl.ResourceType != "" && sl.Permission != "" {
			out[sl.ResourceType] = sl.Permission
		}
	}
	return out
}

// MetaagentTrigger names WHEN the metaagent classifies a turn, and whether it
// acts on what it finds.
//
// Three operating points, ordered by how much they do. The default is the
// conservative one deliberately: the metaagent turns an utterance into a
// session change, so widening WHEN it looks widens how often untrusted text
// reaches an authorization surface — even though the classifier itself can
// grant nothing.
//
// All three leave the EXPLICIT path alone. An @metaagent mention, and the
// session-start trigger, behave identically under every value; this setting
// governs the AMBIENT half only. A user who addresses the metaagent directly is
// making a request, and silently ignoring it in one mode would be surprising.
const (
	// MetaagentTriggerMention is the default: the ambient path is off entirely.
	// The metaagent runs only when addressed explicitly, plus at session start.
	// This is today's behavior.
	MetaagentTriggerMention = "mention"

	// MetaagentTriggerShadow classifies every inbound turn and APPLIES NOTHING.
	//
	// The measurement mode, and the reason it exists is that the alternative is
	// measuring in production. The prefilter is a keyword heuristic that trades
	// recall for cost and whose miss rate is unmeasured; the classifier behind
	// it is an LLM reading untrusted text. Shadow runs both for real and records
	// what WOULD have happened — prefiltered-out versus extractor-invoked, and
	// which decision came back — while changing nothing about the session.
	//
	// So a cluster can turn this on for a few agents, read the numbers, and
	// decide whether inline is worth it. That is the same discipline the plan
	// gate's logging mode follows, for the same reason.
	MetaagentTriggerShadow = "shadow"

	// MetaagentTriggerInline classifies every inbound turn AND acts on it.
	//
	// Everything downstream still applies: a widening needs approval, standing
	// still gates the surface, and the admin ceiling still clamps. What changes
	// is only how often the classifier runs.
	MetaagentTriggerInline = "inline"
)

// MetaagentConfig configures the metaagent's ambient trigger.
type MetaagentConfig struct {
	// Trigger is when the metaagent classifies a turn, and whether it acts.
	//
	//   mention (default) — ambient off; explicit @metaagent only.
	//   shadow            — classify every turn, apply nothing, record it.
	//   inline            — classify every turn and act.
	//
	// +kubebuilder:validation:Enum=mention;shadow;inline
	// +kubebuilder:default=mention
	// +optional
	Trigger string `json:"trigger,omitempty"`
}

// GetMetaagentTrigger returns the resolved trigger, defaulting to mention.
//
// Fail-safe on an unrecognized value: anything that is not exactly shadow or
// inline resolves to mention, so a typo NARROWS when the metaagent looks rather
// than silently widening it. The CRD enum rejects a bad value at admission;
// this covers one that predates the enum or arrives from an older tier.
func (m *MetaagentConfig) GetMetaagentTrigger() string {
	if m == nil {
		return MetaagentTriggerMention
	}
	switch m.Trigger {
	case MetaagentTriggerShadow, MetaagentTriggerInline:
		return m.Trigger
	default:
		return MetaagentTriggerMention
	}
}

// ClassifiesAmbient reports whether this trigger runs the classifier on every
// inbound turn — true for both shadow and inline.
func (m *MetaagentConfig) ClassifiesAmbient() bool {
	return m.GetMetaagentTrigger() != MetaagentTriggerMention
}

// AppliesAmbient reports whether an ambient classification may CHANGE anything.
// Only inline does; shadow runs the whole path and stops short of the effect.
func (m *MetaagentConfig) AppliesAmbient() bool {
	return m.GetMetaagentTrigger() == MetaagentTriggerInline
}

// ResolvedResourceStanding is how approval for one resource type is governed,
// resolved from every schema fragment reachable from this class.
type ResolvedResourceStanding struct {
	// ResourceType is the SpiceDB resource type this answer is for.
	ResourceType string `json:"resourceType"`

	// Standing is `required` or `session-only`. Never empty: a type whose
	// fragments declare nothing is refused at resolve time rather than published
	// with a blank answer a reader would have to interpret.
	Standing string `json:"standing"`

	// ApproverPermission is the permission an approver must hold on an instance,
	// set only when Standing is `required`.
	// +optional
	ApproverPermission string `json:"approverPermission,omitempty"`
}

// EffectivePermissions is every permission a grant on this slot may confer:
// Permission plus Permissions, deduplicated, in declaration order.
//
// Read this rather than either field, so "the slot's permissions" cannot mean
// one thing at the schema composer and another at the binding site — a
// mismatch there writes a grant against a relation the schema never emitted,
// which fails at SpiceDB with a FailedPrecondition nobody sees.
func (s AuthzSlot) EffectivePermissions() []string {
	out := make([]string, 0, 1+len(s.Permissions))
	seen := map[string]bool{}
	for _, p := range append([]string{s.Permission}, s.Permissions...) {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// SlotPermissionSets maps each slot's resource type to every permission a grant
// on it may confer — the CEILING an approval may bind within.
//
// The plural companion to SlotPermissions. Binding is filtered through this:
// an approval naming a permission the slot never declared has no
// slot_grant_<permission> relation in the composed schema, so writing it fails
// the WHOLE grant write at SpiceDB and the approval buys nothing at all — not
// even the permissions that were declared.
func (s *AgentClassSpec) SlotPermissionSets() map[string][]string {
	slots := s.GetSlots()
	if len(slots) == 0 {
		return nil
	}
	out := make(map[string][]string, len(slots))
	for _, sl := range slots {
		if sl.ResourceType == "" {
			continue
		}
		out[sl.ResourceType] = append(out[sl.ResourceType], sl.EffectivePermissions()...)
	}
	return out
}

// GetCrossAgentThreads returns Authz.CrossAgentThreads or a zero-value
// pointer; never returns nil.
//
// The zero value is the CLOSED one — Enabled false, WakeBudget zero — so a
// class that declares nothing admits no other agent and grants no wake. Every
// caller therefore gets the safe answer by default rather than by remembering
// to nil-check, which is the point of the getter.
func (b *AuthzBlock) GetCrossAgentThreads() *CrossAgentThreadsConfig {
	if b == nil || b.CrossAgentThreads == nil {
		return &CrossAgentThreadsConfig{}
	}
	return b.CrossAgentThreads
}

// GetTrifecta returns Authz.Trifecta or a zero-value pointer; never returns nil.
//
// The zero value is the OFF one — Mode empty, which every consumer treats as
// disabled — so a class that declares nothing runs no trifecta check. Callers
// get that answer by construction rather than by remembering to nil-check.
func (b *AuthzBlock) GetTrifecta() *TrifectaConfig {
	if b == nil || b.Trifecta == nil {
		return &TrifectaConfig{}
	}
	return b.Trifecta
}

// ReferencesSidecarToolbox reports whether this class declares a
// SidecarToolbox ref equal to name.
//
// It lives here, on the type, because two independently-reachable gates ask
// the same question about the agent-builder workshop and must answer it
// identically: the operator's ensureWorkshop (a cluster sanction buys nothing
// unless the class actually wires that sidecar in) and the browser start
// route's per-starter workshop cap (a class that creates no workshop must
// never be counted against it). A second copy of the loop is how the two come
// to disagree about which classes are workshop classes — the route refusing a
// start the operator would have allowed, or admitting one it will boot-fail.
//
// A nil receiver reports false: no class declares nothing.
func (ac *AgentClass) ReferencesSidecarToolbox(name string) bool {
	if ac == nil {
		return false
	}
	for _, ref := range ac.Spec.SidecarToolboxes {
		if ref.Ref == name {
			return true
		}
	}
	return false
}
