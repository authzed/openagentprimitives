package channelcmd

import (
	"fmt"

	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/uihelpers"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// channelKeyWidth caps the CHANNELKEY cell. A key is transport-supplied and can
// be arbitrarily long; ansi.Truncate spends the budget in display columns, the
// same measure the table sizes its columns in.
const channelKeyWidth = 50

func newChannelShowCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show Channel spec, status, and recent sessions.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			var ch spiceboxv1alpha1.Channel
			if err := b.Controller.Get(cmd.Context(), client.ObjectKey{Namespace: b.Namespace, Name: args[0]}, &ch); err != nil {
				return fmt.Errorf("get channel: %w", err)
			}
			out := cmd.OutOrStdout()

			fmt.Fprintf(out, "Name:        %s\n", ch.Name)
			fmt.Fprintf(out, "Namespace:   %s\n", ch.Namespace)
			fmt.Fprintf(out, "Kind:        %s\n", ch.Spec.Kind)
			fmt.Fprintf(out, "AgentClass:  %s\n", ch.Spec.AgentClass)
			if ch.Spec.AgentIdentity != "" {
				fmt.Fprintf(out, "Identity:    %s\n", ch.Spec.AgentIdentity)
			}
			fmt.Fprintf(out, "Scope:       %s\n", ch.Spec.SessionScope)
			fmt.Fprintf(out, "Secret:      %s\n", ch.Spec.CredentialsRef.SecretName)
			fmt.Fprintln(out)

			fmt.Fprintln(out, "Conditions:")
			uihelpers.PrintConditions(out, ch.Status.Conditions, "  ")
			fmt.Fprintln(out)

			// The sessions list is a supplementary section, so a failure here
			// warns and lets the Channel the user actually asked for still
			// print. It must not pass silently: "no recent sessions" and "we
			// could not find out" look identical on the screen, and only one
			// of them means the channel is idle.
			var sessions spiceboxv1alpha1.AgentSessionList
			listErr := b.Controller.List(cmd.Context(), &sessions,
				client.InNamespace(b.Namespace),
				client.MatchingLabels{spiceboxv1alpha1.LabelChannelName: ch.Name},
			)
			switch {
			case listErr != nil:
				cliout.Warn(out, "could not list recent sessions for this channel: %v", listErr)
			case len(sessions.Items) > 0:
				fmt.Fprintln(out, "Recent sessions:")
				t := g.Table(out, "  NAME", "PHASE", "CHANNELKEY", "AGE")
				for _, s := range sessions.Items {
					key := ""
					if s.Spec.InputChannel != nil {
						key = s.Spec.InputChannel.Key
					}
					t.Row("  "+s.Name, s.Status.Phase, ansi.Truncate(key, channelKeyWidth, "…"),
						apcmd.DurationSinceShort(s.CreationTimestamp.Time))
				}
				fmt.Fprint(out, t.Render())
			}
			return nil
		},
	}
	return cmd
}
