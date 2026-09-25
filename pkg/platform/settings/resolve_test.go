package settings

import (
	"testing"
	"time"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func mkBudget(turns int32, tokens int64, dur string) *v1.BudgetConfig {
	d, _ := time.ParseDuration(dur)
	return &v1.BudgetConfig{MaxTurns: turns, MaxTokens: tokens, MaxDuration: metav1.Duration{Duration: d}}
}

func TestResolve_NoSettings_PassesClassAndSessionThrough(t *testing.T) {
	in := Inputs{
		ClassModel:  &v1.ModelConfig{Provider: "anthropic", Name: "claude-opus-4-8", APIKey: v1.SecretKeyRef{Name: "k", Key: "v"}},
		ClassBudget: mkBudget(50, 1_000_000, "1h"),
	}
	got, vs := Resolve(in)
	require.Empty(t, vs, "no Settings CRs ⇒ no violations")
	assert.Equal(t, int32(50), got.Budget.MaxTurns)
	assert.Equal(t, "claude-opus-4-8", got.Model.Name)
}

func strp(ss ...string) *[]string { v := append([]string{}, ss...); return &v }

func ceil(turns int32, tokens int64, dur string) *v1.SettingsBudgetCeiling {
	d, _ := time.ParseDuration(dur)
	return &v1.SettingsBudgetCeiling{MaxTurns: turns, MaxTokens: tokens, MaxDuration: metav1.Duration{Duration: d}}
}

func boolPtr(b bool) *bool { return &b }

func clusterWithCatalog(entries []v1.ModelCatalogEntry, denied []string, override *bool) *v1.SettingsSpec {
	cat := append([]v1.ModelCatalogEntry{}, entries...)
	return &v1.SettingsSpec{
		ModelCatalog: &cat,
		Limits:       &v1.SettingsLimits{DeniedModels: denied, AllowModelOverride: override},
	}
}

var opusEntry = v1.ModelCatalogEntry{
	Name: "claude-opus-4-8", Provider: "anthropic", Default: true,
	TokenRef: &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "anthropic-key", Key: "token"},
}
var haikuEntry = v1.ModelCatalogEntry{
	Name: "claude-haiku-4-5", Provider: "anthropic",
	TokenRef: &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "cheap-key", Key: "token"},
}

func firstFatalReason(vs []Violation) string {
	for _, v := range vs {
		if v.Fatal {
			return v.Reason
		}
	}
	return ""
}

func TestResolve_Budget(t *testing.T) {
	t.Run("class request under ceiling ⇒ unchanged, no warning", func(t *testing.T) {
		in := Inputs{
			ClassBudget: mkBudget(20, 500_000, "30m"),
			Namespace:   &v1.SettingsSpec{Limits: &v1.SettingsLimits{Budget: ceil(100, 2_000_000, "2h")}},
		}
		got, vs := Resolve(in)
		assert.Equal(t, int32(20), got.Budget.MaxTurns)
		assert.Empty(t, vs)
	})

	t.Run("class request over ceiling ⇒ clamped + BudgetClamped warning", func(t *testing.T) {
		in := Inputs{
			ClassBudget: mkBudget(500, 9_000_000, "10h"),
			Cluster:     &v1.SettingsSpec{Limits: &v1.SettingsLimits{Budget: ceil(100, 2_000_000, "2h")}},
		}
		got, vs := Resolve(in)
		assert.Equal(t, int32(100), got.Budget.MaxTurns)
		assert.Equal(t, int64(2_000_000), got.Budget.MaxTokens)
		require.Len(t, vs, 1)
		assert.Equal(t, ReasonBudgetClamped, vs[0].Reason)
		assert.False(t, vs[0].Fatal)
		assert.Equal(t, "clamped", got.Provenance["budget.maxTurns"])
	})

	t.Run("session over class ⇒ clamped to class", func(t *testing.T) {
		in := Inputs{
			ClassBudget:   mkBudget(20, 500_000, "30m"),
			SessionBudget: mkBudget(80, 500_000, "30m"),
		}
		got, vs := Resolve(in)
		assert.Equal(t, int32(20), got.Budget.MaxTurns, "class caps session")
		require.Len(t, vs, 1)
		assert.Equal(t, ReasonBudgetClamped, vs[0].Reason)
	})

	t.Run("class omits budget ⇒ inherits namespace default", func(t *testing.T) {
		in := Inputs{
			Namespace: &v1.SettingsSpec{Defaults: &v1.SettingsDefaults{Budget: mkBudget(15, 300_000, "20m")}},
		}
		got, _ := Resolve(in)
		assert.Equal(t, int32(15), got.Budget.MaxTurns)
		assert.Equal(t, "namespace", got.Provenance["budget"])
	})

	t.Run("default inherited but over ceiling ⇒ clamped, no warning (defaults are best-effort)", func(t *testing.T) {
		in := Inputs{
			Cluster: &v1.SettingsSpec{
				Limits:   &v1.SettingsLimits{Budget: ceil(10, 0, "")},
				Defaults: &v1.SettingsDefaults{Budget: mkBudget(50, 300_000, "20m")},
			},
		}
		got, vs := Resolve(in)
		assert.Equal(t, int32(10), got.Budget.MaxTurns)
		assert.Empty(t, vs, "clamping an inherited default is silent (not an explicit over-ask)")
	})

	t.Run("no budget requested but ceiling exists ⇒ ceiling becomes the value, no warning", func(t *testing.T) {
		in := Inputs{
			Cluster: &v1.SettingsSpec{Limits: &v1.SettingsLimits{Budget: ceil(40, 0, "")}},
		}
		got, vs := Resolve(in)
		assert.Equal(t, int32(40), got.Budget.MaxTurns)
		assert.Empty(t, vs)
	})
}

