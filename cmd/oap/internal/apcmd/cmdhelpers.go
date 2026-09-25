package apcmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// NewNamespacedDeleteCmd builds a "delete <name>" cobra command that deletes a
// single namespaced CR of type T from the Globals' namespace and prints
// "deleted <noun> <ns>/<name>". noun is the human-facing kind name used in the
// success line (e.g. "AgentClass"). newObj returns a fresh, empty T; the
// command sets its Namespace and Name before deleting.
func NewNamespacedDeleteCmd[T client.Object](g *Globals, short, noun string, newObj func() T) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			obj := newObj()
			obj.SetNamespace(b.Namespace)
			obj.SetName(args[0])
			if err := b.Controller.Delete(cmd.Context(), obj); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %s %s/%s\n", noun, b.Namespace, args[0])
			return nil
		},
	}
}

// NewApplyCmd builds an "apply <path>" cobra command that reads a YAML file,
// splits it into documents, and server-side applies each via the dynamic
// client, printing "applied <kind>/<name>" per document. short is the only
// per-command variation.
func NewApplyCmd(g *Globals, short string) *cobra.Command {
	return &cobra.Command{
		Use:   "apply <path>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			docs, err := manifests.Split(data)
			if err != nil {
				return err
			}
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			for _, d := range docs {
				if err := kube.Apply(cmd.Context(), b.Dynamic, d, "ap-apply"); err != nil {
					return fmt.Errorf("apply %s/%s: %w", d.GetKind(), d.GetName(), err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "applied %s/%s\n", d.GetKind(), d.GetName())
			}
			return nil
		},
	}
}

// NewShowCmd builds a "show <name>" cobra command that fetches a single CR of
// type T and renders it via the supplied render closure. When namespaced is
// true the object is looked up in the Globals' namespace; otherwise it is
// looked up cluster-scoped (name only). newObj returns a fresh, empty T to
// decode into; render writes the human-facing output.
func NewShowCmd[T client.Object](g *Globals, short string, namespaced bool, newObj func() T, render func(io.Writer, T)) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			key := client.ObjectKey{Name: args[0]}
			if namespaced {
				key.Namespace = b.Namespace
			}
			obj := newObj()
			if err := b.Controller.Get(cmd.Context(), key, obj); err != nil {
				return err
			}
			render(cmd.OutOrStdout(), obj)
			return nil
		},
	}
}
