package channelcmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

func newChannelApplyCmd(g *apcmd.Globals) *cobra.Command {
	file := ""
	cmd := &cobra.Command{
		Use:   "apply -f <file>",
		Short: "Apply a Channel YAML manifest.",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(file)
			if err != nil {
				return fmt.Errorf("read %s: %w", file, err)
			}
			docs, err := manifests.Split(data)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, d := range docs {
				if err := kube.Apply(cmd.Context(), b.Dynamic, d, "ap-apply"); err != nil {
					return fmt.Errorf("apply %s/%s: %w", d.GetKind(), d.GetName(), err)
				}
				fmt.Fprintf(out, "applied %s/%s\n", d.GetKind(), d.GetName())
			}
			return nil
		},
	}
	apcmd.FileFlag(cmd, &file, "Path to manifest YAML")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}
