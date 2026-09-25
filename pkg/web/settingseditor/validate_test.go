package settingseditor

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	settings "github.com/authzed/openagentprimitives/pkg/platform/settings"

	// Pinning kinds, as blank-imported by cmd/oap/main.go:11-15 — required so
	// the pinning registry is populated before PinningKindsError/the resolver
	// can validate any pinning rule or dependency pin.
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/cli"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/image"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/mcp"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/oap"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/skill"

	// Contentguard inspector kind, as blank-imported by
	// pkg/controllers/webhooks/settings/contentinspectors_test.go — required
	// so the contentguard registry is populated before ContentInspectorsError
	// can validate any limits.contentInspectors entry.
	_ "github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/urlallowlist"
)

func TestValidate_CleanClusterSpec_NoErrors_EffectivePreview(t *testing.T) {
	spec := &v1alpha1.SettingsSpec{
		ModelCatalog: &[]v1alpha1.ModelCatalogEntry{{
			Name: "claude-sonnet-5", Provider: "anthropic", Default: true,
			TokenRef: &v1alpha1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "model-default-token", Key: "token"},
		}},
		Limits: &v1alpha1.SettingsLimits{Budget: &v1alpha1.SettingsBudgetCeiling{MaxTurns: 50}},
	}
	res := Validate(spec, true)
	assert.Empty(t, res.Errors)
	require.NotNil(t, res.Effective, "a valid spec must produce an effective-settings preview")
}

func TestValidate_CleanSpecMarshalsEmptyArraysNeverNull(t *testing.T) {
	// The wire contract (the settings UI's ValidationResult) promises arrays
	// for errors/violations; a nil slice would marshal to JSON null and break
	// every consumer exactly on the common valid-spec path.
	spec := &v1alpha1.SettingsSpec{
		ModelCatalog: &[]v1alpha1.ModelCatalogEntry{{
			Name: "claude-sonnet-5", Provider: "anthropic", Default: true,
			TokenRef: &v1alpha1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "model-default-token", Key: "token"},
		}},
	}
	res := Validate(spec, true)
	data, err := json.Marshal(res)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"errors":[]`)
	assert.Contains(t, string(data), `"violations":[]`)
	assert.NotContains(t, string(data), `"errors":null`)
	assert.NotContains(t, string(data), `"violations":null`)
}

func TestValidate_NamespaceTierRejectsTokenRef(t *testing.T) {
	spec := &v1alpha1.SettingsSpec{
		ModelCatalog: &[]v1alpha1.ModelCatalogEntry{{
			Name: "claude-sonnet-5", Provider: "anthropic",
			TokenRef: &v1alpha1.NamespacedSecretKeyRef{Namespace: "x", Name: "y", Key: "z"},
		}},
	}
	res := Validate(spec, false)
	assert.NotEmpty(t, res.Errors, "namespace tier forbids tokenRef; ModelCatalogError must fire")
}

func TestValidate_SurfacesResolverViolations(t *testing.T) {
	// A default model not present in the catalog is the canonical violation.
	spec := &v1alpha1.SettingsSpec{
		ModelCatalog: &[]v1alpha1.ModelCatalogEntry{{
			Name: "claude-sonnet-5", Provider: "anthropic", Default: true,
			TokenRef: &v1alpha1.NamespacedSecretKeyRef{Namespace: "a", Name: "b", Key: "c"},
		}},
		Defaults: &v1alpha1.SettingsDefaults{Model: &v1alpha1.DefaultModel{Provider: "openai", Name: "gpt-5.5"}},
	}
	res := Validate(spec, true)
	found := false
	for _, v := range res.Violations {
		if v.Reason == settings.ReasonModelNotInCatalog {
			found = true
		}
	}
	assert.True(t, found, "expected a %s violation, got %+v", settings.ReasonModelNotInCatalog, res.Violations)
}