func TestResolveBudget_SessionExpiration(t *testing.T) {
	hour := metav1.Duration{Duration: time.Hour}
	twoHour := metav1.Duration{Duration: 2 * time.Hour}

	cases := []struct {
		name           string
		in             Inputs
		wantExp        time.Duration
		wantClampedMsg string // non-empty: expect one BudgetClamped violation whose Message contains this
	}{
		{
			name: "unset everywhere: zero (no expiry, no default)",
			in: Inputs{
				ClassBudget: &v1.BudgetConfig{MaxTurns: 10, MaxTokens: 100},
			},
			wantExp: 0,
		},
		{
			name: "class value passes through",
			in: Inputs{
				ClassBudget: &v1.BudgetConfig{MaxTurns: 10, MaxTokens: 100, SessionExpiration: twoHour},
			},
			wantExp: 2 * time.Hour,
		},
		{
			name: "cluster ceiling clamps a larger class value",
			in: Inputs{
				ClassBudget: &v1.BudgetConfig{MaxTurns: 10, MaxTokens: 100, SessionExpiration: twoHour},
				Cluster:     &v1.SettingsSpec{Limits: &v1.SettingsLimits{Budget: &v1.SettingsBudgetCeiling{SessionExpiration: hour}}},
			},
			wantExp:        time.Hour,
			wantClampedMsg: "sessionExpiration",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, vs := Resolve(tc.in)
			assert.Equal(t, tc.wantExp, out.Budget.SessionExpiration.Duration)
			if tc.wantClampedMsg != "" {
				require.Len(t, vs, 1)
				assert.Equal(t, ReasonBudgetClamped, vs[0].Reason)
				assert.Contains(t, vs[0].Message, tc.wantClampedMsg)
			}
		})
	}
}

