package preferencescmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/memclient"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

func newInspectCmd(g *apcmd.Globals) *cobra.Command {
	var turn int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "inspect <session>",
		Short: "Show the resolved per-user preferences a session's agent sees",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInspect(cmd, g, args[0], turn, asJSON)
		},
	}
	cmd.Flags().IntVar(&turn, "turn", -1, "User-turn index to resolve for (default: latest)")
	apcmd.JSONFlag(cmd, &asJSON, "Output as JSON")
	return cmd
}

func runInspect(cmd *cobra.Command, g *apcmd.Globals, sessionName string, turnIndex int, asJSON bool) error {
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

	out := cmd.OutOrStdout()
	resp, err := conn.Client.GetPreferences(ctx, b.Namespace, sessionName, turnIndex)
	if err != nil {
		if isNoPreferencesError(err) {
			fmt.Fprintf(out, "session %q's class declares no preferences.\n", sessionName)
			return nil
		}
		return err
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}
	return renderSnapshot(out, g.Theme(out), resp)
}

// isNoPreferencesError reports whether err is the specific 404 httpsrv's
// handlePreferencesGet answers when the session's AgentClass declares no
// UserPreferences at all ("class declares no preferences" —
// pkg/memory/httpsrv/preferences.go). httpclient's statusError type is
// unexported, so this matches on the rendered message rather than a status
// code: a 404 alone is ambiguous (an unknown ?turn, a mistyped session name,
// and a class-with-no-preferences all answer 404 with different bodies), and
// only this one case is "there is nothing to inspect," not "the lookup
// failed" — the others should still surface as errors.
func isNoPreferencesError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "class declares no preferences")
}

// renderSnapshot is the pure formatting half of runInspect: one table row per
// resolved key (KEY | VALUE | SOURCE | NOTE), then any Violations as trailing
// lines. A key's Locked-ness is carried by its Source value alone
// (preferences.Resolve only ever sets Locked=true alongside Source=="locked")
// so there is no separate LOCKED column to keep in sync.
func renderSnapshot(out io.Writer, th *tui.Theme, resp preferences.SnapshotResponse) error {
	t := tui.NewTable(th, "KEY", "VALUE", "SOURCE", "NOTE")
	for _, k := range resp.Snapshot.Keys {
		t.Row(k.Name, formatPreferenceValue(k.Value), string(k.Source), k.Note)
	}
	if _, err := fmt.Fprint(out, t.Render()); err != nil {
		return err
	}
	for _, v := range resp.Snapshot.Violations {
		if _, err := fmt.Fprintln(out, "violation: "+v); err != nil {
			return err
		}
	}
	return nil
}

// formatPreferenceValue renders a resolved key's raw JSON value for the
// table. nil (SourceUnset — no default, no global, no saved user value)
// prints an explicit placeholder rather than an empty cell that would read
// as a rendering bug.
func formatPreferenceValue(v *apiextv1.JSON) string {
	if v == nil {
		return "(unset)"
	}
	return strings.TrimSpace(string(v.Raw))
}
