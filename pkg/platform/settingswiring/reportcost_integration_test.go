//go:build integration

// pkg/platform/settingswiring/reportcost_integration_test.go
package settingswiring

import (
	"context"
	"os"
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMain(m *testing.M) { os.Exit(testenv.RunPackage(m)) }

func boolPtr(b bool) *bool { return &b }

// newCostClass builds a minimal AgentClass valid enough that ResolveForSession
// (ForSession=true, a missing model is fatal) does not itself produce a fatal
// violation unrelated to reportSessionCost — the test only cares about
// eff.ReportSessionCost.
func newCostClass(namespace, name string) *v1.AgentClass {
	return &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: v1.AgentClassSpec{
			Model: &v1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-opus-4-8",
				APIKey:   v1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
			SystemPrompt: v1.PromptSource{Inline: "you are an agent"},
			Budget:       &v1.BudgetConfig{MaxTurns: 10, MaxTokens: 50000},
		},
	}
}

// TestReportSessionCostResolution_ClusterDefaultFalse pins that a cluster
// defaults.reportSessionCost=false reaches EffectiveSettings.ReportSessionCost
// through ResolveForSession's live tier fetch.
func TestReportSessionCostResolution_ClusterDefaultFalse(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	cas := &v1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterAgentSettingsName},
		Spec: v1.SettingsSpec{
			Defaults: &v1.SettingsDefaults{
				ReportSessionCost: boolPtr(false),
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, cas), "create ClusterAgentSettings")
	t.Cleanup(func() { _ = env.Client.Delete(ctx, cas) })

	class := newCostClass("default", "rc-cluster-false")
	require.NoError(t, env.Client.Create(ctx, class), "create AgentClass")

	sess := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "rc-cluster-false-sess"},
		Spec: v1.AgentSessionSpec{
			Class:  class.Name,
			Prompt: v1.PromptSource{Inline: "hello"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	eff, _, err := ResolveForSession(ctx, env.Client, class, sess)
	require.NoError(t, err, "ResolveForSession")
	assert.False(t, eff.ReportSessionCost, "cluster defaults.reportSessionCost=false must resolve to false")
}

// TestReportSessionCostResolution_NoClusterCR pins the default-on behaviour when
// no ClusterAgentSettings exists — the missing-tier case FetchTiers treats as
// NotFound-is-not-an-error.
func TestReportSessionCostResolution_NoClusterCR(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	// Belt-and-suspenders: confirm no ClusterAgentSettings singleton lingers
	// from a prior test in this shared envtest.
	_ = env.Client.Delete(ctx, &v1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterAgentSettingsName},
	})

	class := newCostClass("default", "rc-no-cluster")
	require.NoError(t, env.Client.Create(ctx, class), "create AgentClass")

	sess := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "rc-no-cluster-sess"},
		Spec: v1.AgentSessionSpec{
			Class:  class.Name,
			Prompt: v1.PromptSource{Inline: "hello"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	eff, _, err := ResolveForSession(ctx, env.Client, class, sess)
	require.NoError(t, err, "ResolveForSession")
	assert.True(t, eff.ReportSessionCost, "absent ClusterAgentSettings must resolve reportSessionCost to true")
}
