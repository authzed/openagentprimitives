package identitycmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup"
)

func newIdentitySetupCmd(g *apcmd.Globals) *cobra.Command {
	var (
		toolkits []string
		mcps     []string
		force    bool
		opts     SetupOptions
	)
	cmd := &cobra.Command{
		Use:   "setup <identity-name>",
		Short: "Walk through credentials for an explicit set of toolkits + MCPs",
		Long: `Identity-rooted setup (no AgentClass context). Requires at least
one of --toolkits or --mcps. Each entry is the resource name (e.g.
gh, kubectl, linear-readonly).

This is the experimentation path; for production agents you typically
want 'oap agent setup-identity <classname>' which derives intent from
the AgentClass's Toolspec/MCP graph.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			opts.NoColor = g.NoColor
			return runIdentitySetup(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
				os.Stdin, b.Controller, b.Namespace, args[0], toolkits, mcps, force, opts)
		},
	}
	cmd.Flags().StringSliceVar(&toolkits, "toolkits", nil, "Comma-separated toolkit names (e.g. gh,kubectl)")
	cmd.Flags().StringSliceVar(&mcps, "mcps", nil, "Comma-separated MCPServer names")
	AddSetupFlags(cmd, &opts, &force)
	return cmd
}

// AddSetupFlags declares the flags that govern how a setup run is
// presented, on whichever command is rooting it. Declared once so `oap identity
// setup` and `oap agent setup-identity` cannot describe the same flag two
// different ways.
//
// force is a separate pointer rather than a SetupOptions field because it
// governs what the run DOES (re-run a flow whose credential already exists),
// not how it is presented, and both run functions take it as a parameter.
func AddSetupFlags(cmd *cobra.Command, opts *SetupOptions, force *bool) {
	cmd.Flags().BoolVar(&opts.nonInteractive, "non-interactive", false,
		"Never prompt: verify credentials that are already provisioned and fail on any that are not")
	cmd.Flags().BoolVar(force, "force", false, "Re-run flows even when credential is already set up")
}

func runIdentitySetup(
	ctx context.Context, out, errOut io.Writer, stdin io.Reader,
	c client.Client, namespace, identityName string,
	toolkits, mcps []string, force bool, opts SetupOptions,
) error {
	if len(toolkits) == 0 && len(mcps) == 0 {
		return fmt.Errorf("setup requires --toolkits and/or --mcps; or root the command at an AgentClass via 'oap agent setup-identity <classname>'")
	}
	var targets []string
	for _, t := range toolkits {
		targets = append(targets, "cli:"+strings.TrimSpace(t))
	}
	for _, m := range mcps {
		targets = append(targets, "mcp:"+strings.TrimSpace(m))
	}
	// OnlyMatch is intentionally not exposed on the no-class form: --toolkits
	// and --mcps already act as the filter. The class-rooted form needs --only
	// because it derives its target list from the AgentClass spec rather than
	// from CLI flags.
	return RunSetup(ctx, c, out, errOut, stdin, opts, setup.RunRequest{
		Namespace:    namespace,
		IdentityName: identityName,
		Targets:      targets,
		Force:        force,
	})
}

// RunSetup is the presentation lifecycle both identity commands share: measure
// the terminal, build the theme from it, and hand the engine a presenter that
// runs each flow's screens, commits its result and summarizes it.
//
// Capabilities come from the writer the command was handed rather than from
// os.Stdout: a test or a shell pipeline supplies a plain io.Writer, and taking
// over a screen the command is not actually writing to would leave the wizard
// invisible. In production the two are the same file.
//
// It is the ONLY place the request's streams and its presenter are wired, which
// is what keeps the two agreeing — see RunRequest.Present on why they must.
func RunSetup(
	ctx context.Context, c client.Client,
	out, errOut io.Writer, stdin io.Reader,
	opts SetupOptions, req setup.RunRequest,
) error {
	theme := tui.NewTheme(apcmd.DetectCaps(out, opts.NoColor))
	req.Stdin = stdin
	req.Stdout = out
	req.Stderr = errOut
	req.Theme = theme
	req.NonInteractive = opts.nonInteractive
	req.Present = newIdentityFlowPresenter(stdin, out, errOut, theme, opts)
	return setup.Run(ctx, c, req)
}
