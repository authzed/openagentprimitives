package toolscmd

import (
	"fmt"

	"github.com/spf13/cobra"

	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
	mcptrust "github.com/authzed/openagentprimitives/pkg/tools/mcp/trust"
	validator "github.com/authzed/openagentprimitives/pkg/tools/mcp/validator"
)

// newMCPCheckCmd is the MCP analog of `oap tools toolspec check`: structural
// load + CEL compile of an MCPServer spec, with non-fatal warnings surfaced
// after a successful compile.
func newMCPCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check <spec.yaml>",
		Short: "Schema + CEL compile check an MCPServer spec",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sp, err := mcpspec.Load(args[0])
			if err != nil {
				return err
			}
			res, err := mcpspec.Compile(sp)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			testCases := 0
			if sp.Generation != nil {
				testCases = len(sp.Generation.TestCases)
			}
			totalConstraints := 0
			for _, t := range sp.Tools {
				totalConstraints += len(t.Args.Constraints)
			}
			fmt.Fprintf(out, "ok: spec %q — %d tool(s), %d constraint(s), %d test case(s)\n",
				sp.Name, len(sp.Tools), totalConstraints, testCases)
			if len(res.Warnings) > 0 {
				fmt.Fprintln(out, "warnings:")
				for _, w := range res.Warnings {
					fmt.Fprintf(out, "  %s: %s\n", w.Path, w.Message)
				}
			}
			for _, tool := range sp.Tools {
				if line := mcptrust.FromSpec(tool.Trust); line != "" {
					fmt.Fprintf(out, "  %s trust:           %s\n", tool.Name, line)
				}
				for _, ax := range []struct {
					flag  bool
					name  string
					check func(mcpspec.Trust) bool
				}{
					{tool.Deny.Trust.OutcomesIrreversible, "trust.outcomesIrreversible",
						func(tr mcpspec.Trust) bool {
							return validator.ContainsEnumField(tr.InputMetadata, "outcomes", "irreversible")
						}},
					{tool.Deny.Trust.DestinationPublic, "trust.destinationPublic",
						func(tr mcpspec.Trust) bool {
							return validator.ContainsEnumField(tr.InputMetadata, "destination", "public")
						}},
					{tool.Deny.Trust.SourceUntrustedPublic, "trust.sourceUntrustedPublic",
						func(tr mcpspec.Trust) bool {
							return validator.ContainsEnumField(tr.ReturnMetadata, "source", "untrustedPublic")
						}},
				} {
					if !ax.flag {
						continue
					}
					mark := "MISS"
					if ax.check(tool.Trust) {
						mark = "HIT"
					}
					fmt.Fprintf(out, "  %s deny.%-30s %s\n", tool.Name, ax.name, mark)
				}
			}
			return nil
		},
	}
	return cmd
}
