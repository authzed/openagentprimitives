// Package pincmd implements `oap pin`: inspecting and updating the dependency
// pin baselines for MCPServers, SidecarToolboxes, Skills and SpiceboxToolkits.
package pincmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// NewCmd is the `oap pin` group: inspect and update dependency pin baselines
// for MCPServers, SidecarToolboxes, Skills, and SpiceboxToolkits.
func NewCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pin",
		Short: "Inspect and update dependency pin baselines",
		Long: `Manage dependency pinning for MCPServers, SidecarToolboxes, Skills, and SpiceboxToolkits.

Pinning records the immutable identity (manifest hash, image digest, git SHA) of each
dependency an agent uses, detects drift, and gates tool calls based on the effective
pinning policy.

Subcommands:
  status   — list all pinned dependencies with strength, digest, drift, and age
  diff     — compare the recorded baseline against the live identity
  update   — set the pin-refreeze annotation to accept a new baseline`,
	}
	cmd.AddCommand(newPinStatusCmd(g))
	cmd.AddCommand(newPinDiffCmd(g))
	cmd.AddCommand(newPinUpdateCmd(g))
	return cmd
}
