package sandboxcmd

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newSandboxListCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List SpiceboxSession CRs in the namespace",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			var list spiceboxv1alpha1.SpiceboxSessionList
			if err := b.Controller.List(cmd.Context(), &list); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(list.Items) == 0 {
				fmt.Fprintf(out, "no SpiceboxSessions in %s\n", b.Namespace)
				return nil
			}
			// Capabilities come from the stream this command writes to, so a
			// pipe, --no-color and NO_COLOR each land on the colorless theme.
			t := g.Table(out,
				"NAME", "READY", "CLASS", "AGENT", "SANDBOX", "CALLS", "AGE")
			for _, s := range list.Items {
				ready := "Unknown"
				for _, c := range s.Status.Conditions {
					if c.Type == "Ready" {
						ready = string(c.Status)
					}
				}
				agent := s.Spec.Agent
				if agent == "" {
					agent = "-"
				}
				t.Row(s.Name, ready, s.Spec.Class, agent, sandboxCell(s),
					fmt.Sprintf("%d", s.Status.CallCount),
					apcmd.DurationSinceShort(s.CreationTimestamp.Time))
			}
			fmt.Fprint(out, t.Render())
			return nil
		},
	}
}

// maxSandboxRefCell bounds the SANDBOX column's ref segment, in display
// columns, so one long ref (the pod backend's "<namespace>/<podName>",
// potentially longer for other backends) does not blow out the table's width in
// a terminal; sandbox show (a flat, non-tabular view) prints the ref in full
// instead.
const maxSandboxRefCell = 28

// sandboxCell renders the SpiceboxSession's sandbox backend for the list
// table: "<kind>:<ref>" from status.sandbox, with " (warm)" appended when the
// sandbox was adopted from a pre-warmed pool rather than created cold —
// prewarmed=false is not shown, since a backend that cannot pre-warm at all
// would otherwise always display a misleadingly alarming "false".
//
// status.sandbox is unset only for a SpiceboxSession created before this seam
// existed; PodName is kept as its fallback so those sessions still show
// something instead of a blank column.
func sandboxCell(s spiceboxv1alpha1.SpiceboxSession) string {
	if h := s.Status.Sandbox; h != nil {
		cell := h.Kind + ":" + truncateSandboxRef(h.Ref, maxSandboxRefCell)
		if h.Prewarmed {
			cell += " (warm)"
		}
		return cell
	}
	if s.Status.PodName != "" {
		return s.Status.PodName
	}
	return "-"
}

// truncateSandboxRef caps s at max *display columns*, ellipsis included.
//
// A ref is backend-supplied and need not be ASCII, so the budget is spent in
// display columns and the cut lands on a rune boundary: lipgloss.Width is the
// only measure the table itself uses, and cutting on a byte offset would split
// a multi-byte rune and emit replacement characters into the cell. Wide runes
// count two, so a CJK ref is cut sooner in runes but lands on the same column
// budget the ASCII case does — which is what keeps every column to its right
// aligned.
func truncateSandboxRef(s string, max int) string {
	if lipgloss.Width(s) <= max {
		return s
	}
	const ellipsis = "…"
	budget := max - lipgloss.Width(ellipsis)
	var b strings.Builder
	used := 0
	for _, r := range s {
		w := lipgloss.Width(string(r))
		if used+w > budget {
			break
		}
		b.WriteRune(r)
		used += w
	}
	return b.String() + ellipsis
}
