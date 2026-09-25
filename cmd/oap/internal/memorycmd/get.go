package memorycmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/memclient"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

func newMemoryGetCmd(g *apcmd.Globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "get <session> <entry-id>",
		Short: "Show full detail for a single memory entry",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMemoryGet(cmd, g, args[0], args[1], asJSON)
		},
	}
	apcmd.JSONFlag(cmd, &asJSON, "Output as JSON object")
	return cmd
}

func runMemoryGet(cmd *cobra.Command, g *apcmd.Globals, sessionName, entryID string, asJSON bool) error {
	ctx := cmd.Context()
	b, err := g.Bundle()
	if err != nil {
		return err
	}
	conn, err := memclient.Connect(ctx, b, sessionName, memoryBackend(cmd))
	if err != nil {
		return err
	}
	defer conn.Close()

	res, err := conn.Client.Query(ctx, memory.Query{
		Scope: conn.Scope,
		IDs:   []string{entryID},
	})
	if err != nil {
		return err
	}
	if len(res.Entries) == 0 {
		return fmt.Errorf("entry %q not found in session %q", entryID, sessionName)
	}
	e := res.Entries[0]

	out := cmd.OutOrStdout()
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(e)
	}

	fmt.Fprintf(out, "ID:        %s\n", e.ID)
	fmt.Fprintf(out, "Kind:      %s\n", e.Kind)
	fmt.Fprintf(out, "Scope:     %s/%s\n", e.Scope.Kind, e.Scope.ID)
	fmt.Fprintf(out, "Created:   %s\n", e.CreatedAt.Format("2006-01-02T15:04:05Z"))

	if len(e.Tags) > 0 {
		fmt.Fprintf(out, "Tags:      %s\n", strings.Join(e.Tags, ", "))
	} else {
		fmt.Fprintln(out, "Tags:      (none)")
	}

	if len(e.Links) > 0 {
		fmt.Fprintln(out, "Links:")
		for _, l := range e.Links {
			fmt.Fprintf(out, "  %s → %s:%s\n", l.Relation, l.Kind, l.ID)
		}
	} else {
		fmt.Fprintln(out, "Links:     (none)")
	}

	if len(e.Content) > 0 {
		fmt.Fprintln(out, "\nContent:")
		var pretty json.RawMessage
		if err := json.Unmarshal(e.Content, &pretty); err == nil {
			indented, _ := json.MarshalIndent(pretty, "  ", "  ")
			fmt.Fprintf(out, "  %s\n", indented)
		} else {
			fmt.Fprintf(out, "  %s\n", e.Content)
		}
	}
	return nil
}
