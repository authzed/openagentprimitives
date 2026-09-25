package settingswizard

import (
	"encoding/json"
	"sort"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/promptinjection"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/urlallowlist"
	"github.com/authzed/openagentprimitives/pkg/authz/pinning/registry"
)

// baselineMinStrength is the ref strength the baseline ceiling requires of
// every kind: at minimum a named ref.
const baselineMinStrength = "named"

// Compose turns Selections into the singleton "cluster" ClusterAgentSettings,
// writing each knob to its correct tier:
//   - tool-guard rules   → spec.defaults.toolGuard  (inherited default policy)
//   - pinning ceiling    → spec.limits.pinning       (ceiling, narrows downward)
//   - content inspectors → spec.limits.contentInspectors (ceiling, additive)
//
// The result is the complete snapshot of the user's wizard selections; on apply
// (see Apply) SSA asserts ownership of the fields it sets. Fields owned by other
// managers that the wizard omits are not removed — see Apply's note.
func Compose(s Selections) *v1alpha1.ClusterAgentSettings {
	cas := &v1alpha1.ClusterAgentSettings{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.SchemeGroupVersion.String(),
			Kind:       "ClusterAgentSettings",
		},
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ClusterAgentSettingsName},
	}

	// --- spec.defaults.toolGuard ---
	// A stored policy carried verbatim wins over the boolean-driven rebuild, so a
	// --defaults re-run preserves an operator's customized thresholds instead of
	// resetting them to the baseline (see Selections.PreservedToolGuard). The
	// interactive path never sets it, so it falls through to the checkbox rebuild.
	if s.PreservedToolGuard != nil {
		cas.Spec.Defaults = &v1alpha1.SettingsDefaults{ToolGuard: s.PreservedToolGuard}
	} else if s.Breaker || s.RateLimit || s.DataLimit {
		rule := v1alpha1.ToolGuardRule{
			Match: v1alpha1.ToolGuardMatch{Tool: "*"},
		}
		if s.Breaker {
			rule.Breaker = &v1alpha1.BreakerSpec{
				FailureThreshold: 5,
				Action:           v1alpha1.ToolGuardActionDeny,
			}
		}
		if s.RateLimit {
			rule.RateLimit = &v1alpha1.RateLimitSpec{
				MaxCallsPerTurn: 30,
				Action:          v1alpha1.ToolGuardActionDeny,
			}
		}
		if s.DataLimit {
			rule.DataLimit = &v1alpha1.DataLimitSpec{
				MaxEgressBytes:  262144,  // 256 KiB
				MaxIngressBytes: 1048576, // 1 MiB
				Action:          v1alpha1.ToolGuardActionDeny,
			}
		}
		cas.Spec.Defaults = &v1alpha1.SettingsDefaults{
			ToolGuard: &v1alpha1.ToolGuardPolicy{
				Rules: []v1alpha1.ToolGuardRule{rule},
			},
		}
	}

	// --- spec.limits: pinning ceiling + content-inspector ceiling ---
	limits := &v1alpha1.SettingsLimits{}
	hasLimits := false

	if s.Pinning {
		limits.Pinning = buildPinning(s.PinningRules)
		hasLimits = true
	}

	var cis []v1alpha1.ContentInspectorConfig

	if s.PromptInjection != nil {
		// Marshal the canonical inspector Config so a future key rename in the
		// promptinjection package fails this build instead of silently drifting.
		// Threshold is *float64; take the address of a local. omitempty on the
		// optional fields keeps the emitted JSON the same curated subset.
		threshold := s.PromptInjection.Threshold
		cfg, _ := json.Marshal(promptinjection.Config{
			DetectorImage: s.PromptInjection.DetectorImage,
			Port:          8919,
			Threshold:     &threshold,
			Action:        s.PromptInjection.Action,
			Points:        []string{"PostToolCall"},
			OnError:       "warn",
		})
		cis = append(cis, v1alpha1.ContentInspectorConfig{
			ID:     "prompt-injection",
			Config: apiextv1.JSON{Raw: cfg},
		})
	}

	if s.URLAllowlist != nil && len(s.URLAllowlist.Rules) > 0 {
		// Marshal the canonical urlallowlist.Config / URLRule structs for the
		// same compile-time-key-safety reason as the prompt-injection block.
		rules := make([]urlallowlist.URLRule, 0, len(s.URLAllowlist.Rules))
		for _, r := range s.URLAllowlist.Rules {
			rules = append(rules, urlallowlist.URLRule{Domain: r.Domain, Action: r.Action})
		}
		cfg, _ := json.Marshal(urlallowlist.Config{
			Rules:         rules,
			DefaultAction: s.URLAllowlist.DefaultAction,
		})
		cis = append(cis, v1alpha1.ContentInspectorConfig{
			ID:     "url-allowlist",
			Config: apiextv1.JSON{Raw: cfg},
		})
	}

	if len(cis) > 0 {
		limits.ContentInspectors = &cis
		hasLimits = true
	}

	// AllowModelOverride is always emitted when a catalog is configured — the
	// two settings are coupled: the catalog is meaningful only when the override
	// ceiling is explicit. When there is no catalog, only emit the flag if it
	// is set to true (an opt-in to override without a managed catalog).
	if s.AllowModelOverride || len(s.ModelCatalog)+len(s.PreservedModelCatalog) > 0 {
		v := s.AllowModelOverride
		limits.AllowModelOverride = &v
		hasLimits = true
	}

	// NativeFileHandling is a passive round-trip of the Tier-2 grant: emit it
	// only when the wizard's Selections carries it (set out-of-band on the CR
	// and preserved across a re-run via prepopulateSelections). It is off by
	// default and has no interactive toggle, so absent means "leave unset".
	if s.NativeFileHandling {
		v := true
		limits.NativeFileHandling = &v
		hasLimits = true
	}

	if hasLimits {
		cas.Spec.Limits = limits
	}

	// The wizard's own entry first, then the entries it never showed the user.
	// ModelCatalog carries no +listType marker, so it is atomic and a forced
	// apply replaces the whole list — emitting only the wizard's entry would
	// delete every other model from the cluster catalog. Order is not
	// load-bearing (entries are resolved by name / the Default flag).
	if len(s.ModelCatalog)+len(s.PreservedModelCatalog) > 0 {
		cat := make([]v1alpha1.ModelCatalogEntry, 0, len(s.ModelCatalog)+len(s.PreservedModelCatalog))
		for _, e := range append(append([]ModelEntrySel{}, s.ModelCatalog...), s.PreservedModelCatalog...) {
			cat = append(cat, v1alpha1.ModelCatalogEntry{
				Name:     e.Name,
				Provider: e.Provider,
				Default:  e.Default,
				TokenRef: &v1alpha1.NamespacedSecretKeyRef{
					Namespace: e.TokenSecretNamespace,
					Name:      e.TokenSecretName,
					Key:       e.TokenSecretKey,
				},
				InputPerMTok:  e.InputPerMTok,
				OutputPerMTok: e.OutputPerMTok,
			})
		}
		cas.Spec.ModelCatalog = &cat
	}

	return cas
}

