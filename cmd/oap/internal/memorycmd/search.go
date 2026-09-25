package memorycmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/memclient"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

func newMemorySearchCmd(g *apcmd.Globals) *cobra.Command {
	var (
		text   string
		kinds  string
		tags   []string
		fields []string
		since  string
		until  string
		limit  int
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "search <session>",
		Short: "Search memory entries using ranked retrieval",
		Long: `Search memory entries using natural language text, full-text search,
and vector similarity. Results are ranked by relevance score. Requires
search providers to be configured on the operator.

Use --text for natural language queries. Structured filters (--kind,
--tag, --field, --since, --until) pre-filter before ranking.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMemorySearch(cmd, g, args[0], text, kinds, tags, fields, since, until, limit, asJSON)
		},
	}
	cmd.Flags().StringVar(&text, "text", "", "Natural language search query")
	apcmd.MemoryKindFilterFlag(cmd, &kinds)
	apcmd.MemoryTagFilterFlag(cmd, &tags)
	apcmd.MemoryFieldFilterFlag(cmd, &fields)
	apcmd.TimeRangeFlags(cmd, &since, &until)
	apcmd.ResultLimitFlag(cmd, &limit)
	apcmd.JSONFlag(cmd, &asJSON, "Output as JSON")
	return cmd
}

func runMemorySearch(cmd *cobra.Command, g *apcmd.Globals, sessionName, text, kinds string, tags, fields []string, since, until string, limit int, asJSON bool) error {
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

	req := memory.SearchRequest{
		Scopes: []memory.Scope{conn.Scope},
		Text:   text,
		Tags:   tags,
		Limit:  limit,
	}
	if kinds != "" {
		req.Kinds = strings.Split(kinds, ",")
	}
	if since != "" {
		t, err := parseTime(since)
		if err != nil {
			return fmt.Errorf("--since: %w", err)
		}
		req.Since = &t
	}
	if until != "" {
		t, err := parseTime(until)
		if err != nil {
			return fmt.Errorf("--until: %w", err)
		}
		req.Until = &t
	}
	for _, f := range fields {
		ff, err := parseFieldFilter(f)
		if err != nil {
			return fmt.Errorf("--field: %w", err)
		}
		req.Fields = append(req.Fields, ff)
	}

	res, err := conn.Client.Search(ctx, req)
	if err != nil {
		return err
	}

	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	out := cmd.OutOrStdout()
	return renderSearchResult(out, g.Theme(out), res, limit)
}

// renderSearchResult prints the ranked table under whatever the search has to
// declare about itself. limit is the --limit that produced res; see
// renderQueryResult.
func renderSearchResult(out io.Writer, th *tui.Theme, res memory.MergedSearchResult, limit int) error {
	if len(res.Entries) == 0 {
		fmt.Fprintln(out, "No results found.")
		return nil
	}

	var allDropped []string
	for _, pr := range res.PerProvider {
		allDropped = append(allDropped, pr.DroppedFilters...)
	}
	if len(allDropped) > 0 {
		fmt.Fprintf(out, "NOTE: some providers dropped filters: %s\n\n",
			strings.Join(allDropped, ", "))
	}
	// Relevance decaying down the list is a reason the window is small, not a
	// reason to leave the reader guessing whether it was reached.
	if res.Truncated {
		if err := apcmd.TruncationNotice(out, "results", limit); err != nil {
			return err
		}
	}

	t := tui.NewTable(th, "SCORE", "SOURCE", "KIND", "ID")
	for _, se := range res.Entries {
		t.Row(fmt.Sprintf("%.3f", se.Score), se.Source, se.Entry.Kind, se.Entry.ID)
	}
	_, err := fmt.Fprint(out, t.Render())
	return err
}
