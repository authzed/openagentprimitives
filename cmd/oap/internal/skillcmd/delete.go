package skillcmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newSkillDeleteCmd(g *apcmd.Globals) *cobra.Command {
	var cluster bool
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a Skill (or ClusterSkill with --cluster)",
		Long: "<name> is the Kubernetes object name (as shown in `oap skill list`).\n\n" +
			"Note: a Skill materialized by a SkillSource is recreated on the next sync; " +
			"delete the SkillSource (`oap skill source delete`) to stop materializing it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			if cluster {
				obj := &spiceboxv1alpha1.ClusterSkill{}
				obj.Name = args[0]
				if err := b.Controller.Delete(cmd.Context(), obj); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "deleted ClusterSkill %s\n", args[0])
				return nil
			}
			obj := &spiceboxv1alpha1.Skill{}
			obj.Namespace = b.Namespace
			obj.Name = args[0]
			if err := b.Controller.Delete(cmd.Context(), obj); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted Skill %s/%s\n", b.Namespace, args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&cluster, "cluster", false, "Delete a cluster-scoped ClusterSkill")
	return cmd
}
