// Package-local cluster-safety heuristic, absorbed from the CLI when pkg/platform/cloud took over cluster-kind dispatch.
//
// The check is INTENTIONALLY conservative: loopback IPs, *.local DNS,
// kubernetes.docker.internal, and a small set of recognized dev-cluster
// context-name prefixes are accepted. Anything else is refused, with
// AllowOverride (--allow-non-local-cluster) as the documented escape for
// unusual but legitimate setups such as a corporate dev cluster on a VPN.
package local

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	"k8s.io/client-go/rest"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// Validate refuses to install the local profile onto anything that is not a
// local cluster.
//
// Two independent refusals, in order of severity:
//
//  1. A managed-cloud providerID. This is NEVER overridable: the local profile
//     installs sqlite, an in-memory SpiceDB datastore, and a file:// artifact
//     PVC. On a real cloud cluster that is data loss waiting to happen, however
//     the user reached it.
//  2. A server URL / kube-context that doesn't look local. Overridable via
//     AllowOverride, because the heuristic cannot see a VPN'd dev cluster.
func (Strategy) Validate(ctx context.Context, p cloud.ValidateParams) error {
	if managed, err := cloud.DetectedManagedKey(ctx, p); err != nil {
		return err
	} else if managed != "" {
		return fmt.Errorf(
			"refusing the %s cluster kind against a %s cluster: it installs sqlite, an in-memory SpiceDB datastore, and a file:// artifact PVC, none of which are durable on a managed cloud. Drop --local (or pass --cluster-kind=%s), or point kubectl at a local cluster",
			cloud.KeyLocal, managed, managed)
	}
	if p.AllowOverride {
		return nil
	}
	if ok, reason := looksLocal(p.RESTConfig, p.ContextName); !ok {
		return fmt.Errorf(
			"refusing the %s cluster kind against a non-local cluster: %s\nTo override (e.g. a corporate dev cluster on a VPN), pass --allow-non-local-cluster",
			cloud.KeyLocal, reason)
	}
	return nil
}

// looksLocal reports whether the kubeconfig's active cluster looks like a local
// dev cluster. Returns (allowed, reason); reason is non-empty only when the
// answer is false.
func looksLocal(cfg *rest.Config, contextName string) (allowed bool, reason string) {
	if cfg == nil || cfg.Host == "" {
		return false, "empty server URL — no cluster configured"
	}
	u, err := url.Parse(cfg.Host)
	if err != nil {
		return false, fmt.Sprintf("couldn't parse server URL %q: %v", cfg.Host, err)
	}
	host := u.Hostname()
	if host == "" {
		return false, fmt.Sprintf("server URL %q has no host component", cfg.Host)
	}

	// 1. Loopback (127.0.0.1, ::1, etc.).
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			return true, ""
		}
	}

	// 2. localhost / *.local / docker-desktop's well-known hostname.
	if host == "localhost" ||
		host == "kubernetes.docker.internal" ||
		strings.HasSuffix(host, ".local") {
		return true, ""
	}

	// 3. Recognized dev-cluster context names. These map to clusters
	//    that bind to 127.0.0.1 but identify the context by a
	//    distinctive prefix in the kubeconfig:
	//      - kind:           context name "kind-<cluster>"
	//      - minikube:       context name "minikube" (or "minikube-…")
	//      - k3d:            context name "k3d-<cluster>"
	//      - docker-desktop: context name "docker-desktop"
	switch {
	case contextName == "docker-desktop",
		contextName == "minikube",
		strings.HasPrefix(contextName, "minikube-"),
		strings.HasPrefix(contextName, "kind-"),
		strings.HasPrefix(contextName, "k3d-"):
		return true, ""
	}

	return false, fmt.Sprintf(
		"cluster %q (server %s) doesn't match a local-cluster pattern",
		contextName, cfg.Host)
}
