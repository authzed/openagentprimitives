package builtins_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
)

type stubFlow struct{ name string }

func (s stubFlow) Name() string { return s.name }
func (stubFlow) Screens(context.Context, builtins.Request) ([]tui.Screen, error) {
	return nil, errors.New("stubFlow: asks nothing; it exists to be registered and looked up")
}
func (stubFlow) Result(context.Context, builtins.Request, *tui.State) error {
	return errors.New("stubFlow: stores nothing; it exists to be registered and looked up")
}
func (stubFlow) Verify(context.Context, builtins.VerifyRequest) (builtins.VerifyResult, error) {
	return builtins.VerifyResult{Status: builtins.VerifyIndeterminate, Detail: "stubFlow: verification not implemented"}, nil
}

func TestRegisterAndGet(t *testing.T) {
	builtins.Reset()
	builtins.Register(stubFlow{name: "x"})
	_, ok := builtins.Get("x")
	assert.True(t, ok, "Get(x) should hit")
	_, ok = builtins.Get("y")
	assert.False(t, ok, "Get(y) should miss")
}

func TestRegisterDupePanics(t *testing.T) {
	builtins.Reset()
	builtins.Register(stubFlow{name: "x"})
	require.Panics(t, func() {
		builtins.Register(stubFlow{name: "x"})
	}, "expected panic on duplicate")
}
