package toolscmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/pkg/tools/catalog"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/validator"
)

func newToolspecTestCmd() *cobra.Command {
	var toolkitsDir string
	cmd := &cobra.Command{
		Use:   "test <spec.yaml>",
		Short: "Replay persisted test cases against a spec",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sp, err := spec.Load(args[0])
			if err != nil {
				return err
			}
			dir := resolveToolkitsDir(toolkitsDir)
			cat, err := catalog.LoadDir(dir)
			if err != nil {
				return fmt.Errorf("load catalog %s: %w", dir, err)
			}
			var tk *toolkit.Toolkit
			for _, t := range cat.Toolkits {
				if t.Name == sp.Toolkit.Name {
					tk = t
					break
				}
			}
			if tk == nil {
				return fmt.Errorf("toolkit %q referenced by spec not found in %s", sp.Toolkit.Name, dir)
			}

			if sp.Generation == nil || len(sp.Generation.TestCases) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no test cases to run")
				return nil
			}

			cases := sp.Generation.TestCases
			pass := 0
			var failures []string
			for _, tc := range cases {
				d, err := validator.Check(tk, sp, validator.Invocation{
					Command:       tk.Target.Binary,
					Argv:          tc.Argv,
					Env:           tc.Env,
					Cwd:           tc.Cwd,
					BinaryVersion: tc.BinaryVersion,
				})
				if err != nil {
					failures = append(failures, fmt.Sprintf("- %s: internal error: %v", tc.Intent, err))
					continue
				}
				if d.Allow == tc.ExpectAllow {
					pass++
					continue
				}
				reason := ""
				if d.FailedOn != nil {
					reason = " — " + d.FailedOn.Message
				}
				failures = append(failures, fmt.Sprintf("- %s: expected allow=%t, got allow=%t%s", tc.Intent, tc.ExpectAllow, d.Allow, reason))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%d/%d passed\n", pass, len(cases))
			for _, f := range failures {
				fmt.Fprintln(cmd.OutOrStdout(), f)
			}
			if len(failures) > 0 {
				return fmt.Errorf("%d case(s) failed", len(failures))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&toolkitsDir, "toolkits", "", "toolkit catalog dir (env TOOLSPEC_TOOLKITS)")
	return cmd
}
