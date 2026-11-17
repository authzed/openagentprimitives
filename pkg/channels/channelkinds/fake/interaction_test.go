package fake

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// A refused click's notice reaches the interaction sender like the other
// interaction envelopes; it is recorded for tests, not reported as an
// unsupported kind (which the outbound relay logs on every refused click).
func TestInteractionSenderRecordsDecisionRejections(t *testing.T) {
	ch := &spiceboxv1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ch-rejections"}}
	drv := driverFor(ch)
	pl := channelevents.InteractionDecisionRejectedPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: "ns", Name: "s"},
		Category:        "plan_phase", RequestRef: "req-1", Class: "not_authorized", Reason: "you are not authorized to decide this request",
		Clicker: channelevents.ExternalIdentity{Kind: "fake", Email: "member@example.com"},
	}
	env, err := channelevents.BuildEnvelope("ns", "s", channelevents.KindInteractionDecisionRejected, pl)
	require.NoError(t, err)

	res, err := newInteractionSender(drv).Send(context.Background(), channelkinds.SessionInfo{Namespace: "ns", Name: "s"}, env)
	require.NoError(t, err)
	assert.Equal(t, "req-1", res.RequestRef)
	got := drv.InteractionRejections()
	require.Len(t, got, 1)
	assert.Equal(t, pl, got[0].Payload)
	assert.Equal(t, "s", got[0].SessionRef.Name)
}
