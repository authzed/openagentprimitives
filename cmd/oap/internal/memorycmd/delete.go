package memorycmd

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/memclient"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

func newMemoryDeleteCmd(g *apcmd.Globals) *cobra.Command {
	var confirm bool
	cmd := &cobra.Command{
		Use:   "delete <session> <entry-id>",
		Short: "Delete a memory entry",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMemoryDelete(cmd, g, args[0], args[1], confirm)
		},
	}
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Skip confirmation prompt")
	return cmd
}

func runMemoryDelete(cmd *cobra.Command, g *apcmd.Globals, sessionName, entryID string, skipConfirm bool) error {
	ctx := cmd.Context()
	b, err := g.Bundle()
	if err != nil {
		return err
	}
	conn, err := memclient.Connect(ctx, b, sessionName, "")
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

	if !skipConfirm {
		fmt.Fprintf(cmd.OutOrStdout(),
			"Delete entry %s (kind: %s) from session %s?\nType 'yes' to confirm: ",
			e.ID, e.Kind, sessionName)
		scanner := bufio.NewScanner(cmd.InOrStdin())
		if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "yes" {
			fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
			return nil
		}
	}

	if err := conn.Client.Delete(ctx, conn.Scope, e.Kind, e.ID); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s\n", e.ID)
	return nil
}
