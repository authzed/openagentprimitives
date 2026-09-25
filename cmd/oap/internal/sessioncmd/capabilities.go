package sessioncmd

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// newSessionCapabilitiesCmd shows what a session's tools can actually reach.
//
// The answer comes from the ONE producer that can give it exactly — the runner,
// which holds the live []tool.Tool and publishes the enumerated surface onto
// status at session start. Deriving it here instead (walking AgentClass,
// MCPServer and Toolkit CRs) would be a second derivation that drifts from what
// dispatch actually checks, and "the surface equals what dispatch enforces" is
// the property that makes it worth printing at all.
//
// This is the LIVE surface: exact, and scoped to one session. "What COULD this
// agent do before a session exists?" is a different and privileged question —
// it needs a live MCP probe under an identity — and it is not answered here.
func newSessionCapabilitiesCmd(g *apcmd.Globals) *cobra.Command {
	var showTools bool
	cmd := &cobra.Command{
		Use:   "capabilities <session>",
		Short: "Show the permission classes this session's tools can reach",
		Long: `Show every permission class this AgentSession's tools can reach.

Each row is a handle and the severity of reaching it:

    readonly   perm:read:github_repo
    readwrite  perm:write:tracker_issue
    external   tool:apply_workspace

The severity is the MAX across every tool reaching that handle — a class one
tool reads and another writes is a write class, because the reach is what
matters, not the mildest route to it.

This is a REPORT, not a grant. It says what the tools could reach if authorized;
whether any particular call is allowed is decided at dispatch by the per-tool
check, the plan gate, and the approval flow. A session may hold far less than
this list.

The surface is published by the runner at session start, so a session that has
not started yet — or one whose runner predates this field — shows nothing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			var sess spiceboxv1alpha1.AgentSession
			if err := b.Controller.Get(cmd.Context(),
				client.ObjectKey{Namespace: b.Namespace, Name: args[0]}, &sess); err != nil {
				return err
			}

			return renderPermissionSurface(cmd.OutOrStdout(),
				b.Namespace, args[0], sess.Status.PermissionSurface, showTools)
		},
	}
	cmd.Flags().BoolVar(&showTools, "tools", false,
		"also show which tools reach each handle (the answer to \"why can it do this?\")")
	return cmd
}

// renderPermissionSurface writes the surface listing.
//
// Split from the command so the rendering — which is the part that can
// regress — is testable without a cluster.
func renderPermissionSurface(out io.Writer, ns, name string,
	surface []spiceboxv1alpha1.PermissionSurfaceEntry, showTools bool) error {

	if len(surface) == 0 {
		// Distinguished from "reaches nothing" on purpose. The runner writes no
		// field at all when it has not enumerated, so an operator reading an
		// empty list as "this agent is harmless" would be drawing the opposite
		// conclusion from the one the data supports. Say which it is.
		fmt.Fprintf(out, "no permission surface published for agentsession:%s/%s\n", ns, name)
		fmt.Fprintln(out, "(the runner publishes this at session start; a session that has not started shows nothing)")
		return nil
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, e := range surface {
		if showTools {
			fmt.Fprintf(w, "%s\t%s\t%s\n", e.StateImpact, e.Handle, strings.Join(e.Tools, ", "))
			continue
		}
		fmt.Fprintf(w, "%s\t%s\n", e.StateImpact, e.Handle)
	}
	return w.Flush()
}
