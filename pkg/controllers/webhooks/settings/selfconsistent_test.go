package settings

import (
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/cli"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/image"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/mcp"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/skill"
	"github.com/stretchr/testify/assert"
)

func TestSelfConsistencyError(t *testing.T) {
	cases := []struct {
		name    string
		spec    v1.SettingsSpec
		wantMsg string // "" means consistent
	}{
		{
			name:    "empty spec is consistent",
			spec:    v1.SettingsSpec{},
			wantMsg: "",
		},
		{
			name: "default model in catalog: consistent",
			spec: v1.SettingsSpec{
				ModelCatalog: &[]v1.ModelCatalogEntry{{
					Name: "claude-opus-4-8", Provider: "anthropic", Default: true,
					TokenRef: &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "k", Key: "token"},
				}},
				Defaults: &v1.SettingsDefaults{
					Model: &v1.DefaultModel{Provider: "anthropic", Name: "claude-opus-4-8"},
				},
			},
			wantMsg: "",
		},
		{
			name: "default model not in catalog: inconsistent",
			spec: v1.SettingsSpec{
				ModelCatalog: &[]v1.ModelCatalogEntry{{
					Name: "claude-opus-4-8", Provider: "anthropic", Default: true,
					TokenRef: &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "k", Key: "token"},
				}},
				Defaults: &v1.SettingsDefaults{
					Model: &v1.DefaultModel{Provider: "anthropic", Name: "claude-haiku-4-5"},
				},
			},
			wantMsg: "claude-haiku-4-5",
		},
		{
			name: "name-only default model (no apiKey) is consistent: missing credential not an inconsistency",
			spec: v1.SettingsSpec{
				Defaults: &v1.SettingsDefaults{
					Model: &v1.DefaultModel{Provider: "anthropic", Name: "claude-opus-4-8"},
				},
			},
			wantMsg: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SelfConsistencyError(&tc.spec)
			if tc.wantMsg == "" {
				assert.Empty(t, got)
			} else {
				assert.Contains(t, got, tc.wantMsg)
			}
		})
	}
}

func TestPinningKindsError(t *testing.T) {
	// The four pinning kinds (cli, image, mcp, skill) are blank-imported at
	// the top of this file and register via init(). Tests below rely on all
	// four being present in the registry.
	cases := []struct {
		name    string
		spec    v1.SettingsSpec
		wantMsg string // "" means valid
	}{
		{
			name:    "nil pinning: passes",
			spec:    v1.SettingsSpec{},
			wantMsg: "",
		},
		{
			name: "nil limits: passes",
			spec: v1.SettingsSpec{
				Defaults: &v1.SettingsDefaults{},
			},
			wantMsg: "",
		},
		{
			name: "valid rule kinds (mcp, skill, image, cli): passes",
			spec: v1.SettingsSpec{
				Limits: &v1.SettingsLimits{
					Pinning: &v1.PinningPolicy{
						Rules: []v1.PinningRule{
							{Kind: "mcp", MinStrength: "frozen"},
							{Kind: "skill", MinStrength: "named"},
							{Kind: "image", MinStrength: "frozen"},
							{Kind: "cli", MinStrength: "named"},
						},
					},
				},
			},
			wantMsg: "",
		},
		{
			name: "rule kind typo 'skil': rejected with valid set in message",
			spec: v1.SettingsSpec{
				Limits: &v1.SettingsLimits{
					Pinning: &v1.PinningPolicy{
						Rules: []v1.PinningRule{
							{Kind: "skil", MinStrength: "frozen"},
						},
					},
				},
			},
			wantMsg: `"skil"`,
		},
		{
			name: "bypass kind typo: rejected",
			spec: v1.SettingsSpec{
				Limits: &v1.SettingsLimits{
					Pinning: &v1.PinningPolicy{
						Bypass: []v1.PinningBypass{
							{Kind: "mcpp", Name: "my-server", Reason: "legacy"},
						},
					},
				},
			},
			wantMsg: `"mcpp"`,
		},
		{
			name: "bypass kind typo message contains valid kinds list",
			spec: v1.SettingsSpec{
				Limits: &v1.SettingsLimits{
					Pinning: &v1.PinningPolicy{
						Bypass: []v1.PinningBypass{
							{Kind: "unknown", Name: "x", Reason: "r"},
						},
					},
				},
			},
			wantMsg: "cli",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PinningKindsError(&tc.spec)
			if tc.wantMsg == "" {
				assert.Empty(t, got)
			} else {
				assert.Contains(t, got, tc.wantMsg)
			}
		})
	}
}
