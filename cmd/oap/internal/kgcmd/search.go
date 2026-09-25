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

// factWidth caps the FACT cell, in display columns for the same reason
// summaryWidth is: a fact is model-written prose, not ASCII.
const factWidth = 60

func newKGSearchCmd(g *apcmd.Globals) *cobra.Command {
	var (
		text   string
		limit  int
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "search <session>",
		Short: "Search knowledge graph for facts",
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

			// The KG surface answers with a bare slice, so the probe that
			// separates "20 matched" from "20 shown of many" is made here:
			// ask for one fact past the limit and trim it back off.
			facts, err := conn.Client.KGSearchFacts(ctx, conn.Scope, text, memory.ProbeLimit(limit))
			if err != nil {
				return err
			}
			facts, truncated := memory.TrimProbe(facts, limit)
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(kgFactsPayload(facts, truncated))
			}
			out := cmd.OutOrStdout()
			return renderFacts(out, g.Theme(out), facts, truncated, limit)
		},
	}
	cmd.Flags().StringVar(&text, "text", "", "Natural language search query")
	apcmd.ResultLimitFlag(cmd, &limit)
	apcmd.JSONFlag(cmd, &asJSON, "Output as JSON")
	return cmd
}

// kgFactsPayload is the --json document the fact-listing commands emit. It
// carries the truncation fact alongside the facts themselves: a notice that
// only reaches the rendered table leaves a script with the same silent partial
// the human was spared.
func kgFactsPayload(facts []memory.KGFact, truncated bool) map[string]any {
	return map[string]any{"facts": facts, "truncated": truncated}
}

// kgEntitiesPayload is kgFactsPayload's counterpart for the entity listings.
func kgEntitiesPayload(entities []memory.KGEntity, truncated bool) map[string]any {
	return map[string]any{"entities": entities, "truncated": truncated}
}

// renderFacts prints the fact table. truncated/limit come from the caller's
// probe; a listing with no --limit of its own passes false and 0.
func renderFacts(out io.Writer, th *tui.Theme, facts []memory.KGFact, truncated bool, limit int) error {
	if truncated {
		if err := apcmd.TruncationNotice(out, "facts", limit); err != nil {
			return err
		}
	}
	if len(facts) == 0 {
		fmt.Fprintln(out, "No facts found.")
		return nil
	}
	t := tui.NewTable(th, "NAME", "FACT", "UUID")
	for _, f := range facts {
		t.Row(f.Name, ansi.Truncate(f.Fact, factWidth, "..."), f.UUID)
	}
	_, err := fmt.Fprint(out, t.Render())
	return err
}
