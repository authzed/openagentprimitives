package settings

import (
	"testing"
	"time"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestToStatus(t *testing.T) {
	tg := &v1.EffectiveToolGuard{}
	routing := &v1.OpenRouterRouting{Models: []string{"a/b"}}
	e := EffectiveSettings{
		Model:           v1.ModelConfig{Provider: "openrouter", Name: "openrouter/auto"},
		ModelRouting:    routing,
		Budget:          v1.BudgetConfig{MaxTurns: 20, MaxTokens: 100, MaxDuration: metav1.Duration{Duration: time.Hour}},
		Authz:           EffectiveAuthz{ApprovalTimeout: 3 * time.Minute, InformationLeakageApprovalTTL: 5 * time.Minute, ScopeMaxLLMLatencyMs: 4000},
		AllowedToolkits: []string{"git"},
		AllowedMCP:      []v1.AllowedMCPServer{{Name: "github", Tools: []string{"*"}}},
		Provenance:      map[string]string{"model": "class"},
		ToolGuard:       tg,
	}
	got := e.ToStatus()
	assert.Equal(t, "openrouter/auto", got.Model.Name)
	assert.Equal(t, int32(20), got.Budget.MaxTurns)
	assert.Equal(t, 3*time.Minute, got.Authz.ApprovalTimeout.Duration)
	assert.Equal(t, int32(4000), got.Authz.ScopeMaxLLMLatencyMs)
	assert.Equal(t, []string{"git"}, got.AllowedToolkits)
	assert.Equal(t, "class", got.Provenance["model"])
	assert.Equal(t, tg, got.ToolGuard)
	assert.Same(t, routing, got.ModelRouting)
}

func TestToStatus_CarriesSandbox(t *testing.T) {
	e := EffectiveSettings{
		Sandbox:             map[string]v1.SandboxBackend{"demo-bundle": {Kind: "pod"}},
		AllowedSandboxKinds: []string{"pod"},
	}
	got := e.ToStatus()

	require.Contains(t, got.Sandbox, "demo-bundle")
	assert.Equal(t, "pod", got.Sandbox["demo-bundle"].Kind)
	assert.Equal(t, []string{"pod"}, got.AllowedSandboxKinds)
}
