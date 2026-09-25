package settingswizard

import (
	"testing"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	// buildPinning derives the baseline's kind list from the pinning registry,
	// so this test binary has to register the kinds the way cmd/oap's main.go
	// does — same shape as install_pinning_test.go.
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/cli"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/image"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/mcp"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/oap"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/skill"
	"github.com/authzed/openagentprimitives/pkg/authz/pinning/registry"
)

func TestCompose_baseline(t *testing.T) {
	cas := Compose(BaselineSelections())
	require.Equal(t, v1alpha1.ClusterAgentSettingsName, cas.Name)
	require.NotNil(t, cas.Spec.Defaults)
	require.NotNil(t, cas.Spec.Defaults.ToolGuard)
	require.Len(t, cas.Spec.Defaults.ToolGuard.Rules, 1)
	r := cas.Spec.Defaults.ToolGuard.Rules[0]
	assert.Equal(t, "*", r.Match.Tool)
	require.NotNil(t, r.Breaker)
	assert.Equal(t, int32(5), r.Breaker.FailureThreshold)
	assert.Equal(t, v1alpha1.ToolGuardActionDeny, r.Breaker.Action)
	require.NotNil(t, r.RateLimit)
	assert.Equal(t, int32(30), r.RateLimit.MaxCallsPerTurn)
	require.NotNil(t, r.DataLimit)
	assert.Equal(t, int64(262144), r.DataLimit.MaxEgressBytes)
	assert.Equal(t, int64(1048576), r.DataLimit.MaxIngressBytes)
	// pinning ceiling, warn
	require.NotNil(t, cas.Spec.Limits)
	require.NotNil(t, cas.Spec.Limits.Pinning)
	require.NotEmpty(t, cas.Spec.Limits.Pinning.Rules)
	assert.Equal(t, v1alpha1.PinModeWarn, cas.Spec.Limits.Pinning.Rules[0].Mode)
	// baseline carries NO content inspectors
	assert.Nil(t, cas.Spec.Limits.ContentInspectors)
}

func TestCompose_promptInjection(t *testing.T) {
	s := BaselineSelections()
	s.PromptInjection = &PromptInjectionSel{DetectorImage: "pi-detector:dev", Threshold: 0.8, Action: "approve"}
	cas := Compose(s)
	require.NotNil(t, cas.Spec.Limits.ContentInspectors)
	cis := *cas.Spec.Limits.ContentInspectors
	require.Len(t, cis, 1)
	assert.Equal(t, "prompt-injection", cis[0].ID)
	assert.Contains(t, string(cis[0].Config.Raw), `"detectorImage":"pi-detector:dev"`)
	assert.Contains(t, string(cis[0].Config.Raw), `"threshold":0.8`)
}

func TestCompose_urlAllowlistEmptyRulesProducesNoInspector(t *testing.T) {
	// URLAllowlist != nil but zero rules — must NOT produce an inspector entry
	// (an empty rules list fails admission validation).
	s := BaselineSelections()
	s.URLAllowlist = &URLAllowlistSel{Rules: nil, DefaultAction: "deny"}
	cas := Compose(s)
	// Limits should still exist (pinning), but contentInspectors must be absent.
	require.NotNil(t, cas.Spec.Limits, "pinning ceiling should still be set")
	assert.Nil(t, cas.Spec.Limits.ContentInspectors, "empty-rules URLAllowlist must not emit a content-inspector entry")
}

func TestCompose_ModelCatalog(t *testing.T) {
	s := Selections{
		ModelCatalog: []ModelEntrySel{{
			Name: "claude-opus-4-8", Provider: "anthropic",
			TokenSecretName: "anthropic-key", TokenSecretNamespace: "agentprimitives-system", TokenSecretKey: "token",
			Default:       true,
			InputPerMTok:  15,
			OutputPerMTok: 75,
		}},
		AllowModelOverride: false,
	}
	cas := Compose(s)

	require.NotNil(t, cas.Spec.ModelCatalog)
	got := (*cas.Spec.ModelCatalog)
	require.Len(t, got, 1)
	assert.Equal(t, "claude-opus-4-8", got[0].Name)
	assert.True(t, got[0].Default)
	assert.Equal(t, "anthropic", got[0].Provider)
	require.NotNil(t, got[0].TokenRef)
	assert.Equal(t, "agentprimitives-system", got[0].TokenRef.Namespace)
	assert.Equal(t, "anthropic-key", got[0].TokenRef.Name)
	assert.Equal(t, "token", got[0].TokenRef.Key)
	assert.Equal(t, 15.0, got[0].InputPerMTok, "input price round-trips")
	assert.Equal(t, 75.0, got[0].OutputPerMTok, "output price round-trips")
	require.NotNil(t, cas.Spec.Limits)
	require.NotNil(t, cas.Spec.Limits.AllowModelOverride)
	assert.False(t, *cas.Spec.Limits.AllowModelOverride)
}

