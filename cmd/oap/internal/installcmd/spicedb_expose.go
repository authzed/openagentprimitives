package installcmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

const (
	spicedbExposeServiceName = "svc/spicebox-spicedb"
	spicedbExposeDefaultPort = 50051
)

// exposeKubectlRunner is a package-level seam so tests can assert the
// constructed kubectl argv without spawning a real kubectl process.
var exposeKubectlRunner = defaultExposeKubectlRunner

func newSpiceDBExposeCmd(g *apcmd.Globals) *cobra.Command {
	localPort := spicedbExposeDefaultPort
	remotePort := spicedbExposeDefaultPort

	cmd := &cobra.Command{
		Use:   "expose",
		Short: "Port-forward the in-cluster SpiceDB Service to localhost",
		Long: `Wraps kubectl port-forward against the spicebox-spicedb Service. It lives in
the platform's system namespace, which is where this looks unless -n says
otherwise. Useful for pointing an external SpiceDB client at the operator's
SpiceDB.

Foreground process; Ctrl-C kills the forward.

Example: in another terminal, run
  SPICEDB_ENDPOINT=localhost:50051 ./run.sh
in a client checkout to write permissions into the same SpiceDB this
operator manages.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			argv := buildSpiceDBExposeKubectlArgv(exposeNamespace(g), localPort, remotePort)
			return exposeKubectlRunner(argv)
		},
	}
	cmd.Flags().IntVar(&localPort, "local-port",
		spicedbExposeDefaultPort, "Local port to forward from")
	cmd.Flags().IntVar(&remotePort, "remote-port",
		spicedbExposeDefaultPort, "Remote port on the SpiceDB Service (50051 = gRPC, 8443 = HTTP)")
	return cmd
}

// exposeNamespace resolves where to look for the SpiceDB Service: the global
// -n when the user set it, otherwise the system namespace oap install puts it
// in. The fallback is deliberately NOT the kubeconfig context's namespace —
// SpiceDB is a platform workload, and defaulting to whatever namespace the
// user happens to be sitting in would fail for every user who never runs with
// their context pointed at the system namespace.
func exposeNamespace(g *apcmd.Globals) string {
	if g.Namespace != "" {
		return g.Namespace
	}
	return apcmd.SystemNamespace
}

// buildSpiceDBExposeKubectlArgv builds the kubectl port-forward argv. Pure
// function for testing.
func buildSpiceDBExposeKubectlArgv(namespace string, localPort, remotePort int) []string {
	return []string{
		"port-forward",
		"-n", namespace,
		spicedbExposeServiceName,
		fmt.Sprintf("%d:%d", localPort, remotePort),
	}
}

func defaultExposeKubectlRunner(args []string) error {
	c := exec.Command("kubectl", args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("oap spicedb expose: %w", err)
	}
	return nil
}
