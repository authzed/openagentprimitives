package toolscmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/toolscli"
)

// editKubectlRunner is a package-level seam used by tests to assert kubectl
// argument construction without spawning a real kubectl process.
var editKubectlRunner = defaultEditKubectlRunner

func newToolsEditCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "edit <name>",
		Short: "Open the named tool resource in $EDITOR via kubectl edit",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, ns, err := toolsListClientFactory(g)
			if err != nil {
				return err
			}
			m, err := toolscli.Resolve(cmd.Context(), c, ns, args[0])
			if err != nil {
				return err
			}
			gvr := m.Kind.GVR()
			resourceArg := fmt.Sprintf("%s.%s/%s", gvr.Resource, gvr.Group, args[0])
			kubectlArgs := buildKubectlEditArgs(resourceArg, m.Obj.GetNamespace(), g)
			return editKubectlRunner(kubectlArgs)
		},
	}
	return cmd
}

// buildKubectlEditArgs constructs the argv for `kubectl edit ...`, including
// --kubeconfig / --context / -n flags from the global flags.
func buildKubectlEditArgs(resourceArg, namespace string, g *apcmd.Globals) []string {
	args := []string{"edit", resourceArg}
	if g.Kubeconfig != "" {
		args = append(args, "--kubeconfig", g.Kubeconfig)
	}
	if g.Context != "" {
		args = append(args, "--context", g.Context)
	}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	return args
}

func defaultEditKubectlRunner(args []string) error {
	c := exec.Command("kubectl", args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
