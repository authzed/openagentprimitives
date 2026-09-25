package toolscmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/toolscli"
)

func newToolsApplyCmd(g *apcmd.Globals) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Server-side apply a multi-doc YAML stream of tool resources (any kind)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if path == "" {
				return fmt.Errorf("-f is required")
			}
			c, ns, err := toolsListClientFactory(g)
			if err != nil {
				return err
			}
			var r io.Reader
			if path == "-" {
				r = cmd.InOrStdin()
			} else {
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				defer f.Close()
				r = f
			}
			results, err := toolscli.ApplyStream(cmd.Context(), c, ns, r)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			anyErr := false
			for _, res := range results {
				if res.Err != nil {
					fmt.Fprintf(out, "error: %v\n", res.Err)
					anyErr = true
					continue
				}
				verb := "configured"
				if res.Created {
					verb = "created"
				}
				if res.Updated {
					verb = "configured"
				}
				name := res.Name
				if res.Namespace != "" {
					name = res.Namespace + "/" + name
				}
				fmt.Fprintf(out, "%s/%s %s\n", res.Kind, name, verb)
			}
			if anyErr {
				return fmt.Errorf("one or more documents failed to apply")
			}
			return nil
		},
	}
	apcmd.FileFlag(cmd, &path, "Path to YAML file (or '-' for stdin)")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}
