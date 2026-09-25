package memorycmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/memclient"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

func newMemoryPutCmd(g *apcmd.Globals) *cobra.Command {
	var (
		file    string
		kind    string
		content string
		tags    []string
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "put <session>",
		Short: "Write a memory entry to a session",
		Long: `Write a memory entry. Input modes (mutually exclusive):
  - Pipe a full Entry JSON object via stdin
  - --file: read a full Entry JSON from a file
  - --kind + --content: construct an entry inline

The server forces scope from the URL and auto-generates the ID if empty.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMemoryPut(cmd, g, args[0], file, kind, content, tags, asJSON)
		},
	}
	apcmd.FileFlag(cmd, &file, "Path to a JSON file containing the entry")
	cmd.Flags().StringVar(&kind, "kind", "", "Entry kind (required with --content)")
	cmd.Flags().StringVar(&content, "content", "", "JSON content string")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "Tags to set (repeatable)")
	apcmd.JSONFlag(cmd, &asJSON, "Output stored entry as JSON")
	return cmd
}

func runMemoryPut(cmd *cobra.Command, g *apcmd.Globals, sessionName, file, kind, content string, tags []string, asJSON bool) error {
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

	var e memory.Entry

	switch {
	case file != "":
		data, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("read --file: %w", err)
		}
		if err := json.Unmarshal(data, &e); err != nil {
			return fmt.Errorf("decode --file: %w", err)
		}
	case kind != "" && content != "":
		e.Kind = kind
		e.Content = json.RawMessage(content)
		e.Tags = tags
	case kind != "" && content == "":
		return fmt.Errorf("--content is required when --kind is set")
	default:
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		if len(data) == 0 {
			return fmt.Errorf("no input: pipe JSON via stdin, or use --file or --kind/--content")
		}
		if err := json.Unmarshal(data, &e); err != nil {
			return fmt.Errorf("decode stdin: %w", err)
		}
	}

	if len(tags) > 0 && len(e.Tags) == 0 {
		e.Tags = tags
	}
	e.Scope = conn.Scope

	stored, err := conn.Client.Put(ctx, e)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(stored)
	}
	fmt.Fprintf(out, "Created entry: %s (kind: %s)\n", stored.ID, stored.Kind)
	return nil
}
