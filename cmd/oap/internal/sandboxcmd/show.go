package sandboxcmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/uihelpers"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newSandboxShowCmd(g *apcmd.Globals) *cobra.Command {
	return apcmd.NewShowCmd(g, "Show a SpiceboxSession's spec, resolved class snapshot, pod, and conditions",
		true, func() *spiceboxv1alpha1.SpiceboxSession { return &spiceboxv1alpha1.SpiceboxSession{} },
		renderSandboxSession)
}

func renderSandboxSession(out io.Writer, s *spiceboxv1alpha1.SpiceboxSession) {
	fmt.Fprintf(out, "Name:    %s\n", s.Name)
	fmt.Fprintf(out, "Class:   %s\n", s.Spec.Class)
	if s.Spec.Agent != "" {
		fmt.Fprintf(out, "Agent:   %s\n", s.Spec.Agent)
	}
	if s.Spec.IdleTTL != nil {
		fmt.Fprintf(out, "IdleTTL: %s\n", s.Spec.IdleTTL.Duration)
	}
	if s.Spec.MaxDuration != nil {
		fmt.Fprintf(out, "MaxDur:  %s\n", s.Spec.MaxDuration.Duration)
	}
	fmt.Fprintf(out, "Age:     %s\n", apcmd.DurationSinceShort(s.CreationTimestamp.Time))

	if len(s.Spec.Toolspecs) > 0 {
		fmt.Fprintln(out, "Spec.Toolspecs (narrowing):")
		for _, ts := range s.Spec.Toolspecs {
			fmt.Fprintf(out, "  - %s\n", ts.Name)
		}
	}

	fmt.Fprintln(out, "Status:")
	// status.sandbox is unset only for a session created before this seam
	// existed; PodName is kept as its fallback (see sandbox_list.go's
	// sandboxCell for the same rule applied to the list table).
	if h := s.Status.Sandbox; h != nil {
		fmt.Fprintf(out, "  Sandbox:       %s (%s)\n", h.Ref, h.Kind)
		if h.Prewarmed {
			fmt.Fprintln(out, "  Prewarmed:     true")
		}
	} else if s.Status.PodName != "" {
		fmt.Fprintf(out, "  Pod:           %s\n", s.Status.PodName)
	}
	fmt.Fprintf(out, "  CallCount:     %d\n", s.Status.CallCount)
	if s.Status.LastActivityAt != nil {
		fmt.Fprintf(out, "  LastActivity:  %s\n", s.Status.LastActivityAt.Time.Format("15:04:05"))
	}
	if s.Status.ResolvedAgent != "" {
		fmt.Fprintf(out, "  ResolvedAgent: %s\n", s.Status.ResolvedAgent)
	}
	if len(s.Status.EffectiveToolspecs) > 0 {
		fmt.Fprintln(out, "  EffectiveToolspecs:")
		for _, ts := range s.Status.EffectiveToolspecs {
			fmt.Fprintf(out, "    - %s\n", ts)
		}
	}
	if s.Status.ResolvedClass != nil && len(s.Status.ResolvedClass.Tools) > 0 {
		fmt.Fprintln(out, "  ResolvedClass.Tools (frozen at bind time):")
		for _, tl := range s.Status.ResolvedClass.Tools {
			fmt.Fprintf(out, "    - %s\n", tl.Name)
		}
	}
	fmt.Fprintln(out, "  Conditions:")
	uihelpers.PrintConditions(out, s.Status.Conditions, "    ")

	fmt.Fprintln(out, "\nSee ToolCalls for this session:")
	fmt.Fprintf(out, "  oap tools toolcall list --session %s\n", s.Name)
}