func TestIntersectAllowlist(t *testing.T) {
	cases := []struct {
		name  string
		tiers []*[]string
		want  []string // nil = unconstrained
	}{
		{name: "all nil ⇒ unconstrained (nil)", tiers: []*[]string{nil, nil}, want: nil},
		{name: "one tier sets a list ⇒ that list", tiers: []*[]string{strp("a", "b"), nil}, want: []string{"a", "b"}},
		{name: "two tiers ⇒ intersection", tiers: []*[]string{strp("a", "b", "c"), strp("b", "c", "d")}, want: []string{"b", "c"}},
		{name: "explicit empty list ⇒ deny-all (empty non-nil)", tiers: []*[]string{strp(), nil}, want: []string{}},
		{name: "intersection to empty ⇒ deny-all", tiers: []*[]string{strp("a"), strp("b")}, want: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := intersectAllowlist(tc.tiers...)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestResolve_Model(t *testing.T) {
	full := func(name string) *v1.ModelConfig {
		return &v1.ModelConfig{Provider: "anthropic", Name: name, APIKey: v1.SecretKeyRef{Name: "k", Key: "v"}}
	}

	t.Run("class model resolves via legacy path (no catalog)", func(t *testing.T) {
		in := Inputs{
			ClassModel: full("claude-opus-4-8"),
		}
		got, vs := Resolve(in)
		assert.Equal(t, "claude-opus-4-8", got.Model.Name)
		assert.Empty(t, vs)
	})

	t.Run("denied model ⇒ fatal ModelForbidden", func(t *testing.T) {
		in := Inputs{
			ClassModel: &v1.ModelConfig{FromCatalog: "claude-haiku-4-5"},
			Cluster:    clusterWithCatalog([]v1.ModelCatalogEntry{opusEntry, haikuEntry}, []string{"claude-haiku-4-5"}, boolPtr(true)),
		}
		_, vs := Resolve(in)
		require.Len(t, vs, 1)
		assert.Equal(t, ReasonModelForbidden, vs[0].Reason)
		assert.True(t, vs[0].Fatal)
	})

	t.Run("class omits model ⇒ inherits namespace default model + apiKey", func(t *testing.T) {
		in := Inputs{
			Namespace: &v1.SettingsSpec{Defaults: &v1.SettingsDefaults{Model: &v1.DefaultModel{
				Provider: "anthropic", Name: "claude-sonnet-4-6", APIKey: &v1.SecretKeyRef{Name: "k", Key: "v"},
			}}},
		}
		got, vs := Resolve(in)
		assert.Equal(t, "claude-sonnet-4-6", got.Model.Name)
		assert.Equal(t, "k", got.Model.APIKey.Name)
		assert.Empty(t, vs)
		assert.Equal(t, "namespace", got.Provenance["model"])
	})

	t.Run("cluster defaults name only; class supplies apiKey ⇒ merged", func(t *testing.T) {
		in := Inputs{
			Cluster:    &v1.SettingsSpec{Defaults: &v1.SettingsDefaults{Model: &v1.DefaultModel{Provider: "anthropic", Name: "claude-sonnet-4-6"}}},
			ClassModel: &v1.ModelConfig{APIKey: v1.SecretKeyRef{Name: "k", Key: "v"}}, // name empty
		}
		got, vs := Resolve(in)
		assert.Equal(t, "claude-sonnet-4-6", got.Model.Name)
		assert.Equal(t, "k", got.Model.APIKey.Name)
		assert.Empty(t, vs)
	})

	t.Run("no model anywhere, ForSession ⇒ fatal ModelMissingModel", func(t *testing.T) {
		_, vs := Resolve(Inputs{ForSession: true})
		require.Len(t, vs, 1)
		assert.Equal(t, ReasonModelMissingModel, vs[0].Reason)
		assert.True(t, vs[0].Fatal)
	})

	t.Run("no model anywhere, class-only ⇒ no violation (deferred)", func(t *testing.T) {
		_, vs := Resolve(Inputs{ForSession: false})
		assert.Empty(t, vs)
	})

	t.Run("name but no apiKey, ForSession ⇒ fatal ModelMissingCredential", func(t *testing.T) {
		in := Inputs{
			ForSession: true,
			Cluster:    &v1.SettingsSpec{Defaults: &v1.SettingsDefaults{Model: &v1.DefaultModel{Provider: "anthropic", Name: "claude-sonnet-4-6"}}},
		}
		_, vs := Resolve(in)
		require.Len(t, vs, 1)
		assert.Equal(t, ReasonModelMissingCredential, vs[0].Reason)
		assert.True(t, vs[0].Fatal)
	})
}

func TestResolveModel_Catalog(t *testing.T) {
	cases := []struct {
		name       string
		in         Inputs
		wantName   string
		wantSrc    *v1.NamespacedSecretKeyRef
		wantReason string // "" = no fatal
	}{
		{
			name:     "omitted class model inherits the catalog default with its token",
			in:       Inputs{Cluster: clusterWithCatalog([]v1.ModelCatalogEntry{opusEntry, haikuEntry}, nil, nil), ForSession: true},
			wantName: "claude-opus-4-8",
			wantSrc:  opusEntry.TokenRef,
		},
		{
			name:     "class fromCatalog selects a non-default entry's token",
			in:       Inputs{Cluster: clusterWithCatalog([]v1.ModelCatalogEntry{opusEntry, haikuEntry}, nil, nil), ClassModel: &v1.ModelConfig{FromCatalog: "claude-haiku-4-5"}, ForSession: true},
			wantName: "claude-haiku-4-5",
			wantSrc:  haikuEntry.TokenRef,
		},
		{
			name:       "model not in the catalog is fatal",
			in:         Inputs{Cluster: clusterWithCatalog([]v1.ModelCatalogEntry{opusEntry}, nil, nil), ClassModel: &v1.ModelConfig{Name: "claude-sonnet-4-6"}, ForSession: true},
			wantReason: ReasonModelNotInCatalog,
		},
		{
			name:       "denied model is fatal",
			in:         Inputs{Cluster: clusterWithCatalog([]v1.ModelCatalogEntry{opusEntry, haikuEntry}, []string{"claude-haiku-4-5"}, nil), ClassModel: &v1.ModelConfig{FromCatalog: "claude-haiku-4-5"}, ForSession: true},
			wantReason: ReasonModelForbidden,
		},
		{
			name:       "BYO token without override is fatal",
			in:         Inputs{Cluster: clusterWithCatalog([]v1.ModelCatalogEntry{opusEntry}, nil, nil), ClassModel: &v1.ModelConfig{Provider: "anthropic", Name: "claude-opus-4-8", APIKey: v1.SecretKeyRef{Name: "byo", Key: "k"}}, ForSession: true},
			wantReason: ReasonModelOverrideNotAllowed,
		},
		{
			name:     "BYO token with cluster-granted override is permitted",
			in:       Inputs{Cluster: clusterWithCatalog([]v1.ModelCatalogEntry{opusEntry}, nil, boolPtr(true)), ClassModel: &v1.ModelConfig{Provider: "anthropic", Name: "byo-model", APIKey: v1.SecretKeyRef{Name: "byo", Key: "k"}}, ForSession: true},
			wantName: "byo-model",
			wantSrc:  nil,
		},
		{
			name: "namespace cannot grant override the cluster withheld",
			in: Inputs{
				Cluster:    clusterWithCatalog([]v1.ModelCatalogEntry{opusEntry}, nil, boolPtr(false)),
				Namespace:  &v1.SettingsSpec{Limits: &v1.SettingsLimits{AllowModelOverride: boolPtr(true)}},
				ClassModel: &v1.ModelConfig{Provider: "anthropic", Name: "byo", APIKey: v1.SecretKeyRef{Name: "byo", Key: "k"}},
				ForSession: true,
			},
			wantReason: ReasonModelOverrideNotAllowed,
		},
		{
			name: "namespace narrowing to zero cluster-overlap denies all (no legacy bypass)",
			in: Inputs{
				Cluster:    clusterWithCatalog([]v1.ModelCatalogEntry{opusEntry}, nil, nil),
				Namespace:  &v1.SettingsSpec{ModelCatalog: &[]v1.ModelCatalogEntry{{Name: "bogus"}}},
				ClassModel: &v1.ModelConfig{Provider: "anthropic", Name: "claude-opus-4-8", APIKey: v1.SecretKeyRef{Name: "byo", Key: "k"}},
				ForSession: true,
			},
			wantReason: ReasonModelOverrideNotAllowed, // BYO key still gated; catalog NOT bypassed
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, src, _, _, _, vs := resolveModel(tc.in, map[string]string{})
			if tc.wantReason == "" {
				assert.Empty(t, firstFatalReason(vs), "no fatal expected")
				assert.Equal(t, tc.wantName, got.Name)
				assert.Equal(t, tc.wantSrc, src)
			} else {
				assert.Equal(t, tc.wantReason, firstFatalReason(vs))
			}
		})
	}
}

func TestResolveModel_Routing(t *testing.T) {
	openrouterEntry := v1.ModelCatalogEntry{
		Name: "openrouter/auto", Provider: "openrouter",
		TokenRef: &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "openrouter-key", Key: "token"},
		Routing:  &v1.OpenRouterRouting{Models: []string{"a/b", "c/d"}},
	}

	t.Run("openrouter catalog entry ⇒ EffectiveSettings.ModelRouting populated", func(t *testing.T) {
		in := Inputs{
			Cluster:    clusterWithCatalog([]v1.ModelCatalogEntry{openrouterEntry}, nil, nil),
			ClassModel: &v1.ModelConfig{FromCatalog: "openrouter/auto"},
			ForSession: true,
		}
		eff, vs := Resolve(in)
		require.Empty(t, firstFatalReason(vs))
		require.NotNil(t, eff.ModelRouting)
		assert.Equal(t, []string{"a/b", "c/d"}, eff.ModelRouting.Models)
		// Deep-copied, not aliased to the catalog entry's Routing.
		assert.NotSame(t, openrouterEntry.Routing, eff.ModelRouting)
	})

	t.Run("non-openrouter catalog entry ⇒ ModelRouting stays nil", func(t *testing.T) {
		in := Inputs{
			Cluster:    clusterWithCatalog([]v1.ModelCatalogEntry{opusEntry, haikuEntry}, nil, nil),
			ForSession: true,
		}
		eff, vs := Resolve(in)
		require.Empty(t, firstFatalReason(vs))
		assert.Nil(t, eff.ModelRouting)
	})
}

// TestResolveModel_RoutingMetadata covers the narrowing-only RoutingMetadata
// merge at the Resolve() level: the class's RoutingMetadata refines the catalog
// entry's Routing via mergeRouting, is inert on non-openrouter providers, and
// stands alone when the catalog entry has no Routing of its own.
func TestResolveModel_RoutingMetadata(t *testing.T) {
	openrouterEntry := v1.ModelCatalogEntry{
		Name: "openrouter/auto", Provider: "openrouter",
		TokenRef: &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "openrouter-key", Key: "token"},
		Routing:  &v1.OpenRouterRouting{Only: []string{"anthropic", "openai"}},
	}
	openrouterEntryNoRouting := v1.ModelCatalogEntry{
		Name: "openrouter/auto-bare", Provider: "openrouter",
		TokenRef: &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "openrouter-key", Key: "token"},
	}

	t.Run("AgentClass metadata narrows the catalog's routing (agent cannot widen Only)", func(t *testing.T) {
		in := Inputs{
			Cluster: clusterWithCatalog([]v1.ModelCatalogEntry{openrouterEntry}, nil, nil),
			ClassModel: &v1.ModelConfig{
				FromCatalog:     "openrouter/auto",
				RoutingMetadata: &v1.OpenRouterRouting{Only: []string{"anthropic", "google"}},
			},
			ForSession: true,
		}
		eff, vs := Resolve(in)
		require.Empty(t, firstFatalReason(vs))
		require.NotNil(t, eff.ModelRouting)
		assert.Equal(t, []string{"anthropic"}, eff.ModelRouting.Only, "narrowed to the intersection, not the class's wider set")
	})

	t.Run("non-openrouter provider ⇒ ModelRouting stays nil even when the class sets RoutingMetadata (inert)", func(t *testing.T) {
		in := Inputs{
			Cluster: clusterWithCatalog([]v1.ModelCatalogEntry{opusEntry}, nil, nil),
			ClassModel: &v1.ModelConfig{
				FromCatalog:     "claude-opus-4-8",
				RoutingMetadata: &v1.OpenRouterRouting{Only: []string{"anthropic"}},
			},
			ForSession: true,
		}
		eff, vs := Resolve(in)
		require.Empty(t, firstFatalReason(vs))
		assert.Nil(t, eff.ModelRouting, "RoutingMetadata must have no effect on a non-openrouter provider")
	})

	t.Run("catalog entry has no Routing ⇒ the class's RoutingMetadata alone becomes the effective routing", func(t *testing.T) {
		in := Inputs{
			Cluster: clusterWithCatalog([]v1.ModelCatalogEntry{openrouterEntryNoRouting}, nil, nil),
			ClassModel: &v1.ModelConfig{
				FromCatalog:     "openrouter/auto-bare",
				RoutingMetadata: &v1.OpenRouterRouting{Sort: "price"},
			},
			ForSession: true,
		}
		eff, vs := Resolve(in)
		require.Empty(t, firstFatalReason(vs))
		require.NotNil(t, eff.ModelRouting)
		assert.Equal(t, "price", eff.ModelRouting.Sort)
	})

	t.Run("openrouter entry with nil Routing and nil RoutingMetadata ⇒ ModelRouting stays nil", func(t *testing.T) {
		in := Inputs{
			Cluster:    clusterWithCatalog([]v1.ModelCatalogEntry{openrouterEntryNoRouting}, nil, nil),
			ClassModel: &v1.ModelConfig{FromCatalog: "openrouter/auto-bare"},
			ForSession: true,
		}
		eff, vs := Resolve(in)
		require.Empty(t, firstFatalReason(vs))
		assert.Nil(t, eff.ModelRouting)
	})
}

