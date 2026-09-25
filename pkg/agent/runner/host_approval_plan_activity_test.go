package runner

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestApprovalPauseCause_MapsKind(t *testing.T) {
	require.Equal(t, channelevents.PauseCauseApproval, approvalPauseCause("tool_call"))
	require.Equal(t, channelevents.PauseCauseLeakageApproval, approvalPauseCause("leakage_share"))
	require.Equal(t, channelevents.PauseCauseApproval, approvalPauseCause("something_else"),
		"unknown approval kinds fall back to the generic approval cause")
}
