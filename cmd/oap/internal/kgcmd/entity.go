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

// entitySummaryWidth caps the SUMMARY cell of the entity table, in display
// columns for the same reason summaryWidth is.
const entitySummaryWidth = 50

func newKGEntityCmd(g *apcmd.Globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "entity <session> <uuid>",
		Short: "Get a knowledge graph entity by UUID",
		Args:  cobra.ExactArgs(2),
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

			ent, err := conn.Client.KGGetEntity(ctx, conn.Scope, args[1])
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(ent)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "UUID:    %s\nName:    %s\nSummary: %s\n", ent.UUID, ent.Name, ent.Summary)
			if len(ent.Attributes) > 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Attributes:")
				for k, v := range ent.Attributes {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s\n", k, v)
				}
			}
			return nil
		},
	}
	apcmd.JSONFlag(cmd, &asJSON, "Output as JSON")
	return cmd
}

func newKGFactsCmd(g *apcmd.Globals) *cobra.Command {
	var (
		entityUUID string
		asJSON     bool
	)
	cmd := &cobra.Command{
		Use:   "facts <session>",
		Short: "Get all facts about an entity",
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

			facts, err := conn.Client.KGEntityFacts(ctx, conn.Scope, entityUUID)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]interface{}{"facts": facts})
			}
			// This listing takes no --limit: every fact the graph holds for the
			// entity is returned, so there is no cut to report and no larger
			// limit to suggest.
			out := cmd.OutOrStdout()
			return renderFacts(out, g.Theme(out), facts, false, 0)
		},
	}
	cmd.Flags().StringVar(&entityUUID, "entity", "", "Entity UUID (required)")
	_ = cmd.MarkFlagRequired("entity")
	apcmd.JSONFlag(cmd, &asJSON, "Output as JSON")
	return cmd
}

func newKGRelatedCmd(g *apcmd.Globals) *cobra.Command {
	var (
		entityUUID string
		limit      int
		asJSON     bool
	)
	cmd := &cobra.Command{
		Use:   "related <session>",
		Short: "Find entities related to a given entity",
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

			// Probed one past the limit for the same reason kg search is: the
			// KG surface answers with a bare slice that cannot say whether the
			// limit or the graph ended the list.
			entities, err := conn.Client.KGRelatedEntities(ctx, conn.Scope, entityUUID, memory.ProbeLimit(limit))
			if err != nil {
				return err
			}
			entities, truncated := memory.TrimProbe(entities, limit)
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(kgEntitiesPayload(entities, truncated))
			}
			out := cmd.OutOrStdout()
			return renderEntities(out, g.Theme(out), entities, truncated, limit)
		},
	}
	cmd.Flags().StringVar(&entityUUID, "entity", "", "Entity UUID (required)")
	_ = cmd.MarkFlagRequired("entity")
	apcmd.ResultLimitFlag(cmd, &limit)
	apcmd.JSONFlag(cmd, &asJSON, "Output as JSON")
	return cmd
}

// renderEntities prints the entity table. truncated/limit come from the
// caller's probe; see renderFacts.
func renderEntities(out io.Writer, th *tui.Theme, entities []memory.KGEntity, truncated bool, limit int) error {
	if truncated {
		if err := apcmd.TruncationNotice(out, "entities", limit); err != nil {
			return err
		}
	}
	if len(entities) == 0 {
		fmt.Fprintln(out, "No entities found.")
		return nil
	}
	t := tui.NewTable(th, "UUID", "NAME", "SUMMARY")
	for _, e := range entities {
		t.Row(e.UUID, e.Name, ansi.Truncate(e.Summary, entitySummaryWidth, "..."))
	}
	_, err := fmt.Fprint(out, t.Render())
	return err
}
