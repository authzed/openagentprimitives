package sessioncmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/memstream"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/portforward"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

func newSessionOperationsCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "operations <name>",
		Short: "Show the operations the agent declared and the tool calls under each",
		Long: `Reconstructs the audit trail for an AgentSession by grouping its
ToolCalls under the operation_id label they were stamped with at dispatch
time. When a memory token is available, also fetches the session memory
to recover the original new_operation description for each operation.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSessionOperations(cmd.Context(), cmd.OutOrStdout(), g, args[0])
		},
	}
}

type opView struct {
	ID          string
	Description string // "" when memory is unavailable
	Calls       []opCallView
}

type opCallView struct {
	ToolCallName string
	Tool         string
	Reason       string
	ExitCode     *int32
	Phase        string
	Created      time.Time
}

func runSessionOperations(ctx context.Context, out io.Writer, g *apcmd.Globals, sessionName string) error {
	b, err := g.Bundle()
	if err != nil {
		return err
	}

	// Confirm the session exists. The error here is the user-friendly
	// "session not found" guard; if we instead jumped straight to the
	// ToolCall listing the user would just get an empty table.
	var sess spiceboxv1alpha1.AgentSession
	if err := b.Controller.Get(ctx,
		client.ObjectKey{Namespace: b.Namespace, Name: sessionName}, &sess); err != nil {
		return fmt.Errorf("get AgentSession %s/%s: %w", b.Namespace, sessionName, err)
	}

	// Pull every ToolCall belonging to this AgentSession.
	var tcs spiceboxv1alpha1.ToolCallList
	if err := b.Controller.List(ctx, &tcs,
		client.InNamespace(b.Namespace),
		client.MatchingLabels{"agentsession": sessionName}); err != nil {
		return fmt.Errorf("list ToolCalls: %w", err)
	}

	views := groupByOperation(tcs.Items)

	// Best-effort: enrich with operation descriptions from memory. Failure
	// is non-fatal — we still print whatever we have from the ToolCall side.
	descriptions, memErr := fetchOperationDescriptions(ctx, b, sessionName)
	if memErr == nil {
		for i := range views {
			if d, ok := descriptions[views[i].ID]; ok {
				views[i].Description = d
			}
		}
	}

	if len(views) == 0 {
		fmt.Fprintf(out, "no operations recorded for AgentSession %s/%s\n", b.Namespace, sessionName)
		if memErr != nil {
			fmt.Fprintf(out, "(memory not reachable: %v)\n", memErr)
		}
		return nil
	}

	// Capabilities come from the stream this command writes to, so a pipe,
	// --no-color and NO_COLOR each land on the colorless theme. One theme for
	// the whole run: every operation's block is the same surface.
	th := g.Theme(out)

	for i, v := range views {
		if i > 0 {
			fmt.Fprintln(out)
		}
		header := fmt.Sprintf("Operation %s", v.ID)
		if v.Description != "" {
			header += "  —  " + v.Description
		}
		fmt.Fprintln(out, th.Render(th.Title, header))
		t := tui.NewTable(th, "TOOL", "REASON", "PHASE", "EXIT", "AGE", "TOOLCALL")
		for _, c := range v.Calls {
			exit := "-"
			if c.ExitCode != nil {
				exit = fmt.Sprintf("%d", *c.ExitCode)
			}
			t.Row(c.Tool, c.Reason, c.Phase, exit,
				apcmd.DurationSinceShort(c.Created), c.ToolCallName)
		}
		fmt.Fprint(out, t.Render())
	}

	if memErr != nil {
		fmt.Fprintf(out, "\n(memory not reachable; descriptions omitted: %v)\n", memErr)
	}
	return nil
}

func groupByOperation(items []spiceboxv1alpha1.ToolCall) []opView {
	byID := map[string]*opView{}
	for _, tc := range items {
		opID := tc.Labels["ap.operation"]
		if opID == "" {
			continue // pre-operations toolcall, or non-sandbox dispatch
		}
		v, ok := byID[opID]
		if !ok {
			v = &opView{ID: opID}
			byID[opID] = v
		}
		v.Calls = append(v.Calls, opCallView{
			ToolCallName: tc.Name,
			Tool:         tc.Spec.Tool,
			Reason:       tc.Annotations["ap.operation/reason"],
			ExitCode:     tc.Status.ExitCode,
			Phase:        apcmd.ToolCallPhase(&tc),
			Created:      tc.CreationTimestamp.Time,
		})
	}
	out := make([]opView, 0, len(byID))
	for _, v := range byID {
		sort.Slice(v.Calls, func(i, j int) bool { return v.Calls[i].Created.Before(v.Calls[j].Created) })
		out = append(out, *v)
	}
	// Order operations by their earliest call so the output matches the
	// chronological order the agent worked through them.
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Calls) == 0 || len(out[j].Calls) == 0 {
			return out[i].ID < out[j].ID
		}
		return out[i].Calls[0].Created.Before(out[j].Calls[0].Created)
	})
	return out
}

// fetchOperationDescriptions opens a port-forward to the operator and reads
// the session memory, scanning for `new_operation` tool_use blocks paired
// with their tool_result. Returns a map of operation_id → description.
func fetchOperationDescriptions(ctx context.Context, b *kube.Bundle, sessionName string) (map[string]string, error) {
	var sec corev1.Secret
	if err := b.Controller.Get(ctx,
		client.ObjectKey{Namespace: b.Namespace, Name: sessionName + "-memory-token"}, &sec); err != nil {
		return nil, fmt.Errorf("memory token secret: %w", err)
	}
	memToken := string(sec.Data["token"])
	if memToken == "" {
		return nil, fmt.Errorf("memory token secret has empty 'token' key")
	}

	pf, err := portforward.New(b.REST, "agentprimitives-system", "spicebox-operator", "app.kubernetes.io/name=spicebox-operator", 8082, 0)
	if err != nil {
		return nil, err
	}
	if err := pf.Start(ctx, io.Discard); err != nil {
		return nil, fmt.Errorf("port-forward: %w", err)
	}
	defer pf.Stop()

	streamer := memstream.New(pf.URL(), b.Namespace, sessionName, memToken, time.Second)
	turns, err := streamer.Fetch(ctx)
	if err != nil {
		return nil, err
	}
	return extractOperationDescriptions(turns), nil
}

// extractOperationDescriptions walks the memory turns and builds the
// operation_id → description map. Pairs new_operation tool_use blocks (carrying
// the description in their input) with the matching tool_result block (carrying
// the operation_id in its content).
func extractOperationDescriptions(turns []memory.Turn) map[string]string {
	type pendingOp struct {
		Description string
	}
	pending := map[string]pendingOp{} // tool_use ID → description
	out := map[string]string{}        // operation_id → description

	for _, t := range turns {
		for _, blk := range t.Content {
			switch blk.Type {
			case "tool_use":
				if blk.ToolUse == nil || blk.ToolUse.Name != "new_operation" {
					continue
				}
				var input struct {
					Description string `json:"description"`
				}
				_ = json.Unmarshal(blk.ToolUse.Input, &input)
				pending[blk.ToolUse.ID] = pendingOp{Description: input.Description}
			case "tool_result":
				if blk.ToolResult == nil {
					continue
				}
				p, ok := pending[blk.ToolResult.ToolUseID]
				if !ok {
					continue
				}
				delete(pending, blk.ToolResult.ToolUseID)
				var body struct {
					OperationID string `json:"operation_id"`
				}
				if err := json.Unmarshal([]byte(blk.ToolResult.Content), &body); err != nil {
					continue
				}
				if body.OperationID != "" {
					out[body.OperationID] = p.Description
				}
			}
		}
	}
	return out
}
