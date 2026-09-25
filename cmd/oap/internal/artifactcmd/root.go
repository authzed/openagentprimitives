// Package artifactcmd implements `oap artifact`: inspecting versioned
// artifacts, their revision trees, and the raw artifactstore behind them.
package artifactcmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// NewCmd builds the `oap artifact` subtree.
func NewCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "artifact",
		Short: "Inspect versioned artifacts and their revisions",
	}
	// Versioned, session-scoped commands (primary surface).
	cmd.AddCommand(newArtifactVersionsListCmd(g))
	cmd.AddCommand(newArtifactRevisionsCmd(g))
	cmd.AddCommand(newArtifactGetVersionedCmd(g))
	// Low-level artifactstore inspection (debug-token).
	store := &cobra.Command{Use: "store", Short: "Low-level artifactstore inspection (debug)"}
	store.AddCommand(newArtifactGetCmd(g))
	store.AddCommand(newArtifactListCmd(g))
	cmd.AddCommand(store)
	return cmd
}
