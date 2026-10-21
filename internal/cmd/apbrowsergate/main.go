// Command apbrowsergate is the browser toolbox's gate: the second container in
// a per-session Chrome pod. It runs two listeners — a policy-enforcing forward
// proxy that is Chrome's ONLY egress, and an authenticated reverse proxy to
// Chrome's DevTools that the session runner is the sole client of. It is a
// security boundary: hostname-policy enforcement and per-session token auth both
// live here, so Chrome can reach only policy-approved hosts and nothing can
// drive Chrome's DevTools without the token.
//
// The per-session token is read ONLY from the APBROWSERGATE_TOKEN environment
// variable, never a flag: a flag value is visible in the pod's process list
// (`ps`), and this token is a credential.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/pkg/cli/clikit"
	"github.com/authzed/openagentprimitives/pkg/platform/deplogs"
	"github.com/authzed/openagentprimitives/pkg/tools/browsergate"
)

// tokenEnv names the environment variable carrying the per-session browser
// token. It is deliberately not a flag (see the package comment).
const tokenEnv = "APBROWSERGATE_TOKEN"

// config holds apbrowsergate's non-secret configuration, populated from flags
// and (via clikit) the environment.
type config struct {
	proxyAddr   string
	gateAddr    string
	devtoolsURL string
	policyPath  string
}

func main() {
	deplogs.Silence()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := newCommand().ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	cfg := &config{}
	cmd := &cobra.Command{
		Use:          "apbrowsergate",
		Short:        "Browser toolbox gate: a policy-enforcing CONNECT proxy and an authenticated DevTools proxy.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), cfg)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&cfg.proxyAddr, "proxy-addr", browsergate.DefaultProxyAddr, "Chrome-facing forward-proxy listen address (loopback only)")
	fs.StringVar(&cfg.gateAddr, "gate-addr", browsergate.DefaultGateAddr, "runner-facing DevTools-proxy + /healthz listen address")
	fs.StringVar(&cfg.devtoolsURL, "devtools-url", browsergate.DefaultDevtoolsURL, "Chrome remote-debugging endpoint to proxy to")
	fs.StringVar(&cfg.policyPath, "policy-path", "/etc/browserpolicy/policy.json", "path to the JSON browser policy document (reloaded on mtime change)")
	cmd.PreRunE = clikit.EnvOverridePreRunE(map[string]string{
		"proxy-addr":   "APBROWSERGATE_PROXY_ADDR",
		"gate-addr":    "APBROWSERGATE_GATE_ADDR",
		"devtools-url": "APBROWSERGATE_DEVTOOLS_URL",
		"policy-path":  "APBROWSERGATE_POLICY_PATH",
	})
	return cmd
}

// run builds the gate from cfg and the token env var and serves until the
// context is cancelled. browsergate.New fails closed on a missing token or an
// unreadable/invalid policy, so a bad configuration crash-loops the pod rather
// than serving an open gate.
func run(ctx context.Context, cfg *config) error {
	g, err := browsergate.New(browsergate.Config{
		ProxyAddr:   cfg.proxyAddr,
		GateAddr:    cfg.gateAddr,
		DevtoolsURL: cfg.devtoolsURL,
		Token:       os.Getenv(tokenEnv),
		PolicyPath:  cfg.policyPath,
	})
	if err != nil {
		return err
	}
	return g.Run(ctx)
}
