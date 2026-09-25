package kgcmd

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/memclient"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// summaryWidth caps the SUMMARY cell. Summaries are model-written prose, so the
// cut is spent in display columns rather than bytes: slicing a byte offset out
// of an em dash or a smart quote emits replacement characters into the table.
const summaryWidth = 40

func newKGCommunitiesCmd(g *apcmd.Globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "communities <session>",
		Short: "List detected knowledge graph communities",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			conn, err := memclient.Connect(ctx, b, args[0], "")
			if err != nil {
				return err
			}
			defer conn.Close()

			comms, err := conn.Client.KGCommunities(ctx, conn.Scope)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]interface{}{"communities": comms})
			}
			out := cmd.OutOrStdout()
			return renderCommunities(out, g.Theme(out), comms)
		},
	}
	apcmd.JSONFlag(cmd, &asJSON, "Output as JSON")
	return cmd
}

func renderCommunities(out io.Writer, th *tui.Theme, comms []memory.KGCommunity) error {
	if len(comms) == 0 {
		fmt.Fprintln(out, "No communities found.")
		return nil
	}
	t := tui.NewTable(th, "UUID", "NAME", "MEMBERS", "SUMMARY")
	for _, c := range comms {
		t.Row(c.UUID, c.Name, fmt.Sprintf("%d", len(c.Members)),
			ansi.Truncate(c.Summary, summaryWidth, "..."))
	}
	_, err := fmt.Fprint(out, t.Render())
	return err
}
