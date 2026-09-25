package memorycmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/memclient"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

var knownKinds = []struct {
	Name     string
	IDPrefix string
}{
	{"turn", "turn-"},
	{"authz_decision", "authzd-"},
	{"label", "label-"},
	{"approval", "approval-"},
	{"lifecycle", "lifecycle-"},
	{"relwrites_audit", "relw-"},
	{"tool_session", "toolsess-"},
	{"ui_action", "uiact-"},
	{"ui_view_model", "uivm-"},
	{"ui_view_params", "uivp-"},
}

// kindCountLimit caps the per-kind counting query. A count is a fact the
// reader acts on, so a capped one must never be printed as if it were exact —
// see formatKindCount and kindSummary.Truncated.
const kindCountLimit = 10000

type kindSummary struct {
	Name     string `json:"name"`
	IDPrefix string `json:"idPrefix"`
	Count    int    `json:"count"`
	// Truncated marks Count as a floor rather than a total: the scope holds
	// more than kindCountLimit entries of this kind. Always emitted, so a
	// consumer never has to read an absent field as "exact".
	Truncated bool `json:"truncated"`
}

// formatKindCount renders a count the reader can trust: a capped one carries a
// trailing "+" so it is never mistaken for a total.
func formatKindCount(s kindSummary) string {
	if s.Truncated {
		return fmt.Sprintf("%d+", s.Count)
	}
	return fmt.Sprintf("%d", s.Count)
}

func newMemoryKindsCmd(g *apcmd.Globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "kinds <session>",
		Short: "List registered memory kinds with entry counts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMemoryKinds(cmd, g, args[0], asJSON)
		},
	}
	apcmd.JSONFlag(cmd, &asJSON, "Output as JSON")
	return cmd
}

func runMemoryKinds(cmd *cobra.Command, g *apcmd.Globals, sessionName string, asJSON bool) error {
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

	var summaries []kindSummary
	for _, k := range knownKinds {
		res, err := conn.Client.Query(ctx, memory.Query{
			Scope: conn.Scope,
			Kinds: []string{k.Name},
			Limit: kindCountLimit,
		})
		if err != nil {
			return fmt.Errorf("query kind %s: %w", k.Name, err)
		}
		summaries = append(summaries, kindSummary{
			Name:      k.Name,
			IDPrefix:  k.IDPrefix,
			Count:     len(res.Entries),
			Truncated: res.Truncated,
		})
	}

	out := cmd.OutOrStdout()
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(summaries)
	}

	t := g.Table(out, "KIND", "COUNT", "ID PREFIX")
	for _, s := range summaries {
		t.Row(s.Name, formatKindCount(s), s.IDPrefix)
	}
	_, err = fmt.Fprint(out, t.Render())
	return err
}
