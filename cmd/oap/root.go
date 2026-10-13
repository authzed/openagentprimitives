package main

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/agentcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/artifactcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/auditcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/channelcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/classcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/clilogin"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktopcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/directorycmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/identitycmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/installcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kgcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/memorycmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/pincmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/plangatecmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/preferencescmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/sandboxcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/sessioncmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/settingscmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/setupmcpcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/skillcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/toolscmd"
)

// Globals is the persistent-flag set every subcommand closes over. It is
// defined in apcmd so the command-family packages under cmd/oap/internal can
// take one — a subpackage cannot import package main. The alias lets the root
// tree keep its own short spelling.
type Globals = apcmd.Globals

// NewRootCmd builds the top-level oap command tree.
func NewRootCmd() *cobra.Command {
	return NewRootCmdWithGlobals(&Globals{})
}

// NewRootCmdWithGlobals builds the top-level oap command tree over a
// caller-supplied Globals. This is the smallest seam a test needs to set
// Globals.BundleFn before Execute — production code and every other test
// keeps using NewRootCmd(), which just calls this with a fresh Globals.
func NewRootCmdWithGlobals(g *Globals) *cobra.Command {
	root := &cobra.Command{
		Use:           "oap",
		Short:         "agent-primitives CLI: install, manage, and run agents",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVarP(&g.Namespace, "namespace", "n", "", "Kubernetes namespace (default: kubeconfig context's namespace, falling back to 'default')")
	pf.StringVar(&g.Kubeconfig, "kubeconfig", "", "Path to kubeconfig file (default: $KUBECONFIG or ~/.kube/config)")
	pf.StringVar(&g.Context, "context", "", "Kubeconfig context to use (default: current-context)")
	pf.BoolVar(&g.NoColor, "no-color", false, "Disable terminal colors (also auto-disabled when stdout isn't a TTY or NO_COLOR is set)")
	root.AddCommand(installcmd.NewInstallCmd(g))
	root.AddCommand(installcmd.NewCheckCmd(g))
	root.AddCommand(installcmd.NewBuildCmd(g))
	root.AddCommand(installcmd.NewImageCmd(g))
	root.AddCommand(installcmd.NewInitCmd(g))
	root.AddCommand(installcmd.NewCleanCmd(g))
	root.AddCommand(toolscmd.NewCmd(g))
	root.AddCommand(identitycmd.NewCmd(g))
	root.AddCommand(identitycmd.NewUserIdentityCmd(g))
	root.AddCommand(agentcmd.NewCmd(g))
	root.AddCommand(sessioncmd.NewCmd(g))
	root.AddCommand(sandboxcmd.NewCmd(g))
	root.AddCommand(classcmd.NewCmd(g))
	root.AddCommand(skillcmd.NewCmd(g))
	root.AddCommand(artifactcmd.NewCmd(g))
	root.AddCommand(installcmd.NewSpiceDBCmd(g))
	root.AddCommand(installcmd.NewPlatformCmd(g))
	root.AddCommand(channelcmd.NewCmd(g))
	root.AddCommand(directorycmd.NewCmd(g))
	root.AddCommand(settingscmd.NewCmd(g))
	root.AddCommand(memorycmd.NewCmd(g))
	root.AddCommand(auditcmd.NewCmd(g))
	root.AddCommand(kgcmd.NewCmd(g))
	root.AddCommand(pincmd.NewCmd(g))
	root.AddCommand(plangatecmd.NewCmd(g))
	root.AddCommand(preferencescmd.NewCmd(g))
	root.AddCommand(clilogin.NewLoginCmd(g))
	root.AddCommand(clilogin.NewLogoutCmd(g))
	root.AddCommand(identitycmd.NewIdpCmd(g))
	root.AddCommand(desktopcmd.NewCmd(g))
	root.AddCommand(desktopcmd.NewWindowCmd(g))
	root.AddCommand(setupmcpcmd.NewCmd(g))
	return root
}
