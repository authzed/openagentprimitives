package toolscmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/toolscli"
	"github.com/authzed/openagentprimitives/pkg/tools/contract"
)

func newToolsValidateCmd(g *apcmd.Globals) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "validate -f <file>",
		Short: "Validate a tool spec file (kind detected from apiVersion+kind or flat structure)",
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
			v, ok := k.(contract.Validator)
			if !ok {
				return fmt.Errorf("kind %s does not support validate", k.Name())
			}
			diags, err := v.ValidateFile(path)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			// Capabilities come from the stream this command writes to, so a
			// pipe, --no-color and NO_COLOR each land on the colorless theme.
			if anyErr := toolscli.RenderDiagnostics(out, g.Theme(out), diags); anyErr {
				return fmt.Errorf("validation failed")
			}
			return nil
		},
	}
	apcmd.FileFlag(cmd, &path, "Path to spec file")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}
