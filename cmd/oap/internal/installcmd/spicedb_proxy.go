package installcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/portforward"
	apspicedb "github.com/authzed/openagentprimitives/cmd/oap/internal/spicedb"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/zedctx"
)

const (
	defaultProxyLocalPort = 60061
	defaultProxyContext   = "agentprimitives"
)

// proxyForwarder is the subset of *portforward.PortForwarder that
// runSpiceDBProxy depends on. Lets tests inject a fake.
type proxyForwarder interface {
	Start(ctx context.Context, out io.Writer) error
	LocalPort() uint16
	Done() <-chan error
	Stop()
}

// proxyZedRunner is the subset of *zedctx.Runner that runSpiceDBProxy uses.
type proxyZedRunner interface {
	Available() bool
	Set(ctx context.Context, name, endpoint, token string, insecure bool) error
	Use(ctx context.Context, name string) error
	Remove(ctx context.Context, name string) error
}

// runSpiceDBProxyDeps captures injectable dependencies so we can unit-test
// the orchestrator without real Kubernetes or a real zed binary.
type runSpiceDBProxyDeps struct {
	Namespace   string
	ContextName string
	LocalPort   uint16
	Typed       kubernetes.Interface
	Forwarder   proxyForwarder
	Zed         proxyZedRunner
}

