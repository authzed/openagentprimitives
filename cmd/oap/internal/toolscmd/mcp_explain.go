package toolscmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/validator"
)

// newMCPExplainCmd is the MCP analog of `oap tools toolspec explain`: runs
// validator.Check against a synthetic call and prints the structured trace
// for spec authors to inspect. Mirrors toolspec_explain.go's renderer.
func newMCPExplainCmd() *cobra.Command {
	var (
		specPath string
		toolName string
		argsJSON string
	)
	cmd := &cobra.Command{
		Use:   "explain --spec=<spec.yaml> --tool=<name> --args='<json>'",
		Short: "Run mcp validator against a synthetic call and print a trace",
		RunE: func(cmd *cobra.Command, _ []string) error {
			sp, err := mcpspec.Load(specPath)
			if err != nil {
				return err
			}
			var args map[string]any
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return fmt.Errorf("--args: %w", err)
			}
			d, err := validator.Check(sp, validator.Invocation{
				ToolName: toolName,
				Args:     args,
			})
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), renderMCPDecision(d))
			return nil
		},
	}
	cmd.Flags().StringVar(&specPath, "spec", "", "path to MCPServer spec YAML")
	cmd.Flags().StringVar(&toolName, "tool", "", "tool name to simulate")
	cmd.Flags().StringVar(&argsJSON, "args", "{}", "JSON-encoded args payload")
	_ = cmd.MarkFlagRequired("spec")
	_ = cmd.MarkFlagRequired("tool")
	return cmd
}

func renderMCPDecision(d *validator.Decision) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Allow: %t\n", d.Allow)
	if d.Reason != "" {
		fmt.Fprintf(&b, "Reason: %s\n", d.Reason)
	}
	if d.FailedOn != nil {
		fmt.Fprintf(&b, "FailedOn: %s — %s\n", d.FailedOn.Path, d.FailedOn.Message)
	}
	fmt.Fprintln(&b, "Trace:")
	for _, tr := range d.Trace {
		fmt.Fprintf(&b, "  [%s] %s", tr.Status, tr.Path)
		if tr.Detail != "" {
			fmt.Fprintf(&b, " — %s", tr.Detail)
		}
		b.WriteByte('\n')
		if tr.Rule != "" {
			fmt.Fprintf(&b, "         rule: %s\n", tr.Rule)
		}
	}
	if d.Parsed != nil {
		fmt.Fprintln(&b, "Parsed args:")
		fmt.Fprintf(&b, "  tool: %q\n", d.Parsed.ToolName)
		if len(d.Parsed.Args) > 0 {
			fmt.Fprintf(&b, "  args: %v\n", d.Parsed.Args)
		}
	}
	if len(d.Warnings) > 0 {
		fmt.Fprintln(&b, "Warnings:")
		for _, w := range d.Warnings {
			fmt.Fprintf(&b, "  %s: %s\n", w.Kind, w.Message)
		}
	}
	if len(d.Redactions) > 0 {
		fmt.Fprintln(&b, "Redactions:")
		for id, desc := range d.Redactions {
			fmt.Fprintf(&b, "  id=%s kind=%s name=%s — %s\n", id, desc.Kind, desc.Name, desc.Description)
		}
	}
	return b.String()
}