// TestCompose_NativeFileHandling covers the passive Tier-2 grant round-trip:
// true emits limits.NativeFileHandling=&true; false leaves it unset (the
// wizard has no interactive toggle, so absent means "leave the grant alone").
func TestCompose_NativeFileHandling(t *testing.T) {
	t.Run("true grants: Limits.NativeFileHandling==&true", func(t *testing.T) {
		cas := Compose(Selections{NativeFileHandling: true})
		require.NotNil(t, cas.Spec.Limits)
		require.NotNil(t, cas.Spec.Limits.NativeFileHandling)
		assert.True(t, *cas.Spec.Limits.NativeFileHandling)
	})
	t.Run("false leaves NativeFileHandling unset", func(t *testing.T) {
		// A bare (no other limits) false selection sets no limits field at all,
		// so Compose must not materialize Spec.Limits (hasLimits stays false).
		cas := Compose(Selections{NativeFileHandling: false})
		require.Nil(t, cas.Spec.Limits, "a bare false selection must not materialize spec.limits")
	})
}

func TestCompose_urlAllowlistWithRulesProducesInspector(t *testing.T) {
	s := BaselineSelections()
	s.URLAllowlist = &URLAllowlistSel{
		Rules:         []URLRuleSel{{Domain: "github.com", Action: "allow"}},
		DefaultAction: "deny",
	}
	cas := Compose(s)
	require.NotNil(t, cas.Spec.Limits.ContentInspectors)
	cis := *cas.Spec.Limits.ContentInspectors
	require.Len(t, cis, 1)
	assert.Equal(t, "url-allowlist", cis[0].ID)
	assert.Contains(t, string(cis[0].Config.Raw), `"github.com"`)
	assert.Contains(t, string(cis[0].Config.Raw), `"deny"`)
}

// TestBuildPinning_UnionOfStoredAndBaseline pins buildPinning's three rules:
// a stored rule survives verbatim (the wizard has no minStrength/mode screen,
// and Rules is +listType=map under a forced apply, so re-emitting a baseline
// would take ownership of `mode` and downgrade an operator's ceiling); a
// registered kind with no stored rule gets the baseline; and a stored rule for
// an unregistered kind is preserved rather than deleted.
func TestBuildPinning_UnionOfStoredAndBaseline(t *testing.T) {
	registered := registry.All()
	require.NotEmpty(t, registered, "the blank imports above must populate the registry")
	first := registered[0].Name()

	pol := buildPinning([]PinningRuleSel{
		{Kind: first, MinStrength: "frozen", Mode: v1alpha1.PinModeBlock},
		{Kind: "kind-this-binary-does-not-register", MinStrength: "named", Mode: v1alpha1.PinModeApprove},
	})

	byKind := map[string]v1alpha1.PinningRule{}
	for _, r := range pol.Rules {
		byKind[r.Kind] = r
	}

	assert.Equal(t, v1alpha1.PinModeBlock, byKind[first].Mode, "a stored mode is never downgraded")
	assert.Equal(t, "frozen", byKind[first].MinStrength, "a stored minStrength is never relaxed")
	for _, k := range registered[1:] {
		assert.Equal(t, v1alpha1.PinModeWarn, byKind[k.Name()].Mode, "unstored registered kind %q gets the baseline", k.Name())
		assert.Equal(t, baselineMinStrength, byKind[k.Name()].MinStrength)
	}
	assert.Equal(t, v1alpha1.PinModeApprove, byKind["kind-this-binary-does-not-register"].Mode,
		"a rule for a kind this binary does not register is preserved, not dropped")
	assert.Len(t, pol.Rules, len(registered)+1, "one rule per kind — listMapKey=kind admits no duplicates")
}

// TestCompose_PreservedModelCatalogIsEmitted pins the atomic-list half: the
// wizard collects one entry, but everything it was handed has to come back out
// or a forced apply deletes it.
func TestCompose_PreservedModelCatalogIsEmitted(t *testing.T) {
	cas := Compose(Selections{
		ModelCatalog:          []ModelEntrySel{{Name: "wizard-model", Provider: "anthropic", Default: true}},
		PreservedModelCatalog: []ModelEntrySel{{Name: "other-a"}, {Name: "other-b"}},
	})

	require.NotNil(t, cas.Spec.ModelCatalog)
	names := make([]string, 0, 3)
	for _, e := range *cas.Spec.ModelCatalog {
		names = append(names, e.Name)
	}
	assert.Equal(t, []string{"wizard-model", "other-a", "other-b"}, names)
}

// TestCompose_PreservedCatalogAloneStillEmitsTheList covers the run where the
// operator declines to manage a default model: the entries they never saw must
// still survive rather than being wiped by the forced apply.
func TestCompose_PreservedCatalogAloneStillEmitsTheList(t *testing.T) {
	cas := Compose(Selections{PreservedModelCatalog: []ModelEntrySel{{Name: "other-a"}}})

	require.NotNil(t, cas.Spec.ModelCatalog)
	require.Len(t, *cas.Spec.ModelCatalog, 1)
	assert.Equal(t, "other-a", (*cas.Spec.ModelCatalog)[0].Name)
	require.NotNil(t, cas.Spec.Limits)
	require.NotNil(t, cas.Spec.Limits.AllowModelOverride, "a catalog always pins the override ceiling explicitly")
}