func newSpiceDBProxyCmd(g *apcmd.Globals) *cobra.Command {
	port := uint16(defaultProxyLocalPort)
	contextName := defaultProxyContext

	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "Port-forward the in-cluster SpiceDB and configure a local zed context.",
		Long: `Opens a long-running port-forward from 127.0.0.1:<port> to the
spicebox-spicedb Service, writes a zed context (default name "agentprimitives")
pointing at it, and switches to that context. Blocks until Ctrl-C / SIGTERM,
then removes the zed context entry and tears down the forward.

After it's running, in another terminal:

    zed schema read
    zed relationship read agentsession:abc
    zed permission check agentsession:abc interact user:alice
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			ns := g.Namespace
			if ns == "" {
				ns = "agentprimitives-system"
			}
			pf, err := portforward.New(b.REST, ns, apspicedb.ServiceName, apspicedb.Selector, apspicedb.TargetPort, port)
			if err != nil {
				return err
			}
			runner := zedctx.NewRunner()
			return runSpiceDBProxy(cmd.Context(), cmd.OutOrStdout(), runSpiceDBProxyDeps{
				Namespace:   ns,
				ContextName: contextName,
				LocalPort:   port,
				Typed:       b.Typed,
				Forwarder:   pf,
				Zed:         runner,
			})
		},
	}
	cmd.Flags().Uint16Var(&port, "port", defaultProxyLocalPort, "Local TCP port to bind")
	cmd.Flags().StringVar(&contextName, "zed-context", defaultProxyContext, "zed context name to create / use")
	return cmd
}

// runSpiceDBProxy is the testable core of `oap spicedb proxy`. It:
//  1. reads the SpiceDB token Secret,
//  2. starts the port-forward,
//  3. configures the zed context (set + use),
//  4. blocks on ctx.Done() or SIGINT / SIGTERM,
//  5. removes the zed context (best-effort) and tears down the forward.
func runSpiceDBProxy(ctx context.Context, out io.Writer, d runSpiceDBProxyDeps) error {
	// 1. Read token secret.
	sec, err := d.Typed.CoreV1().Secrets(d.Namespace).Get(ctx, apspicedb.TokenSecret, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("read spicedb token secret: not found in %s", d.Namespace)
		}
		return fmt.Errorf("read spicedb token secret: %w", err)
	}
	token := string(sec.Data[apspicedb.TokenKey])
	if token == "" {
		return fmt.Errorf("spicedb token secret %s/%s has empty token key", d.Namespace, apspicedb.TokenSecret)
	}

	// 2. Wrap ctx so Ctrl-C / SIGTERM cancels the wait below.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 3. Start port-forward.
	if err := d.Forwarder.Start(ctx, io.Discard); err != nil {
		return fmt.Errorf("start port-forward: %w", err)
	}
	defer d.Forwarder.Stop()
	// Safe to skip Stop on Start error: portforward.Start closes its own stopCh on every error path.

	localPort := d.Forwarder.LocalPort()
	endpoint := fmt.Sprintf("127.0.0.1:%d", localPort)
	fmt.Fprintf(out, "==> port-forward %s -> %s/grpc (%s)\n", endpoint, apspicedb.ServiceName, d.Namespace)

	// 4. Configure zed context, if zed is available. If zed is missing, surface
	//    a copy-pasteable command and keep running — the port-forward alone is
	//    still useful.
	if !d.Zed.Available() {
		// The command is printed with a placeholder, never the live key. This
		// branch is the common case on a fresh workstation, and the token is
		// the cluster's unscoped SpiceDB pre-shared key — printing it drops
		// the root authorization credential into terminal scrollback, CI logs,
		// and any pasted bug report. The hint below keeps the command usable
		// by naming where to read the real value from.
		fmt.Fprintf(out, "warning: zed not found on PATH — port-forward is live but no context was written.\n")
		fmt.Fprintf(out, "    zed context set %s %s '<preshared-key>' --insecure\n", d.ContextName, endpoint)
		fmt.Fprintf(out, "    zed context use %s\n", d.ContextName)
		fmt.Fprintf(out, "    (read <preshared-key> from the %q key of Secret %s/%s)\n",
			apspicedb.TokenKey, d.Namespace, apspicedb.TokenSecret)
	} else {
		if err := d.Zed.Set(ctx, d.ContextName, endpoint, token, true); err != nil {
			return fmt.Errorf("zed context set: %w", err)
		}
		if err := d.Zed.Use(ctx, d.ContextName); err != nil {
			// We've already written the context entry; best-effort cleanup.
			if remErr := d.Zed.Remove(context.Background(), d.ContextName); remErr != nil {
				fmt.Fprintf(out, "warning: failed to remove zed context %q after use failure: %v\n", d.ContextName, remErr)
			}
			return fmt.Errorf("zed context use: %w", err)
		}
		fmt.Fprintf(out, "==> zed context %q set (insecure, token=%s), now active\n", d.ContextName, "********")
		fmt.Fprintln(out, "    zed schema read")
		fmt.Fprintln(out, "    zed relationship read agentsession:abc")
		fmt.Fprintln(out, "    zed permission check agentsession:abc interact user:alice")
	}
	fmt.Fprintln(out, "==> Ctrl-C to stop")

	// 5. Block until the user signals shutdown OR the port-forward stream dies.
	//    A mid-session stream death (apiserver drops the connection, pod
	//    restarts, network blip) is a non-shutdown event we must surface; it's
	//    why proxyForwarder exposes Done() at all.
	var streamErr error
	select {
	case <-ctx.Done():
		fmt.Fprintln(out, "==> stopping port-forward")
	case err := <-d.Forwarder.Done():
		streamErr = err
		if streamErr == nil {
			streamErr = errors.New("port-forward stream closed unexpectedly")
		}
		fmt.Fprintf(out, "==> port-forward stream closed: %v\n", streamErr)
	}

	// 6. Best-effort remove. Use Background ctx — the parent ctx may be canceled.
	if d.Zed.Available() {
		if err := d.Zed.Remove(context.Background(), d.ContextName); err != nil {
			fmt.Fprintf(out, "warning: failed to remove zed context %q: %v\n", d.ContextName, err)
		} else {
			fmt.Fprintf(out, "==> removed zed context %q\n", d.ContextName)
		}
	}

	if streamErr != nil {
		return fmt.Errorf("port-forward died: %w", streamErr)
	}
	return nil
}
