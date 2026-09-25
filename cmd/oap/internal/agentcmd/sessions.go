package agentcmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newAgentSessionsCmd(g *apcmd.Globals) *cobra.Command {
	var includeSubagents bool
	c := &cobra.Command{
		Use:   "sessions [<class-name>]",
		Short: "List AgentSession CRs, optionally filtered by class",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			var list spiceboxv1alpha1.AgentSessionList
			if err := b.Controller.List(cmd.Context(), &list, client.InNamespace(b.Namespace)); err != nil {
				return err
			}

			// Filter by class if a positional arg was provided. Allocate a
			// fresh slice rather than re-using items[:0]; the latter would
			// alias the backing array of list.Items, which is fragile if
			// callers later iterate the original slice.
			items := list.Items

			// A delegated child is machinery: one delegating turn can add a
			// session per call, burying the conversations a person actually
			// started. Hidden by DEFAULT and behind a flag rather than dropped,
			// because this is the command an operator reaches for when a
			// delegation misbehaves — and "where did my child session go" must
			// have an answer better than kubectl.
			if !includeSubagents {
				visible := make([]spiceboxv1alpha1.AgentSession, 0, len(items))
				for _, s := range items {
					if s.Spec.Parent == nil {
						visible = append(visible, s)
					}
				}
				items = visible
			}

			if len(args) == 1 {
				className := args[0]
				filtered := make([]spiceboxv1alpha1.AgentSession, 0, len(items))
				for _, s := range items {
					if s.Spec.Class == className {
						filtered = append(filtered, s)
					}
				}
				items = filtered
			}

			out := cmd.OutOrStdout()
			if len(items) == 0 {
				fmt.Fprintln(out, "no AgentSession CRs")
				return nil
			}
			// Capabilities come from the stream this command writes to, so a
			// pipe, --no-color and NO_COLOR each land on the colorless theme.
			t := g.Table(out,
				"NAME", "CLASS", "PHASE", "TURNS", "AGE")
			for _, s := range items {
				turns := "0"
				if s.Status.Progress != nil {
					turns = fmt.Sprintf("%d", s.Status.Progress.TurnCount)
				}
				phase := s.Status.Phase
				if phase == "" {
					phase = "Pending"
				}
				t.Row(s.Name, s.Spec.Class, phase, turns,
					apcmd.DurationSinceShort(s.CreationTimestamp.Time))
			}
			fmt.Fprint(out, t.Render())
			return nil
		},
	}
	c.Flags().BoolVar(&includeSubagents, "include-subagents", false,
		"Include delegated child sessions (those with a parent). Hidden by default: "+
			"one delegating turn can create a session per call.")
	return c
}
