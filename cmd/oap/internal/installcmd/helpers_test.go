package installcmd

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// newRoot assembles this package's commands under a root carrying the same
// persistent flags cmd/oap's does, so a test can run `install …`, `init …`,
// `build …` and friends exactly as a user types them.
//
// These are top-level commands rather than one subtree, so there is no single
// NewCmd to drive; the root here stands in for cmd/oap's. It deliberately
// carries only this package's commands — a test that reached for another
// family's would be asserting about wiring, which belongs in cmd/oap.
func newRoot(t *testing.T) *cobra.Command {
	t.Helper()
	g := &apcmd.Globals{}
	root := &cobra.Command{
		Use:           "oap",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVarP(&g.Namespace, "namespace", "n", "", "Kubernetes namespace")
	pf.StringVar(&g.Kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	pf.StringVar(&g.Context, "context", "", "Kubeconfig context to use")
	pf.BoolVar(&g.NoColor, "no-color", false, "Disable terminal colors")
	for _, c := range []*cobra.Command{
		NewInstallCmd(g),
		NewCheckCmd(g),
		NewBuildCmd(g),
		NewImageCmd(g),
		NewInitCmd(g),
		NewCleanCmd(g),
		NewSpiceDBCmd(g),
		NewPlatformCmd(g),
	} {
		root.AddCommand(c)
	}
	return root
}
