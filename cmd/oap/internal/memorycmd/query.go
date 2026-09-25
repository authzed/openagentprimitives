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

func newMemoryQueryCmd(g *apcmd.Globals) *cobra.Command {
	var (
		kinds      string
		tags       []string
		since      string
		until      string
		limit      int
		linkedTo   []string
		linkedFrom []string
		fields     []string
		ids        []string
		asJSON     bool
	)
	cmd := &cobra.Command{
		Use:   "query <session>",
		Short: "Query memory entries with rich filters",
		Long: `Query memory entries using link filters, field predicates, and all
the filters available in 'oap memory list'. Use --linked-to to find entries
that reference a specific entry, --field for content field equality.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMemoryQuery(cmd, g, args[0], kinds, tags, since, until, limit, linkedTo, linkedFrom, fields, ids, asJSON)
		},
	}
	apcmd.MemoryKindFilterFlag(cmd, &kinds)
	apcmd.MemoryTagFilterFlag(cmd, &tags)
	apcmd.TimeRangeFlags(cmd, &since, &until)
	apcmd.EntryLimitFlag(cmd, &limit)
	cmd.Flags().StringSliceVar(&linkedTo, "linked-to", nil, "Format: kind:id (repeatable)")
	cmd.Flags().StringSliceVar(&linkedFrom, "linked-from", nil, "Format: kind:id (repeatable)")
	apcmd.MemoryFieldFilterFlag(cmd, &fields)
	cmd.Flags().StringSliceVar(&ids, "id", nil, "Exact entry ID filter (repeatable)")
	apcmd.JSONFlag(cmd, &asJSON, "Output as JSON array")
	return cmd
}

func runMemoryQuery(cmd *cobra.Command, g *apcmd.Globals, sessionName, kinds string, tags []string, since, until string, limit int, linkedTo, linkedFrom, fields, ids []string, asJSON bool) error {
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

	q := memory.Query{Scope: conn.Scope, Limit: limit, Tags: tags, IDs: ids}
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
	for _, lt := range linkedTo {
		lf, err := parseLinkFilter(lt)
		if err != nil {
			return fmt.Errorf("--linked-to: %w", err)
		}
		q.LinkedTo = append(q.LinkedTo, lf)
	}
	for _, lf := range linkedFrom {
		f, err := parseLinkFilter(lf)
		if err != nil {
			return fmt.Errorf("--linked-from: %w", err)
		}
		q.LinkedFrom = append(q.LinkedFrom, f)
	}
	for _, f := range fields {
		ff, err := parseFieldFilter(f)
		if err != nil {
			return fmt.Errorf("--field: %w", err)
		}
		q.FieldEquals = append(q.FieldEquals, ff)
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

func parseLinkFilter(s string) (memory.LinkFilter, error) {
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return memory.LinkFilter{}, fmt.Errorf("%q must be kind:id", s)
	}
	return memory.LinkFilter{Kind: s[:i], ID: s[i+1:]}, nil
}

func parseFieldFilter(s string) (memory.FieldFilter, error) {
	// path? (IsNil) and path! (NotNil)
	if strings.HasSuffix(s, "?") {
		return memory.FieldFilter{Path: s[:len(s)-1], Op: memory.FieldOpIsNil}, nil
	}
	if strings.HasSuffix(s, "!") {
		return memory.FieldFilter{Path: s[:len(s)-1], Op: memory.FieldOpNotNil}, nil
	}
	// Two-char operators: >=, <=
	for _, op := range []struct {
		sym string
		op  memory.FieldOp
	}{
		{">=", memory.FieldOpGte},
		{"<=", memory.FieldOpLte},
	} {
		if i := strings.Index(s, op.sym); i > 0 && i+len(op.sym) < len(s) {
			return memory.FieldFilter{Path: s[:i], Op: op.op, Value: s[i+len(op.sym):]}, nil
		}
	}
	// Single-char operators: >, <, =
	for _, op := range []struct {
		sym byte
		op  memory.FieldOp
	}{
		{'>', memory.FieldOpGt},
		{'<', memory.FieldOpLt},
		{'=', memory.FieldOpEq},
	} {
		if i := strings.IndexByte(s, op.sym); i > 0 && i < len(s)-1 {
			return memory.FieldFilter{Path: s[:i], Op: op.op, Value: s[i+1:]}, nil
		}
	}
	return memory.FieldFilter{}, fmt.Errorf("%q: use path=value, path>value, path>=value, path<value, path<=value, path? (nil), or path! (not nil)", s)
}
