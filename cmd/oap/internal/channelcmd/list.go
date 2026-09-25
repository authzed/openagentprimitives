package channelcmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newChannelListCmd(g *apcmd.Globals) *cobra.Command {
	allNamespaces := false
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Channels.",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			var list spiceboxv1alpha1.ChannelList
			opts := []client.ListOption{}
			if !allNamespaces {
				opts = append(opts, client.InNamespace(b.Namespace))
			}
			if err := b.Controller.List(cmd.Context(), &list, opts...); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(list.Items) == 0 {
				fmt.Fprintln(out, "no Channel CRs")
				return nil
			}
			t := g.Table(out, "NAMESPACE", "NAME", "KIND", "AGENTCLASS", "SCOPE", "CONNECTED", "AGE")
			for _, ch := range list.Items {
				connected := ""
				for _, c := range ch.Status.Conditions {
					if c.Type == spiceboxv1alpha1.ChannelConditionConnected {
						connected = string(c.Status)
						break
					}
				}
				if connected == "" {
					// Monitoring channels are output-only event sinks: they
					// bind no inbound Listener, so the Connected condition is
					// never set. Show "monitoring" rather than a misleading
					// "Unknown" (which reads as a fault).
					if ch.Spec.Role == spiceboxv1alpha1.ChannelRoleMonitoring {
						connected = "monitoring"
					} else {
						connected = "Unknown"
					}
				}
				t.Row(ch.Namespace, ch.Name, ch.Spec.Kind, ch.Spec.AgentClass,
					ch.Spec.SessionScope, connected,
					apcmd.DurationSinceShort(ch.CreationTimestamp.Time))
			}
			fmt.Fprint(out, t.Render())
			return nil
		},
	}
	apcmd.AllNamespacesFlag(cmd, &allNamespaces)
	return cmd
}
