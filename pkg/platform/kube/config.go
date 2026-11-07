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
// relayed envelope on a single serialized goroutine, and shares this budget
// with everything else the process does. Nine concurrently-busy sessions
// outran 5 requests/s and backed the relay up by twenty minutes: agent
// replies posted that late, and approval prompts expired before the human
// ever saw them.
const (
	DefaultQPS   float32 = 50
	DefaultBurst int     = 100
)

// RestConfig returns a *rest.Config, preferring the in-cluster config and
// falling back to controller-runtime's GetConfig (KUBECONFIG) for local
// development. On the fallback path it returns GetConfig's error unwrapped, so
// callers can apply their own fmt.Errorf wrap.
//
// Either way, an unset QPS or Burst is raised to DefaultQPS/DefaultBurst. A
// value already set (GetConfig's own, or one a caller configured) is kept.
func RestConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = ctrl.GetConfig()
		if err != nil {
			return nil, err
		}
	}
	withRateLimitDefaults(cfg)
	return cfg, nil
}

func withRateLimitDefaults(cfg *rest.Config) {
	if cfg.QPS == 0 {
		cfg.QPS = DefaultQPS
	}
	if cfg.Burst == 0 {
		cfg.Burst = DefaultBurst
	}
}
