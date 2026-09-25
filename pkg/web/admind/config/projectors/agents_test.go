package projectors

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func agentClass(name, ns string, mutate func(*spiceboxv1alpha1.AgentClass)) *spiceboxv1alpha1.AgentClass {
	ac := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if mutate != nil {
		mutate(ac)
	}
	return ac
}

func TestAgentsProjector(t *testing.T) {
	valid := agentClass("alpha", "ns1", func(ac *spiceboxv1alpha1.AgentClass) {
		ac.Spec.IdentityMode = spiceboxv1alpha1.IdentityModeAgent
		ac.Spec.AgentIdentity = "gh-bot"
		ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "a", Ref: "a"}, {Name: "b", Ref: "b"}}
		ac.Spec.Skills = []spiceboxv1alpha1.AgentSkill{{Name: "s", Ref: "github.com/o/r//s@v1"}}
		ac.Status.Conditions = []metav1.Condition{cond(spiceboxv1alpha1.AgentClassConditionValid, metav1.ConditionTrue, "Valid")}
		ac.Status.OapInstall = &spiceboxv1alpha1.OapInstallStatus{
			SourceRef: "ghcr.io/example/demo-agent:1", Digest: "sha256:abc", Version: "1.2.0", SourceKind: "registry",
		}
	})
	degraded := agentClass("bravo", "ns1", func(ac *spiceboxv1alpha1.AgentClass) {
		ac.Status.Conditions = []metav1.Condition{cond(spiceboxv1alpha1.AgentClassConditionValid, metav1.ConditionFalse, "ToolspecMissing")}
	})
	unstamped := agentClass("charlie", "ns2", nil) // no conditions

	c := newClient(t, valid, degraded, unstamped)
	rows, err := agentsProjector{}.List(context.Background(), c)
	require.NoError(t, err)
	require.Len(t, rows, 3)

	a := rowByName(t, rows, "alpha", "")
	assert.Equal(t, "namespaced", a.Scope)
	assert.Equal(t, "ns1", a.Namespace)
	assert.Equal(t, "Valid", a.Status)
	assert.Equal(t, spiceboxv1alpha1.IdentityModeAgent, badgeVal(a, "identityMode"))
	assert.Equal(t, "gh-bot", badgeVal(a, "identityRef"), "identityRef badge = spec.agentIdentity")
	assert.Equal(t, 2, countVal(a, "tools"))
	assert.Equal(t, 1, countVal(a, "skills"))
	assert.Equal(t, "kubectl edit agentclass alpha -n ns1", a.ManageCmd)
	assert.Equal(t, "1.2.0", badgeVal(a, "oap"), "oap badge = status.oapInstall.version")

	b := rowByName(t, rows, "bravo", "")
	assert.Equal(t, "Degraded", b.Status)
	assert.Equal(t, "ToolspecMissing", b.StatusReason)
	assert.Empty(t, badgeVal(b, "identityRef"), "no spec.agentIdentity → no identityRef badge")
	assert.Empty(t, badgeVal(b, "oap"), "no status.oapInstall → no oap badge")

	// No Valid condition stamped → Unknown, never implied healthy.
	ch := rowByName(t, rows, "charlie", "")
	assert.Equal(t, "Unknown", ch.Status)
}
