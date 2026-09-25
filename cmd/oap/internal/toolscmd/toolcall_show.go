package toolscmd

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/uihelpers"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newToolcallShowCmd(g *apcmd.Globals) *cobra.Command {
	return apcmd.NewShowCmd(g, "Show a ToolCall's spec, status, conditions, and artifact refs",
		true, func() *spiceboxv1alpha1.ToolCall { return &spiceboxv1alpha1.ToolCall{} },
		renderToolCall)
}

func renderToolCall(out io.Writer, tc *spiceboxv1alpha1.ToolCall) {
	fmt.Fprintf(out, "Name:    %s\n", tc.Name)
	fmt.Fprintf(out, "Session: %s\n", tc.Spec.Session)
	fmt.Fprintf(out, "Tool:    %s\n", tc.Spec.Tool)
	if tc.Spec.Mode != "" {
		fmt.Fprintf(out, "Mode:    %s\n", tc.Spec.Mode)
	}
	if tc.Spec.Timeout.Duration > 0 {
		fmt.Fprintf(out, "Timeout: %s\n", tc.Spec.Timeout.Duration)
	}
	fmt.Fprintf(out, "Age:     %s\n", apcmd.DurationSinceShort(tc.CreationTimestamp.Time))

	if len(tc.Spec.Args) > 0 {
		fmt.Fprintln(out, "Args:")
		for _, a := range tc.Spec.Args {
			fmt.Fprintf(out, "  - %s\n", a)
		}
	}

	if len(tc.Spec.Env) > 0 {
		fmt.Fprintln(out, "Env (non-secret, from spec):")
		keys := make([]string, 0, len(tc.Spec.Env))
		for k := range tc.Spec.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(out, "  %s=%s\n", k, tc.Spec.Env[k])
		}
	}

	if tc.Status.Agent != nil && len(tc.Status.Agent.Injected) > 0 {
		fmt.Fprintln(out, "AgentInjection:")
		for _, inj := range tc.Status.Agent.Injected {
			fmt.Fprintf(out, "  %s=%s\n", inj.Key, inj.Masked)
		}
	}

	if tc.Spec.Stdin != "" {
		preview := tc.Spec.Stdin
		if len(preview) > 200 {
			preview = preview[:200] + "...(truncated)"
		}
		fmt.Fprintf(out, "Stdin (%d bytes):\n  %s\n", len(tc.Spec.Stdin),
			strings.ReplaceAll(preview, "\n", "\n  "))
	}

	fmt.Fprintln(out, "Status:")
	fmt.Fprintf(out, "  Phase: %s\n", apcmd.ToolCallPhase(tc))
	if tc.Status.StartedAt != nil {
		fmt.Fprintf(out, "  StartedAt:  %s\n", tc.Status.StartedAt.Time.Format("15:04:05"))
	}
	if tc.Status.FinishedAt != nil {
		fmt.Fprintf(out, "  FinishedAt: %s\n", tc.Status.FinishedAt.Time.Format("15:04:05"))
	}
	if tc.Status.ExitCode != nil {
		fmt.Fprintf(out, "  ExitCode:   %d\n", *tc.Status.ExitCode)
	}
	if tc.Status.StdoutArtifactRef != "" {
		trunc := ""
		if tc.Status.StdoutTruncated {
			trunc = " (truncated)"
		}
		fmt.Fprintf(out, "  Stdout: %s%s\n", tc.Status.StdoutArtifactRef, trunc)
	}
	if tc.Status.StderrArtifactRef != "" {
		trunc := ""
		if tc.Status.StderrTruncated {
			trunc = " (truncated)"
		}
		fmt.Fprintf(out, "  Stderr: %s%s\n", tc.Status.StderrArtifactRef, trunc)
	}
	if len(tc.Status.OutputArtifacts) > 0 {
		fmt.Fprintln(out, "  OutputArtifacts:")
		for _, a := range tc.Status.OutputArtifacts {
			fmt.Fprintf(out, "    - %s (%s, %d bytes)\n", a.Path, a.ArtifactRef, a.Size)
		}
	}

	fmt.Fprintln(out, "  Conditions:")
	uihelpers.PrintConditions(out, tc.Status.Conditions, "    ")

	if tc.Status.StdoutArtifactRef != "" || tc.Status.StderrArtifactRef != "" {
		fmt.Fprintln(out, "\nFetch output:")
		if tc.Status.StdoutArtifactRef != "" {
			fmt.Fprintf(out, "  oap artifact get %s\n", tc.Status.StdoutArtifactRef)
		}
		if tc.Status.StderrArtifactRef != "" {
			fmt.Fprintf(out, "  oap artifact get %s\n", tc.Status.StderrArtifactRef)
		}
	}
}
