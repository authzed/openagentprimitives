package sessioncmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	humanize "github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/uihelpers"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// renderOperationsSummary appends a short, K8s-only summary of the
// operations declared during the session: one line per op with its call
// count and tool list. The full audit trail (with descriptions and
// per-call reasons) lives in `oap session operations`.
func renderOperationsSummary(ctx context.Context, out io.Writer, b *kube.Bundle, sessionName string) {
	var tcs spiceboxv1alpha1.ToolCallList
	if err := b.Controller.List(ctx, &tcs,
		client.InNamespace(b.Namespace),
		client.MatchingLabels{"agentsession": sessionName}); err != nil {
		// A bare return here would silently omit the section, reading as
		// "no operations" rather than "couldn't fetch". Surface a one-line
		// warning instead (mirrors runSessionOperations' surfaced warnings).
		fmt.Fprintf(out, "Operations: (could not list ToolCalls: %v)\n", err)
		return
	}
	views := groupByOperation(tcs.Items)
	if len(views) == 0 {
		return
	}
	fmt.Fprintln(out, "Operations:")
	for _, v := range views {
		tools := map[string]struct{}{}
		for _, c := range v.Calls {
			tools[c.Tool] = struct{}{}
		}
		toolList := make([]string, 0, len(tools))
		for t := range tools {
			toolList = append(toolList, t)
		}
		fmt.Fprintf(out, "  %s — %d call(s)  [%s]\n",
			v.ID, len(v.Calls), strings.Join(toolList, ", "))
	}
	fmt.Fprintf(out, "  (run `oap session operations %s` for descriptions and per-call reasons)\n", sessionName)
}

func newSessionShowCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show status of an AgentSession",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			var s spiceboxv1alpha1.AgentSession
			if err := b.Controller.Get(cmd.Context(), client.ObjectKey{Namespace: b.Namespace, Name: args[0]}, &s); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Name:       %s\n", s.Name)
			fmt.Fprintf(out, "Namespace:  %s\n", s.Namespace)
			fmt.Fprintf(out, "Class:      %s\n", s.Spec.Class)

			phase := s.Status.Phase
			if phase == "" {
				phase = spiceboxv1alpha1.AgentSessionPhasePending
			}
			fmt.Fprintf(out, "Phase:      %s\n", phase)

			// Progress
			fmt.Fprintln(out, "Progress:")
			if s.Status.Progress != nil {
				fmt.Fprintf(out, "  Turns:        %d\n", s.Status.Progress.TurnCount)
				fmt.Fprintf(out, "  InputTokens:  %d\n", s.Status.Progress.InputTokens)
				fmt.Fprintf(out, "  OutputTokens: %d\n", s.Status.Progress.OutputTokens)
				fmt.Fprintf(out, "  ToolCalls:    %d\n", s.Status.Progress.ToolCallCount)
			} else {
				fmt.Fprintf(out, "  (none)\n")
			}

			// Conditions
			fmt.Fprintln(out, "Conditions:")
			uihelpers.PrintConditions(out, s.Status.Conditions, "  ")

			// Result
			if s.Status.Result != nil {
				fmt.Fprintln(out, "Result:")
				fmt.Fprintf(out, "  Summary: %s\n", s.Status.Result.Summary)
				for _, a := range s.Status.Result.Artifacts {
					fmt.Fprintf(out, "  Artifact: id=%s description=%s\n", a.ID, a.Description)
				}
			}

			// FailureReason
			if s.Status.FailureReason != "" {
				fmt.Fprintf(out, "FailureReason: %s\n", s.Status.FailureReason)
			}

			// RunnerNotes
			if len(s.Status.RunnerNotes) > 0 {
				fmt.Fprintln(out, "RunnerNotes:")
				for _, n := range s.Status.RunnerNotes {
					fmt.Fprintf(out, "  [%s] %s\n", n.Time.UTC().Format("15:04:05"), n.Message)
				}
			}

			// Slot pins: display-only mirror of SpiceDB's slot_pin relation (see
			// spiceboxv1alpha1.SlotPin's doc comment) — one line per
			// single-occupancy resource type the session is currently pinned to.
			if len(s.Status.SlotPins) > 0 {
				fmt.Fprintln(out, "Slot pins:")
				for _, p := range s.Status.SlotPins {
					if p.MovedBy != "" {
						when := "-"
						if p.MovedAt != nil && !p.MovedAt.IsZero() {
							when = humanize.Time(p.MovedAt.Time)
						}
						fmt.Fprintf(out, "  %s → %s (moved %s by %s)\n", p.ResourceType, p.ResourceID, when, p.MovedBy)
					} else {
						fmt.Fprintf(out, "  %s → %s\n", p.ResourceType, p.ResourceID)
					}
				}
			}

			// Operations summary (built from ToolCall labels — no memory fetch).
			renderOperationsSummary(cmd.Context(), out, b, s.Name)

			return nil
		},
	}
}
