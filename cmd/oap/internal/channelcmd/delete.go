package channelcmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newChannelDeleteCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a Channel CR (does not cascade to existing sessions).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			ch := &spiceboxv1alpha1.Channel{}
			ch.Name = args[0]
			ch.Namespace = b.Namespace
			if err := b.Controller.Delete(cmd.Context(), ch); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "deleted %s/%s\n", b.Namespace, args[0])
			fmt.Fprintln(out, "note: existing AgentSessions spawned by this Channel are NOT cascade-deleted.")
			return nil
		},
	}
	return cmd
}
