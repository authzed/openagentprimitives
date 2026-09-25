// Package plangatecmd implements `oap plangate`: reading back what the plan
// gate recorded for a session.
package plangatecmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// NewCmd builds the `oap plangate` subtree: reading back what the plan gate
// recorded for a session.
//
// This exists because the logging-mode dataset had no reader. plangate.Metrics
// computes the four numbers enforcement is gated on, and nothing in any binary
// called it — so "run in logging mode and look at the data" was not actually a
// thing anyone could do, and the enforcement decision had no evidence path.
func NewCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plangate",
		Short: "Inspect what the plan gate recorded for a session",
	}
	cmd.AddCommand(newPlanGateMetricsCmd(g))
	return cmd
}
