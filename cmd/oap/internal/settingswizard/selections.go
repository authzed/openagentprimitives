package settingswizard

import v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

// Selections is the wizard's normalized output — what the user chose. The
// composer turns it into a ClusterAgentSettings. The huh TUI and --defaults
// both produce a Selections.
type Selections struct {
	Breaker            bool // baseline circuit breaker
	RateLimit          bool // baseline rate limit
	DataLimit          bool // baseline data-volume budget
	Pinning            bool // baseline dependency pinning (warn)
	PromptInjection    *PromptInjectionSel
	URLAllowlist       *URLAllowlistSel
	ModelCatalog       []ModelEntrySel // cluster model catalog entries the wizard collected
	AllowModelOverride bool            // permit AgentClasses to bring their own model+token
	// PinningRules carries the pinning ceiling already stored on the CR so a
	// re-run cannot weaken it. The wizard's Pinning checkbox is one boolean,
	// but PinningPolicy.Rules is +listType=map +listMapKey=kind and the wizard
	// applies with Force — so any rule it re-emits takes ownership of that
	// kind's `mode`. Without the stored rules here, a re-run silently rewrites
	// an operator's `--pinning-mode=block` ceiling as the baseline warn.
	// Empty means "no stored ceiling": compose the baseline.
	PinningRules []PinningRuleSel
	// PreservedModelCatalog carries the catalog entries the wizard does not
	// edit. The wizard only ever collects the single default entry, while
	// ModelCatalog on the CR has no +listType marker and is therefore ATOMIC —
	// a forced apply replaces the whole list. Carrying the rest through is what
	// keeps a re-run from deleting every non-default model in the catalog.
	PreservedModelCatalog []ModelEntrySel
	// NativeFileHandling is a passive round-trip of the Tier-2 provider-native
	// file-handling grant (off by default; set out-of-band via the CR — the
	// wizard has no interactive toggle for it, it only preserves an existing
	// grant on re-run). Enabling it routes large artifact bytes through the
	// provider's code-execution sandbox; content-guard still inspects them.
	NativeFileHandling bool
	// PreservedToolGuard carries a stored spec.defaults.toolGuard policy through
	// VERBATIM, so a --defaults re-run re-asserts the baseline controls without
	// discarding an operator's customized thresholds. It is the toolGuard analog
	// of PinningRules: the Breaker/RateLimit/DataLimit booleans only say WHICH
	// controls are on, and Compose rebuilds them with fixed baseline thresholds,
	// so a re-run would otherwise reset a custom failure threshold or byte
	// budget. When set, Compose emits it as-is and ignores the three booleans.
	//
	// The defaults-merge path sets it (see settingscmd.mergeDefaultsWithExisting);
	// the INTERACTIVE path leaves it nil, because there the checkboxes are the
	// user's live edit and must drive the rebuild. Held as the API type because
	// it is an opaque pass-through, not a wizard-authored value.
	PreservedToolGuard *v1alpha1.ToolGuardPolicy
}

// PinningRuleSel is one already-stored pinning rule, carried verbatim through
// a wizard re-run. Mirrors v1alpha1.PinningRule; kept as its own type so
// Selections stays the wizard's own vocabulary rather than the API's.
type PinningRuleSel struct {
	Kind        string
	MinStrength string
	Mode        string
}

// PromptInjectionSel configures the optional prompt-injection content
// inspector. DetectorImage is the OCI image for the zero-egress ONNX
// classifier sidecar; Threshold is the classifier confidence cut-off (0–1);
// Action is "approve" or "block".
type PromptInjectionSel struct {
	DetectorImage string
	Threshold     float64
	Action        string // approve | block
}

// URLAllowlistSel configures the optional URL-allowlist content inspector.
type URLAllowlistSel struct {
	Rules         []URLRuleSel
	DefaultAction string // deny | approve
}

// URLRuleSel is one entry in a URLAllowlistSel.
type URLRuleSel struct {
	Domain string
	Action string // allow | deny | approve
}

// ModelEntrySel is one catalog entry the wizard collects. TokenSecretNamespace
// defaults to the install namespace; Default marks the inherited default.
// InputPerMTok / OutputPerMTok are optional best-effort USD list prices per
// million tokens, surfaced by the admin dashboard's cost estimator; zero means
// no price (the dashboard shows "est. (no price)").
type ModelEntrySel struct {
	Name                 string
	Provider             string
	TokenSecretName      string
	TokenSecretNamespace string
	TokenSecretKey       string
	Default              bool
	InputPerMTok         float64
	OutputPerMTok        float64
}

// BaselineSelections is the recommended secure baseline installed by --defaults
// and pre-checked in the wizard checklist. Interactive opt-ins (PromptInjection,
// URLAllowlist) are nil.
func BaselineSelections() Selections {
	return Selections{Breaker: true, RateLimit: true, DataLimit: true, Pinning: true}
}
