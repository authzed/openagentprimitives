// Package kube holds small Kubernetes client-config helpers shared by the
// cmd/ server binaries (channelsd, webd). It is distinct from
// cmd/oap/internal/kube, which serves the CLI.
package kube

import (
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
)

// RestConfig returns a *rest.Config, preferring the in-cluster config and
// falling back to controller-runtime's GetConfig (KUBECONFIG) for local
// development. On the fallback path it returns GetConfig's error unwrapped, so
// callers can apply their own fmt.Errorf wrap.
func RestConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return ctrl.GetConfig()
	}
	return cfg, nil
}