func TestResolve_ToolkitAndMCP(t *testing.T) {
	t.Run("toolkit outside allowlist ⇒ fatal ToolkitNotAllowed", func(t *testing.T) {
		in := Inputs{
			ClassToolkits: []string{"git", "kubectl"},
			Namespace:     &v1.SettingsSpec{Limits: &v1.SettingsLimits{AllowedToolkits: strp("git")}},
		}
		_, vs := Resolve(in)
		require.Len(t, vs, 1)
		assert.Equal(t, ReasonToolkitNotAllowed, vs[0].Reason)
		assert.True(t, vs[0].Fatal)
		assert.Contains(t, vs[0].Message, "kubectl")
	})

	t.Run("MCP server outside allowlist ⇒ fatal MCPServerNotAllowed", func(t *testing.T) {
		in := Inputs{
			ClassMCP:  []MCPRequest{{Server: "github"}, {Server: "secret-server"}},
			Namespace: &v1.SettingsSpec{Limits: &v1.SettingsLimits{AllowedMCPServers: &[]v1.AllowedMCPServer{{Name: "github", Tools: []string{"*"}}}}},
		}
		_, vs := Resolve(in)
		require.Len(t, vs, 1)
		assert.Equal(t, ReasonMCPServerNotAllowed, vs[0].Reason)
		assert.Contains(t, vs[0].Message, "secret-server")
	})

	t.Run("MCP tool outside per-server allowlist ⇒ fatal MCPToolNotAllowed", func(t *testing.T) {
		in := Inputs{
			ClassMCP:  []MCPRequest{{Server: "jira", Tools: []string{"search_issues", "delete_project"}}},
			Namespace: &v1.SettingsSpec{Limits: &v1.SettingsLimits{AllowedMCPServers: &[]v1.AllowedMCPServer{{Name: "jira", Tools: []string{"search_issues", "get_issue"}}}}},
		}
		_, vs := Resolve(in)
		require.Len(t, vs, 1)
		assert.Equal(t, ReasonMCPToolNotAllowed, vs[0].Reason)
		assert.Contains(t, vs[0].Message, "delete_project")
	})

	t.Run("no allowlist ⇒ everything permitted", func(t *testing.T) {
		in := Inputs{ClassToolkits: []string{"anything"}, ClassMCP: []MCPRequest{{Server: "x", Tools: []string{"y"}}}}
		_, vs := Resolve(in)
		assert.Empty(t, vs)
	})

	t.Run("two-tier MCP allowlist ⇒ server + tool intersection enforced", func(t *testing.T) {
		in := Inputs{
			ClassMCP: []MCPRequest{{Server: "jira", Tools: []string{"search_issues"}}},
			Cluster: &v1.SettingsSpec{Limits: &v1.SettingsLimits{AllowedMCPServers: &[]v1.AllowedMCPServer{
				{Name: "jira", Tools: []string{"search_issues", "get_issue"}},
				{Name: "github", Tools: []string{"*"}},
			}}},
			Namespace: &v1.SettingsSpec{Limits: &v1.SettingsLimits{AllowedMCPServers: &[]v1.AllowedMCPServer{
				{Name: "jira", Tools: []string{"search_issues"}},
			}}},
		}
		got, vs := Resolve(in)
		assert.Empty(t, vs, "jira/search_issues is in both tiers' intersection")
		require.Len(t, got.AllowedMCP, 1, "github dropped by namespace; only jira survives")
		assert.Equal(t, "jira", got.AllowedMCP[0].Name)
		assert.Equal(t, []string{"search_issues"}, got.AllowedMCP[0].Tools)
	})

	t.Run("class requests a restricted server without enumerating tools ⇒ fatal", func(t *testing.T) {
		in := Inputs{
			ClassMCP:  []MCPRequest{{Server: "jira"}},
			Namespace: &v1.SettingsSpec{Limits: &v1.SettingsLimits{AllowedMCPServers: &[]v1.AllowedMCPServer{{Name: "jira", Tools: []string{"search_issues"}}}}},
		}
		_, vs := Resolve(in)
		require.Len(t, vs, 1)
		assert.Equal(t, ReasonMCPToolNotAllowed, vs[0].Reason)
		assert.True(t, vs[0].Fatal)
	})
}

