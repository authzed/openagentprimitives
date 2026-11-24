// Package kube holds small Kubernetes client-config helpers shared by the
// cmd/ server binaries (channelsd, webd). It is distinct from
// cmd/oap/internal/kube, which serves the CLI.
package kube

import (
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Client-side API rate limits applied when the config leaves them unset.
//
// rest.InClusterConfig sets neither, so a client built from it falls back to
// client-go's DefaultQPS/DefaultBurst of 5/10 — a ceiling sized for a CLI, not
// a server. channelsd's outbound relay spends at least one API call per
// relayed envelope on a single serialized goroutine, drawing on the same
// client as inbound message handling and the per-Channel refreshes. Nine
// concurrently-busy sessions outran 5 requests/s and backed the relay up by
// twenty minutes: agent replies posted that late, and approval prompts expired
// before the human ever saw them.
//
// The limit is per client, not per process: client-go builds one token bucket
// for each client constructed from a config, so a binary that builds several
// clients from one RestConfig can exceed DefaultQPS in aggregate.
const (
	DefaultQPS   float32 = 50
	DefaultBurst int     = 100
)

// Sources of the two configs RestConfig chooses between; variables so a test
// can assert the defaults are applied on both branches.
var (
	inClusterConfig = rest.InClusterConfig
	fallbackConfig  = ctrl.GetConfig
)

// RestConfig returns a *rest.Config, preferring the in-cluster config and
// falling back to controller-runtime's GetConfig (KUBECONFIG) for local
// development. On the fallback path it returns GetConfig's error unwrapped, so
// callers can apply their own fmt.Errorf wrap.
//
// The in-cluster config arrives with QPS and Burst unset, and both are raised
// to DefaultQPS/DefaultBurst. The fallback arrives with client-side limiting
// disabled — GetConfig sets QPS to -1 when nothing else configured it, leaving
// throttling to the API server's priority and fairness — and is left that way.
// See withRateLimitDefaults for the exact rule.
func RestConfig() (*rest.Config, error) {
	cfg, err := inClusterConfig()
	if err != nil {
		cfg, err = fallbackConfig()
		if err != nil {
			return nil, err
		}
	}
	withRateLimitDefaults(cfg)
	return cfg, nil
}

// withRateLimitDefaults fills in only what is unset (zero). A QPS or Burst
// already set is kept, and a negative QPS — client-side limiting disabled — is
// left entirely alone: client-go ignores Burst in that case, so defaulting it
// would only make the config claim a limit that is not applied.
func withRateLimitDefaults(cfg *rest.Config) {
	if cfg.QPS < 0 {
		return
	}
	if cfg.QPS == 0 {
		cfg.QPS = DefaultQPS
	}
	if cfg.Burst == 0 {
		cfg.Burst = DefaultBurst
	}
}