// buildPinning constructs the PinningPolicy ceiling: one PinningRule per
// concrete pinning kind. The listMapKey=kind constraint requires one concrete
// entry per kind — a wildcard "*" kind is not valid here.
//
// It is a UNION of what is already stored and the baseline, never a
// replacement of one by the other:
//
//   - A kind with a stored rule keeps that rule byte-for-byte. The wizard has
//     no screen for minStrength or mode, so it has nothing better to say than
//     what the operator already chose — and because Rules is
//     +listType=map/+listMapKey=kind under a forced apply, re-emitting a
//     baseline here would take ownership of `mode` and silently downgrade an
//     `oap install --pinning-mode=block` ceiling to warn.
//   - A registered kind with no stored rule gets the baseline (named + warn),
//     so opting into pinning covers every kind the cluster can enforce and a
//     newly registered kind is not left uncovered.
//   - A stored rule for a kind this binary does not register is preserved
//     rather than dropped: dropping it would delete a ceiling that an operator
//     registering that kind does enforce.
//
// The registered kind list comes from the pinning registry — the same source
// `oap install --pinning-mode` (ensureClusterPinningMode) and the settings
// webhook's kind validation already use — rather than a transcribed literal,
// which had drifted: it named four of the five registered kinds, so `oap` was
// silently excluded from the baseline.
func buildPinning(stored []PinningRuleSel) *v1alpha1.PinningPolicy {
	byKind := make(map[string]PinningRuleSel, len(stored))
	for _, r := range stored {
		byKind[r.Kind] = r
	}

	kinds := registry.All()
	rules := make([]v1alpha1.PinningRule, 0, len(kinds)+len(stored))
	for _, k := range kinds {
		if r, ok := byKind[k.Name()]; ok {
			rules = append(rules, v1alpha1.PinningRule{Kind: r.Kind, MinStrength: r.MinStrength, Mode: r.Mode})
			delete(byKind, k.Name())
			continue
		}
		rules = append(rules, v1alpha1.PinningRule{
			Kind:        k.Name(),
			MinStrength: baselineMinStrength,
			Mode:        v1alpha1.PinModeWarn,
		})
	}

	unregistered := make([]string, 0, len(byKind))
	for kind := range byKind {
		unregistered = append(unregistered, kind)
	}
	sort.Strings(unregistered) // registry.All() is sorted; keep the whole list deterministic
	for _, kind := range unregistered {
		r := byKind[kind]
		rules = append(rules, v1alpha1.PinningRule{Kind: r.Kind, MinStrength: r.MinStrength, Mode: r.Mode})
	}

	return &v1alpha1.PinningPolicy{Rules: rules}
}