func durp(s string) *metav1.Duration {
	d, _ := time.ParseDuration(s)
	return &metav1.Duration{Duration: d}
}

func TestResolve_Authz(t *testing.T) {
	t.Run("class omits, namespace default supplies approvalTimeout", func(t *testing.T) {
		in := Inputs{
			Namespace: &v1.SettingsSpec{Defaults: &v1.SettingsDefaults{Authz: &v1.DefaultAuthz{ApprovalTimeout: durp("3m")}}},
		}
		got, _ := Resolve(in)
		assert.Equal(t, 3*time.Minute, got.Authz.ApprovalTimeout)
	})

	t.Run("class value wins over tier default", func(t *testing.T) {
		in := Inputs{
			ClassAuthz: &v1.AuthzBlock{ApprovalTimeout: durp("7m")},
			Cluster:    &v1.SettingsSpec{Defaults: &v1.SettingsDefaults{Authz: &v1.DefaultAuthz{ApprovalTimeout: durp("3m")}}},
		}
		got, _ := Resolve(in)
		assert.Equal(t, 7*time.Minute, got.Authz.ApprovalTimeout)
	})

	t.Run("nothing set ⇒ hard-coded default 10m", func(t *testing.T) {
		got, _ := Resolve(Inputs{})
		assert.Equal(t, 10*time.Minute, got.Authz.ApprovalTimeout)
		assert.Equal(t, int32(5000), got.Authz.ScopeMaxLLMLatencyMs)
	})
}

