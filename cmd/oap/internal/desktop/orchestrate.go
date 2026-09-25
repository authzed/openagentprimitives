package desktop

import "context"

// EngineHooks are the in-process seam between orchestration sequencing
// (this package, pure + fake-testable) and the heavy, k8s-dependent
// install/configure/init-local paths. Production wiring (the `oap
// desktop` command) populates these with closures over runInstall /
// runInit; keeping them as function fields lets Engine stay free of k8s
// client deps and be exercised in unit tests with a fake
// ClusterProvider. A nil hook is simply skipped — no step is mandatory.
type EngineHooks struct {
	// Install applies the manifest bundle to the freshly-provisioned,
	// freshly-started cluster.
	Install func(ctx context.Context, kubeconfig []byte, env map[string]string) error
	// Configure provisions the demo agent (model token + cluster-default
	// catalog entry, built-in-chat CRs, and — only when cfg.Channel !=
	// nil — the external channel's secret + Channel CR). Runs after
	// Install, before the conditional tunnel step.
	Configure func(ctx context.Context, kubeconfig []byte, cfg Config) error
	// InitLocal runs the tunnel/channel wiring and returns the public
	// URL. Only invoked when cfg.Channel != nil: the built-in chat is
	// reachable at localhost and needs no tunnel.
	InitLocal func(ctx context.Context, kubeconfig []byte, env map[string]string) (publicURL string, err error)
	// Progress, if set, is called at the START of each bring-up step with a
	// short human-readable label ("Booting virtual machine", "Installing
	// components", …). It is the seam the UI uses to surface the current
	// step (menubar status today; a live setup-timeline next). Nil = skipped.
	// Steps fire in Up's order, so a consumer can treat every earlier step as
	// complete once a later one starts, and all steps as complete once Up
	// returns nil.
	Progress func(step string)
}

// Engine sequences local-cluster bring-up/teardown against a
// ClusterProvider, delegating the install/configure/init-local steps to
// EngineHooks.
type Engine struct {
	p     ClusterProvider
	hooks EngineHooks
}

// NewEngine builds an Engine over the given ClusterProvider and hooks.
func NewEngine(p ClusterProvider, hooks EngineHooks) *Engine {
	return &Engine{p: p, hooks: hooks}
}

// Up runs the full bring-up: validate config -> provision -> start ->
// discover guest IP -> fetch + rewrite kubeconfig -> install -> configure
// -> (external channel only) init-local tunnel. Any step error aborts
// the sequence and is returned as-is (no silent errors); callers surface
// it to the UI. Returns the public URL, or "" when the built-in chat
// (no tunnel) is used.
func (e *Engine) Up(ctx context.Context, cfg Config) (string, error) {
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	e.progress("Provisioning disk image")
	if err := e.p.Provision(ctx); err != nil {
		return "", err
	}
	e.progress("Booting virtual machine")
	if err := e.p.Start(ctx); err != nil {
		return "", err
	}
	e.progress("Waiting for guest network")
	ip, err := e.p.GuestIP(ctx)
	if err != nil {
		return "", err
	}
	e.progress("Fetching cluster credentials")
	raw, err := e.p.Kubeconfig(ctx, ip)
	if err != nil {
		return "", err
	}
	kc, err := RewriteKubeconfigServer(raw, ip)
	if err != nil {
		return "", err
	}

	env := cfg.EnvForInstall()
	if e.hooks.Install != nil {
		e.progress("Installing components")
		if err := e.hooks.Install(ctx, kc, env); err != nil {
			return "", err
		}
	}
	if e.hooks.Configure != nil {
		e.progress("Configuring cluster")
		if err := e.hooks.Configure(ctx, kc, cfg); err != nil {
			return "", err
		}
	}
	// Tunnel only when an external channel needs a public URL; the
	// built-in chat is reachable at localhost with no config.
	if cfg.Channel != nil && e.hooks.InitLocal != nil {
		e.progress("Setting up tunnel")
		return e.hooks.InitLocal(ctx, kc, env)
	}
	return "", nil
}

// progress reports a bring-up step to the Progress hook if one is set.
func (e *Engine) progress(step string) {
	if e.hooks.Progress != nil {
		e.hooks.Progress(step)
	}
}

// Down stops the running cluster without destroying its disk state.
func (e *Engine) Down(ctx context.Context) error { return e.p.Stop(ctx) }

// Uninstall destroys the cluster and its underlying VM/disk state.
func (e *Engine) Uninstall(ctx context.Context) error { return e.p.Destroy(ctx) }

// Status reports the underlying provider's current state.
func (e *Engine) Status(ctx context.Context) (State, error) { return e.p.Status(ctx) }
