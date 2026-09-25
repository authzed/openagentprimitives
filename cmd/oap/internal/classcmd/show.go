package classcmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/uihelpers"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newClassShowCmd(g *apcmd.Globals) *cobra.Command {
	return apcmd.NewShowCmd(g, "Show a SpiceboxClass's spec, validation status, and toolspec coverage",
		false, func() *spiceboxv1alpha1.SpiceboxClass { return &spiceboxv1alpha1.SpiceboxClass{} },
		renderClass)
}

func renderClass(out io.Writer, c *spiceboxv1alpha1.SpiceboxClass) {
	fmt.Fprintf(out, "Name:    %s\n", c.Name)
	fmt.Fprintf(out, "Image:   %s\n", c.Spec.Image)
	if c.Spec.RuntimeClassName != nil {
		fmt.Fprintf(out, "Runtime: %s\n", *c.Spec.RuntimeClassName)
	}
	fmt.Fprintf(out, "Age:     %s\n", apcmd.DurationSinceShort(c.CreationTimestamp.Time))

	fmt.Fprintln(out, "Resources:")
	fmt.Fprintf(out, "  cpu:              %s\n", c.Spec.Resources.CPU.String())
	fmt.Fprintf(out, "  memory:           %s\n", c.Spec.Resources.Memory.String())
	if !c.Spec.Resources.EphemeralStorage.IsZero() {
		fmt.Fprintf(out, "  ephemeralStorage: %s\n", c.Spec.Resources.EphemeralStorage.String())
	}
	if c.Spec.Resources.PidsLimit > 0 {
		fmt.Fprintf(out, "  pidsLimit:        %d\n", c.Spec.Resources.PidsLimit)
	}

	if c.Spec.Network.Mode != "" {
		fmt.Fprintln(out, "Network:")
		fmt.Fprintf(out, "  mode: %s\n", c.Spec.Network.Mode)
		if len(c.Spec.Network.AllowedHosts) > 0 {
			fmt.Fprintln(out, "  allowedHosts:")
			for _, h := range c.Spec.Network.AllowedHosts {
				fmt.Fprintf(out, "    - %s\n", h)
			}
		}
	}

	if len(c.Spec.Tools) > 0 {
		fmt.Fprintln(out, "Tools:")
		for _, tl := range c.Spec.Tools {
			fmt.Fprintf(out, "  - name: %s\n", tl.Name)
			if len(tl.Command) > 0 {
				fmt.Fprintf(out, "    command: %v\n", tl.Command)
			}
			if tl.ExpectedDuration != "" {
				fmt.Fprintf(out, "    expectedDuration: %s\n", tl.ExpectedDuration)
			}
		}
	}

	if len(c.Spec.Toolspecs) > 0 {
		fmt.Fprintln(out, "Toolspecs:")
		for _, ts := range c.Spec.Toolspecs {
			fmt.Fprintf(out, "  - %s\n", ts.Name)
		}
	}

	if c.Spec.SessionDefaults.IdleTTL.Duration > 0 || c.Spec.SessionDefaults.MaxDuration.Duration > 0 {
		fmt.Fprintln(out, "SessionDefaults:")
		if c.Spec.SessionDefaults.IdleTTL.Duration > 0 {
			fmt.Fprintf(out, "  idleTTL:     %s\n", c.Spec.SessionDefaults.IdleTTL.Duration)
		}
		if c.Spec.SessionDefaults.MaxDuration.Duration > 0 {
			fmt.Fprintf(out, "  maxDuration: %s\n", c.Spec.SessionDefaults.MaxDuration.Duration)
		}
	}

	fmt.Fprintln(out, "Status:")
	if len(c.Status.ToolspecCoverage) > 0 {
		fmt.Fprintln(out, "  ToolspecCoverage:")
		for tool, specs := range c.Status.ToolspecCoverage {
			fmt.Fprintf(out, "    %s: %v\n", tool, specs)
		}
	}
	fmt.Fprintln(out, "  Conditions:")
	uihelpers.PrintConditions(out, c.Status.Conditions, "    ")
}
