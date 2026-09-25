package toolscmd

import (
	"fmt"

	"github.com/spf13/cobra"

	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/validator"
)

// newMCPTestCmd is the MCP analog of `oap tools toolspec test`: replays each
// TestCase persisted under spec.generation.testCases through validator.Check
// and reports `N/M passed` with one bullet per failure. Non-zero exit iff
// any case's Decision.Allow disagrees with TestCase.ExpectedAllow.
func newMCPTestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "test <spec.yaml>",
		Short: "Replay persisted MCP test cases against a spec",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sp, err := mcpspec.Load(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if sp.Generation == nil || len(sp.Generation.TestCases) == 0 {
				fmt.Fprintln(out, "no test cases to run")
				return nil
			}
			cases := sp.Generation.TestCases
			pass := 0
			var failures []string
			for _, tc := range cases {
				d, err := validator.Check(sp, validator.Invocation{
					ToolName: tc.ToolName,
					Args:     tc.Args,
				})
				if err != nil {
					failures = append(failures, fmt.Sprintf("- %s: internal error: %v", tc.Intent, err))
					continue
				}
				if d.Allow == tc.ExpectedAllow {
					pass++
					continue
				}
				reason := ""
				if d.FailedOn != nil {
					reason = " — " + d.FailedOn.Message
				}
				failures = append(failures, fmt.Sprintf("- %s: expected allow=%t, got allow=%t%s", tc.Intent, tc.ExpectedAllow, d.Allow, reason))
			}
			fmt.Fprintf(out, "%d/%d passed\n", pass, len(cases))
			for _, f := range failures {
				fmt.Fprintln(out, f)
			}
			if len(failures) > 0 {
				return fmt.Errorf("%d case(s) failed", len(failures))
			}
			return nil
		},
	}
	return cmd
}
