package kube

import (
	"fmt"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ClientOpts is the input for constructing a client bundle. Mirrors the global
// CLI flags.
type ClientOpts struct {
	Kubeconfig string // empty → default discovery
	Context    string // empty → current-context
	Namespace  string // empty → context's default ns, falling back to "default"
}

// Bundle holds the four flavors of client oap needs.
type Bundle struct {
	REST       *rest.Config
	Typed      kubernetes.Interface
	Dynamic    dynamic.Interface
	Controller client.Client // controller-runtime typed client (handy for our v1alpha1 types)
	Discovery  discovery.DiscoveryInterface
	Namespace  string // resolved
}

// New constructs a Bundle from opts. Resolves the namespace from the kubeconfig
// when opts.Namespace is empty, falling back to "default".
func New(opts ClientOpts) (*Bundle, error) {
	cfgFlags := genericclioptions.NewConfigFlags(true)
	if opts.Kubeconfig != "" {
		cfgFlags.KubeConfig = &opts.Kubeconfig
	}
	if opts.Context != "" {
		cfgFlags.Context = &opts.Context
	}

	restConfig, err := cfgFlags.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("kube: build rest config: %w", err)
	}
	// oap install/clean fire many API calls in bursts (apply the bundle, ensure
	// Secrets, poll a Gateway address). client-go's default 5 QPS / 10 Burst
	// throttles these into client-side rate-limiter waits that, under a slow
	// cloud LB, exhausted an operation deadline ("client rate limiter Wait …
	// context deadline exceeded"). A short-lived admin CLI can afford a much
	// higher budget.
	restConfig.QPS = 50
	restConfig.Burst = 100

	typed, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("kube: build typed client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("kube: build dynamic client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("kube: build discovery client: %w", err)
	}
	ctrlClient, err := client.New(restConfig, client.Options{Scheme: Scheme})
	if err != nil {
		return nil, fmt.Errorf("kube: build controller-runtime client: %w", err)
	}

	ns := opts.Namespace
	if ns == "" {
		raw, _, err := cfgFlags.ToRawKubeConfigLoader().Namespace()
		if err == nil && raw != "" {
			ns = raw
		} else {
			ns = "default"
		}
	}

	return &Bundle{
		REST: restConfig, Typed: typed, Dynamic: dyn,
		Controller: ctrlClient, Discovery: disc, Namespace: ns,
	}, nil
}