func TestResolve_Skills(t *testing.T) {
	t.Run("skill outside allow ceiling ⇒ fatal SkillNotAllowed", func(t *testing.T) {
		in := Inputs{
			ClassSkills: []string{"github.com/o/r//skills/x@v1", "github.com/evil/r//skills/y@v1"},
			Namespace:   &v1.SettingsSpec{Limits: &v1.SettingsLimits{AllowedSkills: strp("github.com/o/**")}},
		}
		_, vs := Resolve(in)
		require.Len(t, vs, 1)
		assert.Equal(t, ReasonSkillNotAllowed, vs[0].Reason)
		assert.True(t, vs[0].Fatal)
		assert.Contains(t, vs[0].Message, "github.com/evil")
	})

	t.Run("skill must satisfy EVERY constraining tier", func(t *testing.T) {
		in := Inputs{
			ClassSkills: []string{"github.com/o/r//skills/x@v1"},
			Cluster:     &v1.SettingsSpec{Limits: &v1.SettingsLimits{AllowedSkills: strp("github.com/o/**")}},
			Namespace:   &v1.SettingsSpec{Limits: &v1.SettingsLimits{AllowedSkills: strp("github.com/o/r//skills/y@*")}}, // excludes x
		}
		_, vs := Resolve(in)
		require.Len(t, vs, 1)
		assert.Equal(t, ReasonSkillNotAllowed, vs[0].Reason)
	})

	t.Run("denied pattern ⇒ fatal SkillDenied even when allowed", func(t *testing.T) {
		in := Inputs{
			ClassSkills: []string{"github.com/o/r//skills/x@v1"},
			Namespace: &v1.SettingsSpec{Limits: &v1.SettingsLimits{
				AllowedSkills: strp("github.com/o/**"),
				DeniedSkills:  []string{"github.com/o/r//skills/x@*"},
			}},
		}
		_, vs := Resolve(in)
		require.Len(t, vs, 1)
		assert.Equal(t, ReasonSkillDenied, vs[0].Reason)
		assert.True(t, vs[0].Fatal)
	})

	t.Run("no ceilings ⇒ all class skills allowed", func(t *testing.T) {
		in := Inputs{ClassSkills: []string{"github.com/o/r//skills/x@v1"}}
		_, vs := Resolve(in)
		assert.Empty(t, vs)
	})

	t.Run("effective allowed/denied unions surfaced in EffectiveSettings", func(t *testing.T) {
		in := Inputs{
			Cluster:   &v1.SettingsSpec{Limits: &v1.SettingsLimits{AllowedSkills: strp("a//x")}},
			Namespace: &v1.SettingsSpec{Limits: &v1.SettingsLimits{AllowedSkills: strp("b//y"), DeniedSkills: []string{"c//z"}}},
		}
		eff, _ := Resolve(in)
		assert.ElementsMatch(t, []string{"a//x", "b//y"}, eff.AllowedSkills)
		assert.ElementsMatch(t, []string{"c//z"}, eff.DeniedSkills)
	})
}

// hasFatal reports whether vs contains a fatal Violation with the given reason.
func hasFatal(vs []Violation, reason string) bool {
	for _, v := range vs {
		if v.Reason == reason && v.Fatal {
			return true
		}
	}
	return false
}

