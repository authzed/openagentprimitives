package slack

// crossagent_test.go pins the CLASSIFIER. This file pins where the classifier's
// second argument comes from, which is the half that decides whether the
// feature exists at all: the flag was declared, correct, and assigned by
// nothing, so cross-agent participation was permanently off and every
// classifier test still passed.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// crossAgentClass builds a class declaring (or not declaring) cross-agent
// participation.
func crossAgentClass(name string, cfg *spiceboxv1alpha1.CrossAgentThreadsConfig) *spiceboxv1alpha1.AgentClass {
	c := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
	}
	if cfg != nil {
		c.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{CrossAgentThreads: cfg}
	}
	return c
}

// newCrossAgentListener builds a listener bound to boundClass, with objs in the
// API server.
func newCrossAgentListener(t *testing.T, boundClass string, objs ...runtime.Object) (*slackListener, *spiceboxv1alpha1.Channel) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-ch", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: KindName, AgentClass: boundClass},
	}
	b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ch)
	for _, o := range objs {
		b = b.WithObjects(o.(interface {
			runtime.Object
			metav1.Object
		}))
	}
	l := &slackListener{deps: channelkinds.Deps{Channel: ch, K8sClient: b.Build()}}
	return l, ch
}

// TestTheBoundClassTurnsCrossAgentOn is the wiring itself. activeFeatures is
// the tick's existing class read; cross-agent admission rides it, so one GET a
// minute answers both questions and the flag can never describe a different
// class than the budget.
func TestTheBoundClassTurnsCrossAgentOn(t *testing.T) {
	l, ch := newCrossAgentListener(t, "codebot",
		crossAgentClass("codebot", &spiceboxv1alpha1.CrossAgentThreadsConfig{Enabled: true, WakeBudget: 4}))

	enabled, budget := l.crossAgentConfig()
	assert.False(t, enabled, "before any class read the answer must be the closed one")
	assert.Equal(t, 0, budget)

	l.activeFeatures(context.Background(), ch)

	enabled, budget = l.crossAgentConfig()
	assert.True(t, enabled, "the class declares cross-agent participation, so the listener must admit another agent")
	assert.Equal(t, 4, budget, "and carry the class's budget, not a hardcoded one")
	assert.True(t, admitsInbound(originOtherAgent, enabled),
		"which is the whole point: the classifier's answer changes because the class says so")
}

// TestAClassThatDeclaresNothingAdmitsNoAgent — the default direction. A class
// with no authz block at all must not admit, and must not grant a budget.
func TestAClassThatDeclaresNothingAdmitsNoAgent(t *testing.T) {
	l, ch := newCrossAgentListener(t, "plainbot", crossAgentClass("plainbot", nil))

	l.activeFeatures(context.Background(), ch)

	enabled, budget := l.crossAgentConfig()
	assert.False(t, enabled)
	assert.Equal(t, 0, budget)
	assert.False(t, admitsInbound(originOtherAgent, enabled))
}

// TestTurningItOffTakesEffectWithoutARestart.
//
// A running listener is never restarted for a spec edit, so if the answer were
// resolved once at Start it would be frozen for the process's lifetime — an
// operator revoking cross-agent participation during an incident would watch
// it keep working. The tick re-reads, so the revocation lands.
func TestTurningItOffTakesEffectWithoutARestart(t *testing.T) {
	class := crossAgentClass("codebot", &spiceboxv1alpha1.CrossAgentThreadsConfig{Enabled: true, WakeBudget: 4})
	l, ch := newCrossAgentListener(t, "codebot", class)

	l.activeFeatures(context.Background(), ch)
	enabled, _ := l.crossAgentConfig()
	require.True(t, enabled, "precondition: it is on")

	// The operator turns it off. Read back and rewrite through the same client
	// the listener uses.
	var live spiceboxv1alpha1.AgentClass
	require.NoError(t, l.deps.K8sClient.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "codebot"}, &live))
	live.Spec.Authz.CrossAgentThreads.Enabled = false
	require.NoError(t, l.deps.K8sClient.Update(context.Background(), &live))

	// Defeat the read TTL the way a real minute would.
	l.scopesMu.Lock()
	l.scopeFeatures = nil
	l.scopesMu.Unlock()

	l.activeFeatures(context.Background(), ch)

	enabled, _ = l.crossAgentConfig()
	assert.False(t, enabled,
		"revoking cross-agent participation must take effect on the tick — a listener is not restarted for a spec edit")
}

// TestADeletedClassStopsAdmitting is the fail direction that differs from the
// scope half deliberately.
//
// When the bound class cannot be found, the scope half falls back to the
// BASELINE — failing safe there means requiring less, because flagging every
// optional scope as missing would train operators to ignore the condition. The
// admission half fails safe by CLOSING, because here the risk is admitting.
// The two point opposite ways off the same read, which is why each is stated
// at its own branch rather than inherited.
func TestADeletedClassStopsAdmitting(t *testing.T) {
	class := crossAgentClass("codebot", &spiceboxv1alpha1.CrossAgentThreadsConfig{Enabled: true, WakeBudget: 4})
	l, ch := newCrossAgentListener(t, "codebot", class)

	l.activeFeatures(context.Background(), ch)
	enabled, _ := l.crossAgentConfig()
	require.True(t, enabled, "precondition: it is on")

	require.NoError(t, l.deps.K8sClient.Delete(context.Background(), class))
	l.scopesMu.Lock()
	l.scopeFeatures = nil
	l.scopesMu.Unlock()

	l.activeFeatures(context.Background(), ch)

	enabled, budget := l.crossAgentConfig()
	assert.False(t, enabled,
		"a deleted class must stop admitting other agents, even though the scope half falls back to the baseline on the same branch")
	assert.Equal(t, 0, budget)
}

// TestUnbindingTheChannelStopsAdmitting: rebinding a Channel away from a
// cross-agent class — to none at all — must lose the admission. Left at the
// zero value it would silently keep the old class's answer, because that
// branch does no read.
func TestUnbindingTheChannelStopsAdmitting(t *testing.T) {
	class := crossAgentClass("codebot", &spiceboxv1alpha1.CrossAgentThreadsConfig{Enabled: true, WakeBudget: 4})
	l, ch := newCrossAgentListener(t, "codebot", class)

	l.activeFeatures(context.Background(), ch)
	enabled, _ := l.crossAgentConfig()
	require.True(t, enabled, "precondition: it is on")

	// The tick hands over the Channel AS OF THIS TICK — now bound to nothing.
	unbound := ch.DeepCopy()
	unbound.Spec.AgentClass = ""
	l.activeFeatures(context.Background(), unbound)

	enabled, budget := l.crossAgentConfig()
	assert.False(t, enabled, "a Channel bound to no class declares nothing, so it admits nothing")
	assert.Equal(t, 0, budget)
}
