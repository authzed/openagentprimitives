package main

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// scopeRefreshingListener is a Listener that also implements
// channelkinds.ScopeRefresher, counting the refreshes it is asked for.
// plainListener (session_attached_dispatch_test.go) implements neither
// optional interface — the shape every other channel kind has today.
type scopeRefreshingListener struct {
	plainListener
	refreshes atomic.Int32
	sawSpec   atomic.Value // the spec.agentClass of the last Channel handed over
}

func (l *scopeRefreshingListener) RefreshScopes(_ context.Context, ch *spiceboxv1alpha1.Channel) {
	l.refreshes.Add(1)
	if ch != nil {
		l.sawSpec.Store(ch.Spec.AgentClass)
	}
}

// TestRefreshListenerScopesDispatchesOnlyToScopeRefreshers pins the optional
// interface: channelsd re-derives the scope condition on its existing reconcile
// tick for kinds that can answer, and leaves every other kind alone rather than
// branching on a kind name.
func TestRefreshListenerScopesDispatchesOnlyToScopeRefreshers(t *testing.T) {
	m := &channelManager{
		listeners: map[string]channelkinds.Listener{},
		cancels:   map[string]context.CancelFunc{},
	}
	refresher := &scopeRefreshingListener{}
	m.listeners["demo-ns/refreshing"] = refresher
	m.listeners["demo-ns/plain"] = plainListener{}

	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "refreshing"},
		Spec:       spiceboxv1alpha1.ChannelSpec{AgentClass: "demo-agent"},
	}
	m.refreshListenerScopes(context.Background(), "demo-ns/refreshing", ch)
	m.refreshListenerScopes(context.Background(), "demo-ns/refreshing", ch)
	assert.Equal(t, int32(2), refresher.refreshes.Load(),
		"every tick must re-ask a ScopeRefresher; the listener itself decides whether to write")

	// The Channel handed over must be THIS tick's object, not the listener's
	// start-time snapshot — that is the only way a rebind is ever seen.
	rebound := ch.DeepCopy()
	rebound.Spec.AgentClass = "other-agent"
	m.refreshListenerScopes(context.Background(), "demo-ns/refreshing", rebound)
	assert.Equal(t, "other-agent", refresher.sawSpec.Load(),
		"the freshly listed Channel must be passed through, not dropped")

	// A listener without the interface, and a Channel with no listener at all
	// (resolve failed, or client-hosted kind), must both be no-ops.
	require.NotPanics(t, func() {
		m.refreshListenerScopes(context.Background(), "demo-ns/plain", ch)
		m.refreshListenerScopes(context.Background(), "demo-ns/absent", ch)
	})
}
