package toolscmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/uihelpers"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newToolkitShowCmd(g *apcmd.Globals) *cobra.Command {
	return apcmd.NewShowCmd(g, "Show a SpiceboxToolkit's binary target, subcommands, and validation status",
		false, func() *spiceboxv1alpha1.SpiceboxToolkit { return &spiceboxv1alpha1.SpiceboxToolkit{} },
		renderToolkit)
}

func renderToolkit(out io.Writer, tk *spiceboxv1alpha1.SpiceboxToolkit) {
	fmt.Fprintf(out, "Name:     %s\n", tk.Name)
	if tk.Spec.Version != "" {
		fmt.Fprintf(out, "Version:  %s\n", tk.Spec.Version)
	}
	fmt.Fprintf(out, "Revision: %s\n", tk.Spec.ToolkitRevision)
	fmt.Fprintf(out, "Binary:   %s\n", tk.Spec.Target.Binary)
	if tk.Spec.Target.VersionRange != "" {
		fmt.Fprintf(out, "Version range: %s\n", tk.Spec.Target.VersionRange)
	}
	fmt.Fprintf(out, "Age:      %s\n", apcmd.DurationSinceShort(tk.CreationTimestamp.Time))

	if tk.Spec.Docs != "" {
		fmt.Fprintf(out, "Docs:     %s\n", tk.Spec.Docs)
	}

	fmt.Fprintf(out, "Subcommands (%d):\n", len(tk.Spec.Subcommands))
	for _, s := range tk.Spec.Subcommands {
		path := strings.Join(s.Path, " ")
		if s.Description != "" {
			fmt.Fprintf(out, "  - %-20s — %s\n", path, s.Description)
		} else {
			fmt.Fprintf(out, "  - %s\n", path)
		}
	}

	if len(tk.Spec.GlobalFlags) > 0 {
		fmt.Fprintf(out, "GlobalFlags (%d): (use kubectl get spiceboxtoolkit %s -o yaml for full schema)\n",
			len(tk.Spec.GlobalFlags), tk.Name)
	}

	fmt.Fprintln(out, "Status:")
	fmt.Fprintln(out, "  Conditions:")
	uihelpers.PrintConditions(out, tk.Status.Conditions, "    ")
}
