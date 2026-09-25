package toolscmd

import (
	"fmt"

	"github.com/spf13/cobra"

	toolspeccel "github.com/authzed/openagentprimitives/pkg/tools/cel"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

func newToolspecCheckCmd() *cobra.Command {
	var toolkitPath string
	cmd := &cobra.Command{
		Use:   "check <spec.yaml>",
		Short: "Schema + CEL compile check a spec (against its toolkit)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sp, err := spec.Load(args[0])
			if err != nil {
				return err
			}
			if toolkitPath != "" {
				tk, err := toolkit.Load(toolkitPath)
				if err != nil {
					return err
				}
				if tk.Name != sp.Toolkit.Name {
					return fmt.Errorf("spec references toolkit %q, got %q", sp.Toolkit.Name, tk.Name)
				}
				if tk.ToolkitRevision != sp.Toolkit.Revision {
					return fmt.Errorf("spec pins revision %q, toolkit is %q", sp.Toolkit.Revision, tk.ToolkitRevision)
				}
				// A flag the toolkit declares RequiresConstraint decides how
				// much authority the call carries, and allowSubcommands cannot
				// bound it — narrowing subcommands leaves every global flag
				// reachable. Fatal rather than a warning for that reason.
				if err := spec.ValidateRequiredFlagConstraints(tk.FlagsRequiringConstraint(), sp); err != nil {
					return err
				}
			}
			// Compile each CEL expression (constraints + exceptions.when).
			for i, c := range sp.Constraints {
				if _, err := toolspeccel.Compile(c.CEL); err != nil {
					return fmt.Errorf("constraints[%d]: %w", i, err)
				}
			}
			for i, ex := range sp.Exceptions {
				if _, err := toolspeccel.Compile(ex.When); err != nil {
					return fmt.Errorf("exceptions[%d].when: %w", i, err)
				}
			}
			// Non-fatal lint: a CEL constraint with no message renders to
			// agents as a generic "an additional constraint applies" line.
			for _, w := range spec.LintConstraintMessages(sp.Constraints) {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok: spec %q — %d constraint(s), %d exception(s)\n",
				sp.Name, len(sp.Constraints), len(sp.Exceptions))
			return nil
		},
	}
	cmd.Flags().StringVar(&toolkitPath, "toolkit", "", "optional toolkit for cross-check")
	return cmd
}
