package toolscmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/pkg/tools/catalog"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/render"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

func newToolspecDescribeCmd() *cobra.Command {
	var (
		toolkitsDir string
		format      string
	)
	cmd := &cobra.Command{
		Use:   "describe <spec.yaml>",
		Short: "Render a plain-language description of a spec",
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
			out, err := render.Describe(sp, tk, render.Format(format))
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), out)
			return nil
		},
	}
	cmd.Flags().StringVar(&toolkitsDir, "toolkits", "", "toolkit catalog dir (env TOOLSPEC_TOOLKITS)")
	apcmd.RenderFormatFlag(cmd, &format, "")
	return cmd
}

func resolveToolkitsDir(flag string) string {
	if flag != "" {
		return flag
	}
	if v := os.Getenv("TOOLSPEC_TOOLKITS"); v != "" {
		return v
	}
	return "./toolkits"
}
