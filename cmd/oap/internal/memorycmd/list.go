package memorycmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/memclient"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

func newMemoryListCmd(g *apcmd.Globals) *cobra.Command {
	var (
		kinds  string
		tags   []string
		since  string
		until  string
		limit  int
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "list <session>",
		Short: "List memory entries for a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMemoryList(cmd, g, args[0], kinds, tags, since, until, limit, asJSON)
		},
	}
	apcmd.MemoryKindFilterFlag(cmd, &kinds)
	apcmd.MemoryTagFilterFlag(cmd, &tags)
	apcmd.TimeRangeFlags(cmd, &since, &until)
	apcmd.EntryLimitFlag(cmd, &limit)
	apcmd.JSONFlag(cmd, &asJSON, "Output as JSON array")
	return cmd
}

func runMemoryList(cmd *cobra.Command, g *apcmd.Globals, sessionName, kinds string, tags []string, since, until string, limit int, asJSON bool) error {
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

	q := memory.Query{Scope: conn.Scope, Limit: limit, Tags: tags}
	if kinds != "" {
		q.Kinds = strings.Split(kinds, ",")
	}
	if since != "" {
		t, err := parseTime(since)
		if err != nil {
			return fmt.Errorf("--since: %w", err)
		}
		q.Since = &t
	}
	if until != "" {
		t, err := parseTime(until)
		if err != nil {
			return fmt.Errorf("--until: %w", err)
		}
		q.Until = &t
	}

	res, err := conn.Client.Query(ctx, q)
	if err != nil {
		return err
	}

	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}

	out := cmd.OutOrStdout()
	return renderQueryResult(out, g.Theme(out), res, limit)
}

// renderQueryResult prints the entry table under whatever the read has to
// declare about itself. limit is the --limit that produced res, needed because
// the truncation notice hands the reader a larger one to re-run with.
func renderQueryResult(out io.Writer, th *tui.Theme, res memory.QueryResult, limit int) error {
	if res.Partial {
		fmt.Fprintf(out, "WARNING: backend dropped predicates: %s (results may be unfiltered)\n\n",
			strings.Join(res.DroppedPredicates, ", "))
	}
	// Dropped predicates and a hit limit are independent failures of
	// completeness — the first answered a wider question than was asked, the
	// second answered part of it — so both are reported when both happened.
	if res.Truncated {
		if err := apcmd.TruncationNotice(out, "entries", limit); err != nil {
			return err
		}
	}
	if len(res.Entries) == 0 {
		fmt.Fprintln(out, "No entries found.")
		return nil
	}
	t := tui.NewTable(th, "KIND", "ID", "CREATED", "TAGS")
	for _, e := range res.Entries {
		t.Row(e.Kind, e.ID, e.CreatedAt.Format(time.DateTime), strings.Join(e.Tags, ", "))
	}
	_, err := fmt.Fprint(out, t.Render())
	return err
}

func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a valid RFC3339 timestamp or duration", s)
	}
	return time.Now().Add(-d), nil
}
