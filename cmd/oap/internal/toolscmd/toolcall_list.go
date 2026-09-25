package toolscmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newToolcallListCmd(g *apcmd.Globals) *cobra.Command {
	var sessionFilter string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List ToolCall CRs in the namespace",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			var list spiceboxv1alpha1.ToolCallList
			if err := b.Controller.List(cmd.Context(), &list); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			// Capabilities come from the stream this command writes to, so a
			// pipe, --no-color and NO_COLOR each land on the colorless theme.
			t := g.Table(out,
				"NAME", "SESSION", "TOOL", "PHASE", "EXIT", "AGE")
			rows := 0
			for _, tc := range list.Items {
				if sessionFilter != "" && tc.Spec.Session != sessionFilter {
					continue
				}
				phase := apcmd.ToolCallPhase(&tc)
				exit := "-"
				if tc.Status.ExitCode != nil {
					exit = fmt.Sprintf("%d", *tc.Status.ExitCode)
				}
				t.Row(tc.Name, tc.Spec.Session, tc.Spec.Tool, phase, exit,
					apcmd.DurationSinceShort(tc.CreationTimestamp.Time))
				rows++
			}
			if rows == 0 {
				if sessionFilter != "" {
					fmt.Fprintf(out, "no ToolCalls in %s for session %q\n", b.Namespace, sessionFilter)
				} else {
					fmt.Fprintf(out, "no ToolCalls in %s\n", b.Namespace)
				}
				return nil
			}
			fmt.Fprint(out, t.Render())
			return nil
		},
	}
	cmd.Flags().StringVar(&sessionFilter, "session", "",
		"Only show ToolCalls targeting this SpiceboxSession name")
	return cmd
}
