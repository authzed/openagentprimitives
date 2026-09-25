// Package localtunnel exposes a local TCP port as a public HTTPS URL
// for development workflows. The primary use case is `oap init --local`:
// the operator runs `oap` against a local k8s cluster (kind, minikube,
// docker-desktop), starts a tunnel pointing at the in-cluster identityd
// Service via port-forward, and patches identityd's external-URL
// ConfigMap so OAuth providers' redirect_uri parameters + Slack DM
// links resolve to the public hostname.
//
// The Tunnel interface is sized to accept multiple reverse-tunnel providers;
// ngrok is the one implemented today (pkg/web/localtunnel/ngrok).
package localtunnel

import "context"

// Tunnel exposes a local TCP port as a public HTTPS URL. Concrete
// implementations live in subpackages (ngrok today, others later).
//
// Lifecycle: caller does Start → uses publicURL → Stop on shutdown.
// Stop is idempotent and safe from any goroutine.
type Tunnel interface {
	// Start opens the tunnel pointing at localAddr (e.g.
	// "http://127.0.0.1:8080"). Returns the externally-reachable
	// public URL. Blocks until the tunnel is ready to forward traffic
	// or ctx is cancelled.
	Start(ctx context.Context, localAddr string) (publicURL string, err error)

	// Stop closes the tunnel. Idempotent — safe to call multiple times
	// and from any goroutine. Returns an error only on unrecoverable
	// shutdown failures (the caller logs + proceeds; the process is
	// usually exiting anyway).
	Stop() error
}
