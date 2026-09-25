package toolscmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/toolscli"
	"github.com/authzed/openagentprimitives/pkg/tools/contract"
)

func newToolsLintCmd(g *apcmd.Globals) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "lint -f <file>",
		Short: "Run offline lint on a tool spec file",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if path == "" {
				return fmt.Errorf("-f is required")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			k, err := dispatchKind(data)
			if err != nil {
				return err
			}
			l, ok := k.(contract.Linter)
			if !ok {
				return fmt.Errorf("kind %s does not support lint", k.Name())
			}
			diags, err := l.LintFile(path)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			// Capabilities come from the stream this command writes to, so a
			// pipe, --no-color and NO_COLOR each land on the colorless theme.
			if anyErr := toolscli.RenderDiagnostics(out, g.Theme(out), diags); anyErr {
				return fmt.Errorf("lint failed")
			}
			return nil
		},
	}
	apcmd.FileFlag(cmd, &path, "Path to spec file")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}
