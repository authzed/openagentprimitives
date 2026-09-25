package agentcmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
)

// newAgentUninstallCmd removes every resource a matching `oap agent install`
// left on the cluster: every CR/Secret/ConfigMap carrying the install label
// <name> (see install.UninstallGraph for the exact Kind set and the
// adopted-vs-installed label distinction that keeps a shared cluster
// dependency from being reaped by an unrelated install's uninstall).
func newAgentUninstallCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uninstall <name>",
		Short: "Remove every resource a matching `oap agent install` created",
		Long: "Remove every resource a matching `oap agent install` created: every\n" +
			"CR/Secret/ConfigMap carrying the install label <name> (app.kubernetes.io/instance\n" +
			"and agentprimitives.authzed.com/oap-install). A cluster dependency the install\n" +
			"only ADOPTED (a pre-existing, pin-compatible shared SpiceboxToolkit/etc. another\n" +
			"install also depends on) never carries the install label and is left untouched.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			kb, err := g.Bundle()
			if err != nil {
				return err
			}

			deleted, err := install.UninstallGraph(cmd.Context(), kb.Controller, name, kb.Namespace)
			if err != nil {
				return fmt.Errorf("uninstall %s: %w", name, err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "%d resources deleted for install %s\n", deleted, name)
			return nil
		},
	}
	return cmd
}
