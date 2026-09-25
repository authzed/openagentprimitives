package plangatecmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/memclient"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// planGateReport is the whole answer for one session: the plan-wide dataset
// plus the per-phase breakdown the ratchet needs.
type planGateReport struct {
	Session string                `json:"session"`
	Digest  string                `json:"planDigest"`
	Phases  int                   `json:"phaseCount"`
	Metrics plangate.Metric       `json:"metrics"`
	Drift   []plangate.PhaseUsage `json:"phaseDrift"`
}

func newPlanGateMetricsCmd(g *apcmd.Globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "metrics <session>",
		Short: "Report the plan gate's logging-mode dataset for a session",
		Long: `Metrics reconstructs the session's frozen plan from its append-only
plan-gate log and reports what enforcement would have done.

The plan-wide numbers are the four the enforcement decision is gated on: how
many calls the gate governed, how many enforcement would have refused, the
approvals it would have raised by tier, and the gap between reach the agent
declared and reach it actually used.

The per-phase breakdown is the same question asked where it is actionable.
"Declared but never exercised" is the concrete form of over-declaration, and a
plan-wide total cannot say which phase to narrow — a use-it-or-lose-it ratchet
drops handles from the phase that did not use them, so it needs the per-phase
view. A phase that never ran at all is reported separately from one that ran and
used everything: both exercise nothing, and only the first is over-declaration.

This reads the log and nothing else. It makes no changes and reaches no
decision — a session run in logging mode produces exactly this, which is the
evidence the enforcement default is meant to rest on.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlanGateMetrics(cmd, g, args[0], asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the report as JSON")
	return cmd
}

func runPlanGateMetrics(cmd *cobra.Command, g *apcmd.Globals, sessionName string, asJSON bool) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	b, err := g.Bundle()
	if err != nil {
		return err
	}
	conn, err := memclient.Connect(ctx, b, sessionName, "")
	if err != nil {
		return err
	}
	defer conn.Close()

	records, err := planGateRecordsFor(ctx, conn.Client, conn.Scope)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		// Not an error: a session that never planned, or ran with the gate
		// disabled, legitimately has no log. Saying so beats printing zeroes
		// that read as "the agent declared nothing and used nothing".
		fmt.Fprintf(out, "No plan-gate records for %s.\n"+
			"The gate may be disabled for this session, or the agent never declared a plan.\n",
			sessionName)
		return nil
	}

	plan, ok := plangate.PlanFromRecords(records)
	if !ok {
		return fmt.Errorf("plan-gate records exist for %s but no frozen plan could be rebuilt from them; "+
			"the log may be truncated", sessionName)
	}

	report := planGateReport{
		Session: sessionName,
		Digest:  plan.Digest(),
		Phases:  len(plan.Phases),
		Metrics: plangate.Metrics(plan, records),
		Drift:   plangate.PhaseDrift(plan, records),
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	renderPlanGateReport(out, report)
	return nil
}

// planGateRecordsFor reads the session's plan-gate log.
func planGateRecordsFor(ctx context.Context, cl memory.Memory, scope memory.Scope) ([]plangateaudit.Content, error) {
	recs, err := plangateaudit.List(ctx, cl, scope)
	if err != nil {
		return nil, fmt.Errorf("read plan-gate log: %w", err)
	}
	return recs, nil
}

func renderPlanGateReport(out io.Writer, r planGateReport) {
	fmt.Fprintf(out, "Plan gate — %s\n", r.Session)
	fmt.Fprintf(out, "  plan %s  (%d phase(s))\n\n", shortDigest(r.Digest), r.Phases)

	m := r.Metrics
	fmt.Fprintf(out, "  gated calls        %d\n", m.GatedCalls)
	fmt.Fprintf(out, "  would deny         %d (%.0f%%)\n", m.WouldDeny, m.WouldDenyRate()*100)
	fmt.Fprintf(out, "  declared handles   %d\n", m.DeclaredHandles)
	fmt.Fprintf(out, "  exercised handles  %d\n", m.ExercisedHandles)
	if len(m.ApprovalsByTier) > 0 {
		fmt.Fprintf(out, "  approvals by tier  %s\n", renderTiers(m.ApprovalsByTier))
	}
	if len(m.ExercisedNotDeclared) > 0 {
		// The case enforcement would have BLOCKED — the loudest number here,
		// because it is the one that turns into a broken session on the day the
		// default flips.
		fmt.Fprintf(out, "  used but NOT declared: %s\n", strings.Join(m.ExercisedNotDeclared, ", "))
	}

	fmt.Fprintf(out, "\n  per phase:\n")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "    #\tphase\tran\tdeclared\tused\tnever used")
	for _, p := range r.Drift {
		ran := "yes"
		if !p.Entered {
			ran = "NO"
		}
		never := "-"
		if len(p.DeclaredNeverExercised) > 0 {
			never = strings.Join(p.DeclaredNeverExercised, ", ")
		}
		label := p.Label
		if label == "" {
			label = "(unlabelled)"
		}
		fmt.Fprintf(tw, "    %d\t%s\t%s\t%d\t%d\t%s\n",
			p.Index, label, ran, len(p.Declared), len(p.Exercised), never)
	}
	_ = tw.Flush()
}

func renderTiers(byTier map[string]int) string {
	// Fixed order rather than map iteration: a report whose columns move
	// between runs cannot be diffed across sessions, which is the whole point
	// of collecting it.
	var parts []string
	for _, t := range []string{"0", "1", "2"} {
		if n, ok := byTier[t]; ok {
			parts = append(parts, fmt.Sprintf("tier%s=%d", t, n))
		}
	}
	return strings.Join(parts, " ")
}

// shortDigest truncates a plan digest for display. Duplicated (not shared)
// from installcmd's identical image-digest helper: the two live in unrelated
// command families and the helper is a one-line string truncation, not a
// seam worth exporting across packages for.
func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return d
}