// skillPinCeiling builds a settings tier whose only limit is a blocking
// skill pin floor at the given strength ("frozen" or "named").
func skillPinCeiling(minStrength string) *v1.SettingsSpec {
	return &v1.SettingsSpec{Limits: &v1.SettingsLimits{
		Pinning: &v1.PinningPolicy{Rules: []v1.PinningRule{
			{Kind: "skill", MinStrength: minStrength, Mode: v1.PinModeBlock},
		}},
	}}
}

func TestResolve_SkillPinning(t *testing.T) {
	t.Run("a frozen floor rejects a tag-pinned skill", func(t *testing.T) {
		in := Inputs{
			ClassSkills: []string{"github.com/o/r//skills/x@v1.2.0"}, // tag = Named
			Namespace:   skillPinCeiling("frozen"),
		}
		_, vs := Resolve(in)
		require.True(t, hasFatal(vs, ReasonSkillPinningRequired))
	})

	t.Run("a frozen floor accepts a SHA-pinned skill", func(t *testing.T) {
		in := Inputs{
			ClassSkills: []string{"github.com/o/r//skills/x@a1b2c3d"}, // sha = Frozen
			Namespace:   skillPinCeiling("frozen"),
		}
		_, vs := Resolve(in)
		assert.False(t, hasFatal(vs, ReasonSkillPinningRequired))
	})

	t.Run("a named floor rejects an unpinned skill but accepts a tag", func(t *testing.T) {
		in := Inputs{
			ClassSkills: []string{"github.com/o/r//skills/x", "github.com/o/r//skills/y@v1"},
			Namespace:   skillPinCeiling("named"),
		}
		_, vs := Resolve(in)
		// exactly one fatal pinning violation (the unpinned x)
		n := 0
		for _, v := range vs {
			if v.Reason == ReasonSkillPinningRequired && v.Fatal {
				n++
				assert.Contains(t, v.Message, "skills/x")
			}
		}
		assert.Equal(t, 1, n)
	})

	t.Run("unpinned skill yields a non-fatal rolling warning when no tier sets a floor", func(t *testing.T) {
		in := Inputs{ClassSkills: []string{"github.com/o/r//skills/x"}}
		_, vs := Resolve(in)
		var warn *Violation
		for i := range vs {
			if vs[i].Reason == ReasonSkillRolling {
				warn = &vs[i]
			}
		}
		require.NotNil(t, warn)
		assert.False(t, warn.Fatal, "rolling is a warning, not fatal")
	})

	t.Run("strongest floor across tiers wins", func(t *testing.T) {
		in := Inputs{
			ClassSkills: []string{"github.com/o/r//skills/x@v1"}, // tag-pinned
			Cluster:     skillPinCeiling("named"),
			Namespace:   skillPinCeiling("frozen"), // frozen wins
		}
		_, vs := Resolve(in)
		require.True(t, hasFatal(vs, ReasonSkillPinningRequired)) // named < frozen → rejected
	})
}

func TestFoldToolGuardCeilingByteLimitsMinWins(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	cluster := &v1.ToolGuardCeiling{MaxEgressBytes: i64(2048), MaxIngressBytes: i64(8192), MaxUIIngressBytes: i64(16 << 20)}
	ns := &v1.ToolGuardCeiling{MaxEgressBytes: i64(1024), MaxUIIngressBytes: i64(4 << 20)} // stricter egress + UI ingress; no ingress

	got, vs := foldToolGuardCeiling(cluster, ns)
	assert.Empty(t, vs, "no rate bound was authored, so nothing to report")
	require.NotNil(t, got)
	require.NotNil(t, got.MaxEgressBytes)
	assert.Equal(t, int64(1024), *got.MaxEgressBytes, "min egress wins across tiers")
	require.NotNil(t, got.MaxIngressBytes)
	assert.Equal(t, int64(8192), *got.MaxIngressBytes, "only-cluster ingress carried through")
	require.NotNil(t, got.MaxUIIngressBytes, "the UI ceiling folds strictest-across-tiers same as the model one")
	assert.Equal(t, int64(4<<20), *got.MaxUIIngressBytes, "min UI ingress wins across tiers")
}

func TestResolve_Integration_FullChain(t *testing.T) {
	in := Inputs{
		Cluster: &v1.SettingsSpec{
			Limits:   &v1.SettingsLimits{Budget: ceil(100, 0, "")},
			Defaults: &v1.SettingsDefaults{Model: &v1.DefaultModel{Provider: "anthropic", Name: "claude-sonnet-4-6"}},
		},
		Namespace: &v1.SettingsSpec{
			Limits:   &v1.SettingsLimits{Budget: ceil(50, 0, "")},
			Defaults: &v1.SettingsDefaults{Model: &v1.DefaultModel{Provider: "anthropic", Name: "claude-opus-4-8", APIKey: &v1.SecretKeyRef{Name: "k", Key: "v"}}},
		},
		ClassBudget: mkBudget(80, 500_000, "1h"), // 80 > ns ceiling 50 ⇒ clamp to 50
		ForSession:  true,
	}
	got, vs := Resolve(in)
	assert.Equal(t, int32(50), got.Budget.MaxTurns)
	assert.Equal(t, "claude-opus-4-8", got.Model.Name, "class omits model ⇒ namespace default wins over cluster default")
	assert.Equal(t, "k", got.Model.APIKey.Name)
	// exactly one non-fatal BudgetClamped, no fatals
	require.Len(t, vs, 1)
	assert.Equal(t, ReasonBudgetClamped, vs[0].Reason)
	assert.False(t, vs[0].Fatal)
}

