package goalscmd

import (
	"testing"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/stretchr/testify/require"
)

func TestInvalidGoalCLIRequestRefusedBeforeConnecting(t *testing.T) {
	for _, args := range [][]string{
		{"create", "session", "--title", "Agenda", "--outcome", "Prepare"},
		{"update", "session", "goal-id", "--request-id", "retry"},
		{"create", "session", "--request-id", "retry", "--title", "Agenda", "--outcome", "Prepare", "--due-at", "tomorrow"},
	} {
		cmd := NewCmd(&apcmd.Globals{})
		cmd.SetArgs(args)
		err := cmd.Execute()
		require.Error(t, err)
		require.NotContains(t, err.Error(), "kubeconfig")
	}
}
