// Package auditcmd implements `oap audit`: offline verification of a session's
// tamper-evident, append-only memory chains.
package auditcmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// NewCmd builds the `oap audit` subtree.
func NewCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Verify the tamper-evident audit log of a session",
	}
	cmd.AddCommand(newAuditVerifyCmd(g))
	return cmd
}
