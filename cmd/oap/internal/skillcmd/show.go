package skillcmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newSkillShowCmd(g *apcmd.Globals) *cobra.Command {
	var cluster bool
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a Skill's canonical name, description, body, provenance, and status",
		Long:  "<name> is the Kubernetes object name (as shown in `oap skill list`), not the canonical name.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if cluster {
				var s spiceboxv1alpha1.ClusterSkill
				if err := b.Controller.Get(cmd.Context(), client.ObjectKey{Name: args[0]}, &s); err != nil {
					return err
				}
				renderSkill(out, s.Name, "", &s.Spec, s.Status.Conditions, skillPinStrength(s.Status.Pin))
			} else {
				var s spiceboxv1alpha1.Skill
				if err := b.Controller.Get(cmd.Context(), client.ObjectKey{Namespace: b.Namespace, Name: args[0]}, &s); err != nil {
					return err
				}
				renderSkill(out, s.Name, b.Namespace, &s.Spec, s.Status.Conditions, skillPinStrength(s.Status.Pin))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&cluster, "cluster", false, "Target a cluster-scoped ClusterSkill")
	return cmd
}

// skillPinStrength reads the syntactic pin strength off a skill's PinRecord.
// Empty when no record is stamped, which is the case for a canonical name that
// did not parse -- the Valid condition carries the reason.
func skillPinStrength(pin *spiceboxv1alpha1.PinRecord) string {
	if pin == nil {
		return ""
	}
	return pin.Strength
}

func renderSkill(out io.Writer, name, namespace string, spec *spiceboxv1alpha1.SkillSpec, conds []metav1.Condition, pinStrength string) {
	fmt.Fprintf(out, "Name:           %s\n", name)
	if namespace != "" {
		fmt.Fprintf(out, "Namespace:      %s\n", namespace)
	}
	fmt.Fprintf(out, "Canonical name: %s\n", spec.CanonicalName)
	if spec.DisplayName != "" {
		fmt.Fprintf(out, "Display name:   %s\n", spec.DisplayName)
	}
	if pinStrength != "" {
		fmt.Fprintf(out, "Pin strength:   %s\n", pinStrength)
	}
	fmt.Fprintf(out, "Description:    %s\n", spec.Description)

	if spec.Source != nil {
		fmt.Fprintln(out, "Source:")
		fmt.Fprintf(out, "  repo:        %s\n", spec.Source.RepoLocator)
		if spec.Source.Subpath != "" {
			fmt.Fprintf(out, "  subpath:     %s\n", spec.Source.Subpath)
		}
		if spec.Source.Ref != "" {
			fmt.Fprintf(out, "  ref:         %s\n", spec.Source.Ref)
		}
		if spec.Source.ResolvedSHA != "" {
			fmt.Fprintf(out, "  resolvedSHA: %s\n", spec.Source.ResolvedSHA)
		}
		if spec.Source.SourceName != "" {
			fmt.Fprintf(out, "  sourceName:  %s\n", spec.Source.SourceName)
		}
	} else {
		fmt.Fprintln(out, "Source:         (hand-authored)")
	}

	if spec.Bundle != nil {
		fmt.Fprintf(out, "Bundle:         digest=%s (executable scripts/assets)\n", spec.Bundle.Digest)
	} else {
		fmt.Fprintln(out, "Bundle:         (instruction-only — no scripts/assets)")
	}

	printConditionsBlock(out, conds)

	fmt.Fprintln(out, "Body (SKILL.md):")
	fmt.Fprintln(out, "----------------------------------------")
	fmt.Fprintln(out, spec.Body)
	fmt.Fprintln(out, "----------------------------------------")
}
