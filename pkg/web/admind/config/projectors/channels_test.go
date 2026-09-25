package projectors

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestChannelsProjector(t *testing.T) {
	connected := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-a", Namespace: "ns1"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "slack", Role: "both", AgentClass: "alpha"},
		Status: spiceboxv1alpha1.ChannelStatus{
			Conditions: []metav1.Condition{cond(spiceboxv1alpha1.ChannelConditionConnected, metav1.ConditionTrue, "SocketAttached")},
		},
	}
	// Valid is stamped but Connected is absent → Unknown (not implied healthy).
	unconnected := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-b", Namespace: "ns1"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "slack"},
		Status: spiceboxv1alpha1.ChannelStatus{
			Conditions: []metav1.Condition{cond(spiceboxv1alpha1.ChannelConditionValid, metav1.ConditionTrue, "AllReferencesResolve")},
		},
	}

	c := newClient(t, connected, unconnected)
	rows, err := channelsProjector{}.List(context.Background(), c)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	a := rowByName(t, rows, "slack-a", "")
	assert.Equal(t, "namespaced", a.Scope)
	assert.Equal(t, "Connected", a.Status)
	assert.Equal(t, "slack", badgeVal(a, "kind"))
	assert.Equal(t, "alpha", badgeVal(a, "agentClass"))
	assert.Equal(t, "kubectl edit channel slack-a -n ns1", a.ManageCmd)

	b := rowByName(t, rows, "slack-b", "")
	assert.Equal(t, "Unknown", b.Status)
}
