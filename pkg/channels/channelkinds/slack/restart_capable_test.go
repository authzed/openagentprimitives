package slack_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

func TestSlackKind_ImplementsRestartCapable(t *testing.T) {
	k := &slack.Kind{}
	rk, ok := interface{}(k).(channelkinds.RestartCapable)
	assert.True(t, ok, "slackKind must implement RestartCapable")
	if ok {
		called := false
		rk.RegisterRestartTrigger(func(_ context.Context, _ channelkinds.RestartTrigger) error {
			called = true
			return nil
		})
		_ = called
	}
}
