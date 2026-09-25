package channelcmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newChannelDiagCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "test <name>",
		Short: "Diagnostic: report Channel status (Connected, Valid).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			var ch spiceboxv1alpha1.Channel
			if err := b.Controller.Get(cmd.Context(), client.ObjectKey{Namespace: b.Namespace, Name: args[0]}, &ch); err != nil {
				return fmt.Errorf("get: %w", err)
			}
			out := cmd.OutOrStdout()
			for _, c := range ch.Status.Conditions {
				fmt.Fprintf(out, "%s=%s reason=%q\n", c.Type, c.Status, c.Reason)
			}
			if ch.Status.ResolvedAgentClassUID != "" {
				fmt.Fprintf(out, "ResolvedAgentClassUID=%s\n", ch.Status.ResolvedAgentClassUID)
			}
			return nil
		},
	}
	return cmd
}
