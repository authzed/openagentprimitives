package settings

import (
	"context"
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/cli"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/image"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/mcp"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/skill"
)

func TestClusterAgentSettingsWebhook(t *testing.T) {
	cases := []struct {
		name        string
		casName     string
		spec        v1.SettingsSpec
		wantAllowed bool
		wantContain string
	}{
		{
			name:        "singleton name, consistent: allowed",
			casName:     v1.ClusterAgentSettingsName,
			spec:        v1.SettingsSpec{},
			wantAllowed: true,
		},
		{
			name:    "singleton name, self-inconsistent default: denied",
			casName: v1.ClusterAgentSettingsName,
			spec: v1.SettingsSpec{
				ModelCatalog: &[]v1.ModelCatalogEntry{{
					Name: "claude-opus-4-8", Provider: "anthropic", Default: true,
					TokenRef: &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "k", Key: "token"},
				}},
				Defaults: &v1.SettingsDefaults{
					Model: &v1.DefaultModel{Provider: "anthropic", Name: "claude-haiku-4-5"},
				},
			},
			wantAllowed: false,
			wantContain: "claude-haiku-4-5",
		},
		{
			name:        "non-singleton name: denied",
			casName:     "other",
			spec:        v1.SettingsSpec{},
			wantAllowed: false,
			wantContain: v1.ClusterAgentSettingsName,
		},
		{
			name:    "unregistered pinning rule kind: denied",
			casName: v1.ClusterAgentSettingsName,
			spec: v1.SettingsSpec{
				Limits: &v1.SettingsLimits{
					Pinning: &v1.PinningPolicy{
						Rules: []v1.PinningRule{{Kind: "skil", MinStrength: "frozen"}},
					},
				},
			},
			wantAllowed: false,
			wantContain: "skil",
		},
		{
			name:    "valid pinning rule kind (mcp): allowed",
			casName: v1.ClusterAgentSettingsName,
			spec: v1.SettingsSpec{
				Limits: &v1.SettingsLimits{
					Pinning: &v1.PinningPolicy{
						Rules: []v1.PinningRule{{Kind: "mcp", MinStrength: "frozen"}},
					},
				},
			},
			wantAllowed: true,
		},
	}

	sc := scheme()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cas := &v1.ClusterAgentSettings{
				ObjectMeta: metav1.ObjectMeta{Name: tc.casName},
				Spec:       tc.spec,
			}
			c := fake.NewClientBuilder().WithScheme(sc).Build()
			h := NewClusterAgentSettingsWebhook(c, admission.NewDecoder(sc))

			resp := h.Handle(context.Background(), reqFor(t, cas))
			assert.Equal(t, tc.wantAllowed, resp.Allowed)
			if tc.wantContain != "" {
				assert.Contains(t, resp.Result.Message, tc.wantContain)
			}
		})
	}
}

func TestAgentSettingsWebhook(t *testing.T) {
	cases := []struct {
		name        string
		asName      string
		spec        v1.SettingsSpec
		wantAllowed bool
		wantContain string
	}{
		{
			name:        "singleton name, consistent: allowed",
			asName:      v1.AgentSettingsName,
			spec:        v1.SettingsSpec{},
			wantAllowed: true,
		},
		{
			name:   "singleton name, self-inconsistent default: denied",
			asName: v1.AgentSettingsName,
			spec: v1.SettingsSpec{
				ModelCatalog: &[]v1.ModelCatalogEntry{{
					Name: "claude-opus-4-8", Provider: "anthropic", Default: true,
					TokenRef: &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "k", Key: "token"},
				}},
				Defaults: &v1.SettingsDefaults{
					Model: &v1.DefaultModel{Provider: "anthropic", Name: "claude-haiku-4-5"},
				},
			},
			wantAllowed: false,
			wantContain: "claude-haiku-4-5",
		},
		{
			name:        "non-singleton name: denied",
			asName:      "not-default",
			spec:        v1.SettingsSpec{},
			wantAllowed: false,
			wantContain: v1.AgentSettingsName,
		},
		{
			name:   "unregistered bypass kind: denied",
			asName: v1.AgentSettingsName,
			spec: v1.SettingsSpec{
				Limits: &v1.SettingsLimits{
					Pinning: &v1.PinningPolicy{
						Bypass: []v1.PinningBypass{{Kind: "mcpp", Name: "x", Reason: "r"}},
					},
				},
			},
			wantAllowed: false,
			wantContain: "mcpp",
		},
		{
			name:   "valid bypass kind (skill): allowed",
			asName: v1.AgentSettingsName,
			spec: v1.SettingsSpec{
				Limits: &v1.SettingsLimits{
					Pinning: &v1.PinningPolicy{
						Bypass: []v1.PinningBypass{{Kind: "skill", Name: "repo//foo@main", Reason: "legacy"}},
					},
				},
			},
			wantAllowed: true,
		},
	}

	sc := scheme()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			as := &v1.AgentSettings{
				ObjectMeta: metav1.ObjectMeta{Name: tc.asName, Namespace: "team-a"},
				Spec:       tc.spec,
			}
			c := fake.NewClientBuilder().WithScheme(sc).Build()
			h := NewAgentSettingsWebhook(c, admission.NewDecoder(sc))

			resp := h.Handle(context.Background(), reqFor(t, as))
			assert.Equal(t, tc.wantAllowed, resp.Allowed)
			if tc.wantContain != "" {
				assert.Contains(t, resp.Result.Message, tc.wantContain)
			}
		})
	}
}
