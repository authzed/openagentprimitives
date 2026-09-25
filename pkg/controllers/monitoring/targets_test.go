package monitoring

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestTargets_Shape checks structural well-formedness of every row (New,
// GetConditions, and at least one Rule are set) and that a fixed subset of
// known GVKs is present. It deliberately does NOT assert that Targets()
// covers every CRD with a Valid (or equivalent) condition: doing so would
// need an independent enumeration of "CRDs with a health condition" to
// diff against, and the only mechanical proxy available — grepping
// pkg/apis/v1alpha1 for a `*ConditionValid = "Valid"` constant — already
// matches several CRDs Targets() does not monitor today (e.g.
// ClusterIdentityProvider, UserIdentity, Skill, SpiceboxToolchain,
// SpiceboxToolspec, SpiceboxToolkit, SpiceDBBootstrap, WorkspaceSource,
// and SidecarToolbox's own Valid condition — only its PinDrift condition
// is watched). Turning that proxy into a completeness assertion would
// either fail immediately on unrelated, pre-existing gaps this task did
// not introduce, or require a second hand-maintained "these are
// intentionally excluded" list — which just relocates the exact problem
// (a silently-stale hand-maintained list) this test is trying to solve.
// See Targets()'s doc comment: the discipline is procedural (add a row in
// the same change that adds the condition), not enforced by this test.
func TestTargets_Shape(t *testing.T) {
	targets := Targets()
	names := map[string]Target{}
	for _, tg := range targets {
		names[tg.GVKName] = tg
		require.NotNil(t, tg.New, "%s: New must be set", tg.GVKName)
		require.NotNil(t, tg.GetConditions, "%s: GetConditions must be set", tg.GVKName)
		require.NotEmpty(t, tg.Rules, "%s: must have at least one rule", tg.GVKName)
	}
	for _, want := range []string{"AgentIdentity", "AgentSession", "AgentClass", "AgentUI", "MCPServer", "Channel", "RelationshipSource"} {
		assert.Contains(t, names, want)
	}
}

func TestTargets_GetConditionsExtractsStatus(t *testing.T) {
	var aiTarget Target
	for _, tg := range Targets() {
		if tg.GVKName == "AgentIdentity" {
			aiTarget = tg
		}
	}
	require.NotEmpty(t, aiTarget.GVKName, "AgentIdentity target must exist")

	ai := &spiceboxv1alpha1.AgentIdentity{}
	ai.Status.Conditions = []metav1.Condition{
		{Type: "Refresh", Status: metav1.ConditionFalse, Reason: "TokenEndpointError"},
	}
	conds := aiTarget.GetConditions(ai)
	require.Len(t, conds, 1)
	assert.Equal(t, "Refresh", conds[0].Type)
}
