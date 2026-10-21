package agentcmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/identitycmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup"
)

func newAgentSetupIdentityCmd(g *apcmd.Globals) *cobra.Command {
	var (
		identity string
		only     string
		refresh  string
		force    bool
		opts     identitycmd.SetupOptions
	)
	cmd := &cobra.Command{
		Use:   "setup-identity <agentclass>",
		Short: "Walk through every credential the AgentClass needs",
		Long: `Read the AgentClass spec; for each ToolBundle.toolspecs[*] and
MCPServers[*].ref, look up its credential requirements and run the
appropriate provider flow. Skips credentials that are already set up
unless --force.

If the AgentIdentity (spec.agentIdentity, or --identity override) does
not exist, prompts to create it.

--only <prefix>:<name>    run setup for one entity only (e.g. --only mcp:linear)
--refresh <credential>    re-run setup for just one credential by name, the name
                          shown in the "already set up: <name>" line, forcing past
                          the idempotency check (e.g. --refresh tailscale-authkey);
                          shows a "did you mean?" on an unknown name
--identity <name>         override AgentClass.spec.agentIdentity
--force                   re-run flows even when the credential is already set up`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			opts.NoColor = g.NoColor
			return runAgentSetupIdentity(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
				os.Stdin, b.Controller, b.Namespace, args[0], identity, only, refresh, force, opts)
		},
	}
	cmd.Flags().StringVar(&identity, "identity", "", "Override AgentClass.spec.agentIdentity")
	cmd.Flags().StringVar(&only, "only", "", "Run setup for one entity only (e.g. mcp:linear)")
	// --refresh selects by CREDENTIAL name (the name the user sees in the
	// "already set up: <name>" line), not by target entity like --only; it is the
	// easy path for "my tailscale key expired, re-do just that one". Registered
	// here rather than in AddSetupFlags for the same reason --only is agent-form
	// only: the no-class 'oap identity setup' already filters by --toolkits/--mcps.
	cmd.Flags().StringVar(&refresh, "refresh", "",
		"Re-run setup for just one credential by name (the name shown in 'already set up: <name>'), "+
			"forcing past the idempotency check (e.g. --refresh tailscale-authkey)")
	identitycmd.AddSetupFlags(cmd, &opts, &force)
	return cmd
}

func runAgentSetupIdentity(
	ctx context.Context, out, errOut io.Writer, stdin io.Reader,
	c client.Client, namespace, className, identityOverride, only, refresh string, force bool,
	opts identitycmd.SetupOptions,
) error {
	var ac spiceboxv1alpha1.AgentClass
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: className}, &ac); err != nil {
		return fmt.Errorf("get AgentClass %q: %w", className, err)
	}
	identityName := identityOverride
	if identityName == "" {
		identityName = ac.Spec.AgentIdentity
	}
	if identityName == "" {
		return fmt.Errorf("AgentClass %q has no spec.agentIdentity and --identity not given", className)
	}

	// Build target list from the class.
	var targets []string
	for _, tb := range ac.Spec.ToolBundles {
		for _, ts := range tb.Toolspecs {
			targets = append(targets, "toolspec:"+ts)
		}
	}
	for _, mref := range ac.Spec.MCPServers {
		targets = append(targets, "mcp:"+mref.Ref)
	}
	if len(targets) == 0 {
		fmt.Fprintf(out, "AgentClass %q has no toolspecs or MCP servers; nothing to set up.\n", className)
		return nil
	}

	intent := ac.Spec.Description
	return identitycmd.RunSetup(ctx, c, out, errOut, stdin, opts, setup.RunRequest{
		Namespace:         namespace,
		IdentityName:      identityName,
		UserIntent:        intent,
		Targets:           targets,
		OnlyMatch:         only,
		Force:             force,
		RefreshCredential: refresh,
	})
}
