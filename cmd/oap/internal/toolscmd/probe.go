package toolscmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/toolscli"
	"github.com/authzed/openagentprimitives/pkg/tools/contract"
)

func newToolsProbeCmd(g *apcmd.Globals) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "probe -f <file>",
		Short: "Probe a live tool backend using the spec file (e.g. MCP tools/list)",
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
			p, ok := k.(contract.Prober)
			if !ok {
				return fmt.Errorf("kind %s does not support probe", k.Name())
			}
			res, err := p.ProbeFile(path)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			// Capabilities come from the stream this command writes to, so a
			// pipe, --no-color and NO_COLOR each land on the colorless theme.
			return toolscli.RenderProbeResult(out, g.Theme(out), res)
		},
	}
	apcmd.FileFlag(cmd, &path, "Path to spec file")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}
