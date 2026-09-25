package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,spicebox},shortName=aui
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Valid",type="string",JSONPath=".status.conditions[?(@.type=='Valid')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient
//
// AgentUI is a bundle-authored DECLARATION of an agent-defined view: a page
// tree in which generative hooks are the agent's regions, plus the tools and
// actions the view asks the browser be allowed to fire. It is a declaration,
// not a web app.
//
// Namespaced. Reconciled by pkg/controllers/agentui, which validates the whole
// page against the platform component vocabulary and observes both its hook
// table and an eligibility ceiling onto status. Naming a tool here authorizes
// nothing: the deployment half of the grant lives on AgentClass.spec.agentUI, a
// separate object with a separate writer, and the runner still re-authorizes
// every click.
type AgentUI struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentUISpec   `json:"spec,omitempty"`
	Status AgentUIStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentUIList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentUI `json:"items"`
}

// AgentUISpec is entirely BUNDLE-AUTHORED. Nothing here authorizes anything:
// Tools is a request, and the deployment's grant lives on a different object
// (AgentClass.spec.agentUI.grantedTools) so the two never share a field manager.
type AgentUISpec struct {
	// DisplayName is the human label for this view; empty falls back to the
	// CR name.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Chrome carries presentation requests for the surrounding session shell.
	// +optional
	Chrome *AgentUIChrome `json:"chrome,omitempty"`

	// View is the page: one component tree in which every oap:generative
	// node is a region the agent may fill (its name, intent, and the
	// components the AGENT may put there). Held as raw JSON for the same
	// reason AgentUISlot.Default is — CRD types must not depend on
	// pkg/web/uicomponents; the controller validates it at reconcile time.
	//
	// Exactly one of View and Slots is set; the controller (not CEL) enforces
	// it, for the same "better message" reason AgentUIAction's tool/prompt
	// exclusivity is checked there.
	// +optional
	View *apiextensionsv1.JSON `json:"view,omitempty"`

	// Slots is the page's LEGACY flat shape — named regions in a row. Still
	// accepted: the controller compiles each agentWritable slot into a hook
	// (allowedComponents ["*"]) inside a root ap:stack and validates the
	// result exactly as it would a View. New pages declare View.
	// +optional
	Slots []AgentUISlot `json:"slots,omitempty"`

	// Tools is what this UI REQUESTS the browser be able to call directly —
	// one of three conditions (see pkg/web/uigrant). A tool named here is
	// callable only if its origin also permits app-visible calls AND the
	// deployment granted it.
	//
	// The item pattern is exactly the output alphabet of
	// synthesize.NormalizeName, the transform every synthesized tool name
	// passes through to become a Loop.AppTools key, and is the same pattern
	// AgentClass.spec.agentUI.grantedTools carries. Pinning both vocabularies
	// to it makes them identical by construction: a camelCase
	// "widgets_createIssue" is rejected loudly at write time instead of
	// validating clean, matching clean against an equally-unnormalized
	// grantedTools, and dying as a dead button with no denial log anywhere.
	//
	// A CRD pattern is not retroactive: objects persisted before it can still
	// carry non-conforming names until next written, so the runner's normalize
	// step MUST NOT be deleted as redundant.
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9_-]{1,128}$`
	// +optional
	Tools []string `json:"tools,omitempty"`

	// Actions is the bundle-authored action table this UI's controls may fire
	// — see AgentUIAction's doc comment for why the tool and args template
	// live here, server-side, rather than in a control's own props. Like
	// Tools, naming a tool here is a REQUEST: it authorizes nothing by
	// itself, and both the deployment grant and the runner's per-click,
	// viewer-bound re-authorization still run.
	// +optional
	Actions []AgentUIAction `json:"actions,omitempty"`
}

// AgentUIAction is one bundle-authored action: a tool this UI's controls may
// fire, plus the args template to fire it with. It authorizes NOTHING — the
// deployment grant (AgentClass.spec.agentUI.grantedTools) and the per-click,
// viewer-bound re-authorization in the runner are the authorization, and both
// still run. This field exists so the tool and the args template live
// SERVER-side, where a browser can name an action without being able to
// choose what it does.
type AgentUIAction struct {
	// Name is how a control refers to this action; unique within the UI.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_.-]{1,64}$`
	Name string `json:"name"`

	// Tool carries the same normalize alphabet as AgentUISpec.Tools, for the
	// same reason. Optional because an action may declare a Prompt instead:
	// exactly one of Tool and Prompt is set, enforced by pkg/web/uicomponents'
	// validateActions rather than the apiserver, since "exactly one of these
	// two" in CEL buys a worse message than the validator's own.
	// +kubebuilder:validation:Pattern=`^[a-z0-9_-]{1,128}$`
	// +optional
	Tool string `json:"tool,omitempty"`

	// Prompt makes this action ASK THE AGENT instead of calling a tool: the
	// control sends this text to the session as a message from the viewer. It
	// covers "summarize this" or "tell me about this row", which are not tool
	// calls, and grants nothing new — the message is one the viewer could have
	// typed themselves, on the same route under the same authorization and
	// attributed to them. {key} placeholders name a binding parameter or one of
	// this action's Inputs, so a row's values can reach the sentence.
	//
	// The length cap is generous but real: a prompt long enough to be a
	// document belongs in spec.systemPrompt, where standing instructions live.
	// +kubebuilder:validation:MaxLength=2000
	// +optional
	Prompt string `json:"prompt,omitempty"`

	// Args is the args template, held as raw JSON for the same reason
	// AgentUISlot.Default is: CRD types must not depend on pkg/web/uicomponents.
	// +optional
	Args *apiextensionsv1.JSON `json:"args,omitempty"`

	// Inputs names the values a CONTROL may supply at invoke time — a
	// server-side allowlist enforced by pkg/web/uicomponents' validateActions
	// (unique names, disjoint from every binding parameter name).
	// +kubebuilder:validation:items:Pattern=`^[a-zA-Z0-9_.-]{1,64}$`
	// +optional
	Inputs []string `json:"inputs,omitempty"`
}