func TestResolve_NativeFileHandling(t *testing.T) {
	cases := []struct {
		name string
		in   Inputs
		want bool
	}{
		{
			name: "no config ⇒ off",
			in:   Inputs{},
			want: false,
		},
		{
			name: "cluster grants ⇒ on",
			in: Inputs{
				Cluster: &v1.SettingsSpec{Limits: &v1.SettingsLimits{NativeFileHandling: boolPtr(true)}},
			},
			want: true,
		},
		{
			name: "cluster grants but namespace restricts ⇒ off",
			in: Inputs{
				Cluster:   &v1.SettingsSpec{Limits: &v1.SettingsLimits{NativeFileHandling: boolPtr(true)}},
				Namespace: &v1.SettingsSpec{Limits: &v1.SettingsLimits{NativeFileHandling: boolPtr(false)}},
			},
			want: false,
		},
		{
			name: "cluster nil (namespace alone cannot grant) ⇒ off",
			in: Inputs{
				Namespace: &v1.SettingsSpec{Limits: &v1.SettingsLimits{NativeFileHandling: boolPtr(true)}},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, vs := Resolve(tc.in)
			assert.Empty(t, vs)
			assert.Equal(t, tc.want, got.NativeFileHandling)
		})
	}
}

// limitsWith builds a SettingsSpec whose only content is the
// RequireSubagentDigestPins flag, for TestResolve_RequireSubagentDigestPins.
func limitsWith(b *bool) *v1.SettingsSpec {
	return &v1.SettingsSpec{Limits: &v1.SettingsLimits{RequireSubagentDigestPins: b}}
}

// TestResolve_RequireSubagentDigestPins exercises requirement semantics,
// which fold differently than NativeFileHandling's grant semantics: a grant
// needs the cluster's permission before a namespace's "true" counts for
// anything, but a requirement is a ratchet — ANY tier setting true makes it
// true, and no lower tier can relax one a higher tier already set.
func TestResolve_RequireSubagentDigestPins(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name               string
		cluster, namespace *v1.SettingsSpec
		want               bool
	}{
		{"unset everywhere: not required", nil, nil, false},
		{"cluster requires: required", limitsWith(&on), nil, true},
		{"cluster requires, namespace tries to relax: still required", limitsWith(&on), limitsWith(&off), true},
		{"cluster silent, namespace requires: required", nil, limitsWith(&on), true},
		{"cluster explicitly off, namespace requires: required", limitsWith(&off), limitsWith(&on), true},
		{"both explicitly off: not required", limitsWith(&off), limitsWith(&off), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eff, vs := Resolve(Inputs{Cluster: tc.cluster, Namespace: tc.namespace})
			require.Empty(t, vs)
			assert.Equal(t, tc.want, eff.RequireSubagentDigestPins)
		})
	}
}

// Resolve surfaces the sandbox decision alongside every other resolved
// setting, so a caller gets one snapshot rather than two.
func TestResolve_IncludesSandboxDecisionAndProvenance(t *testing.T) {
	kinds := []string{"pod"}
	eff, vs := Resolve(Inputs{
		Cluster: &v1.SettingsSpec{
			Defaults: &v1.SettingsDefaults{Sandbox: &v1.SandboxBackend{Kind: "pod"}},
			Limits:   &v1.SettingsLimits{AllowedSandboxKinds: &kinds},
		},
		BundleSandbox: map[string]BundleSandboxInputs{"demo-bundle": {}},
	})

	require.Empty(t, vs)
	require.Contains(t, eff.Sandbox, "demo-bundle")
	assert.Equal(t, "pod", eff.Sandbox["demo-bundle"].Kind)
	assert.Equal(t, []string{"pod"}, eff.AllowedSandboxKinds)
	assert.Equal(t, "cluster", eff.Provenance["sandbox.demo-bundle.kind"])
}

// A ceiling violation must be fatal and must reach the caller through the same
// Violations channel as every other governance failure.
func TestResolve_ForbiddenSandboxKindIsFatal(t *testing.T) {
	kinds := []string{"pod"}
	_, vs := Resolve(Inputs{
		Cluster: &v1.SettingsSpec{Limits: &v1.SettingsLimits{AllowedSandboxKinds: &kinds}},
		BundleSandbox: map[string]BundleSandboxInputs{
			"demo-bundle": {FromAgentClass: &v1.SandboxBackend{Kind: "off-cluster"}},
		},
	})

	require.Len(t, vs, 1)
	assert.True(t, vs[0].Fatal)
	assert.Contains(t, vs[0].Message, "off-cluster")
}
