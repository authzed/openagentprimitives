package skillcmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newSkillListCmd(g *apcmd.Globals) *cobra.Command {
	var cluster bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Skills (or ClusterSkills with --cluster)",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			// Capabilities come from the stream this command writes to, so a
			// pipe, --no-color and NO_COLOR each land on the colorless theme.
			t := newSkillTable(g.Theme(out))
			n := 0
			anyInvalid := false

			if cluster {
				var list spiceboxv1alpha1.ClusterSkillList
				if err := b.Controller.List(cmd.Context(), &list); err != nil {
					return err
				}
				n = len(list.Items)
				for i := range list.Items {
					s := &list.Items[i]
					valid, _ := validCondition(s.Status.Conditions)
					if valid != "True" {
						anyInvalid = true
					}
					sha := ""
					if s.Spec.Source != nil {
						sha = s.Spec.Source.ResolvedSHA
					}
					skillRow(t, s.Spec.CanonicalName, s.Name, valid, pinnedCondition(s.Status.Conditions), sha, s.CreationTimestamp.Time)
				}
			} else {
				var list spiceboxv1alpha1.SkillList
				if err := b.Controller.List(cmd.Context(), &list, client.InNamespace(b.Namespace)); err != nil {
					return err
				}
				n = len(list.Items)
				for i := range list.Items {
					s := &list.Items[i]
					valid, _ := validCondition(s.Status.Conditions)
					if valid != "True" {
						anyInvalid = true
					}
					sha := ""
					if s.Spec.Source != nil {
						sha = s.Spec.Source.ResolvedSHA
					}
					skillRow(t, s.Spec.CanonicalName, s.Name, valid, pinnedCondition(s.Status.Conditions), sha, s.CreationTimestamp.Time)
				}
			}

			kind := "Skills"
			if cluster {
				kind = "ClusterSkills"
			}
			if n == 0 {
				fmt.Fprintf(out, "no %s found\n", kind)
				return nil
			}
			fmt.Fprint(out, t.Render())
			if anyInvalid {
				fmt.Fprintln(out, "\nSome skills are not Valid=True. Run `oap skill show <name>"+clusterFlagHint(cluster)+"` for the reason.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&cluster, "cluster", false, "List cluster-scoped ClusterSkills instead of namespaced Skills")
	return cmd
}

// clusterFlagHint returns " --cluster" when cluster is true, for help messages.
func clusterFlagHint(cluster bool) string {
	if cluster {
		return " --cluster"
	}
	return ""
}
