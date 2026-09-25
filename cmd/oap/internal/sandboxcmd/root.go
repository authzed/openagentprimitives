// Package sandboxcmd implements `oap sandbox`: inspecting the SpiceboxSession
// resources that back an agent's running sandbox pods.
package sandboxcmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

func NewCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sandbox",
		Short: "Inspect SpiceboxSession resources (per-bundle running sandbox pods)",
	}
	cmd.AddCommand(newSandboxListCmd(g))
	cmd.AddCommand(newSandboxShowCmd(g))
	return cmd
}
