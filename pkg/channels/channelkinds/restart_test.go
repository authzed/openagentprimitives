package channelkinds_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// fakeRestartCapableKind implements just the RestartCapable interface.
type fakeRestartCapableKind struct {
	registered bool
	last       channelkinds.RestartSubmitFunc
}

func (f *fakeRestartCapableKind) RegisterRestartTrigger(submit channelkinds.RestartSubmitFunc) {
	f.registered = true
	f.last = submit
}

func TestRestartCapable_TypeAssertionFlow(t *testing.T) {
	k := &fakeRestartCapableKind{}

	// The channelsd start-up shape: type-assert and wire.
	var submit channelkinds.RestartSubmitFunc = func(_ context.Context, _ channelkinds.RestartTrigger) error {
		return nil
	}
	if rk, ok := interface{}(k).(channelkinds.RestartCapable); ok {
		rk.RegisterRestartTrigger(submit)
	}
	assert.True(t, k.registered)
	require.NotNil(t, k.last)
}

func TestRestartTrigger_Shape(t *testing.T) {
	tr := channelkinds.RestartTrigger{
		SessionRef:   types.NamespacedName{Namespace: "ns", Name: "sess"},
		CutTurnIndex: 5,
		NewUserText:  "edited",
		TriggeredBy: channelevents.ExternalIdentity{
			Kind:       "slack",
			ExternalID: "U1",
		},
		KindRequestRef: "opaque",
	}
	assert.Equal(t, "ns", tr.SessionRef.Namespace)
	assert.Equal(t, int32(5), tr.CutTurnIndex)
}