// AgentUIChrome carries presentation REQUESTS only. Chrome authority is not
// negotiable: the user's explicit toggle always wins, and a trust event
// (an approval addressed to the viewer, a credential request, a hard error)
// auto-reveals chrome regardless of what is asked for here.
type AgentUIChrome struct {
	// InitialState is how the session shell should first appear; the user's
	// own toggle overrides it thereafter.
	// +kubebuilder:validation:Enum=expanded;collapsed
	// +optional
	InitialState string `json:"initialState,omitempty"`
}

type AgentUISlot struct {
	// Name is the slot identifier a declaration node targets; unique per UI.
	Name string `json:"name"`

	// AgentWritable permits the agent to overwrite this slot at runtime.
	// Default false: a slot is author-owned unless it opts in.
	// +optional
	AgentWritable bool `json:"agentWritable,omitempty"`

	// Default is the Tier-0 declaration node rendered with NO agent turn
	// involved — which is what lets a brand-new session paint a complete UI on
	// first load. Held as raw JSON because CRD types must not depend on
	// pkg/web/uicomponents; the controller validates it against the vocabulary at
	// reconcile time.
	// +optional
	Default *apiextensionsv1.JSON `json:"default,omitempty"`
}

type AgentUIStatus struct {
	// EligibleTools is a CEILING, not a grant: spec.tools intersected with the
	// UNION of GrantedTools across every AgentClass naming this AgentUI. It
	// stops there and does NOT filter by per-origin app-visibility, because
	// which origins resolve at all is SESSION-scoped — a property of the
	// AgentSession, unknowable to this namespaced CR.
	//
	// The union is SOUND IN THE REJECT DIRECTION for this field's one purpose,
	// gating which tool names a Tier-0 slot default may bind to: a tool outside
	// it is one NO referencing class could ever grant, so rejecting the binding
	// is never a false negative. Deliberately not called "fail-closed" — that
	// describes the ACCEPT direction, where the union is the LOOSEST choice
	// among referencing classes, and carrying that framing elsewhere would be
	// false. One AgentUI may be deployed by several classes with different
	// grants, so there is no single "the grant" to observe here in principle.
	//
	// OPERATOR-FACING OBSERVATION ONLY — an admin UI showing "tools a bundle
	// could ever bind to". It MUST NOT be read as, or fed into, an
	// authorization decision: the narrower per-session EFFECTIVE set the runner
	// computes against live-resolved origins is what gates a real tool call.
	// Status-subresource only; a client applying spec must never send it.
	// +optional
	EligibleTools []string `json:"eligibleTools,omitempty"`

	// Hooks lists the page's generative regions after the spec.slots shim, so
	// an operator can see what the agent may write without compiling the page
	// themselves. A pure function of spec: written only when the page was
	// validated, and cleared whenever it is not; preserved across a
	// transient grant-resolve failure (Valid=Unknown), the same as
	// EligibleTools, since that failure means this reconcile never
	// re-validated the page at all.
	// +optional
	Hooks []AgentUIHook `json:"hooks,omitempty"`

	// Conditions carries Valid and ToolsGranted; see their type constants.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// AgentUIHook is one region the page exposes to the agent, as OBSERVED from
// spec by the controller — status-subresource only, never applied by a
// client.
type AgentUIHook struct {
	// Name is the hook's identifier, unique within the page.
	Name string `json:"name"`

	// Intent is the author's guidance for what the agent should put here.
	// +optional
	Intent string `json:"intent,omitempty"`

	// AllowedComponents is the component vocabulary this hook accepts; "*"
	// permits any registered component.
	// +optional
	AllowedComponents []string `json:"allowedComponents,omitempty"`
}

// AgentUIConditionValid is the Valid condition type on AgentUIStatus. True
// means the page — spec.view, or spec.slots compiled — parses and validates
// against the platform vocabulary, including every hook's contract; on a
// resolve failure for the AgentClass grant it is set Unknown (see
// ReasonAgentUIGrantUnresolved), never derived from a ceiling this reconcile
// failed to read.
const AgentUIConditionValid = "Valid"

// AgentUIConditionToolsGranted is a SEPARATE condition from Valid: whether
// EligibleTools fully covers the UI's own (deduplicated) spec.tools request.
// True means every requested tool made the ceiling; False means at least one
// did not, with the message carrying uigrant.ExplainCeiling's diagnosis so
// an operator can see WHICH tool and WHY without source-diving. A UI can be
// Valid=True (every slot default well-formed) and ToolsGranted=False at the
// same time — they are independent facts, not a shared verdict.
const AgentUIConditionToolsGranted = "ToolsGranted"

func init() {
	SchemeBuilder.Register(&AgentUI{}, &AgentUIList{})
}
