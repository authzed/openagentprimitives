//go:build e2e

package policy_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestHarness_ToolCallController_BootsCleanly verifies that opting into
// the ToolCall controller wiring boots the harness without errors and
// exposes the three accessors (FakeExec, Registry, GatewayDialer) that
// scenario tests rely on. Full ToolCall lifecycle coverage lives in the
// scenario tests under test/e2e/scenarios/.
func TestHarness_ToolCallController_BootsCleanly(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		WithToolCallController: true,
	})
	require.NotNil(t, h.FakeExec(), "FakeExec accessor")
	require.NotNil(t, h.Registry(), "Registry accessor")
	require.NotNil(t, h.GatewayDialer(), "GatewayDialer accessor")
}
