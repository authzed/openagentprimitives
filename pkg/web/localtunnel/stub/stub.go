// Package stub provides a deterministic Tunnel implementation for tests.
// Configure the fields before Start; the stub returns canned values without
// opening any actual tunnel.
//
// This package registers itself under the name "stub" in
// pkg/web/localtunnel/registry so registry_test.go (and any other test) can
// resolve it by name the same way production resolves "ngrok". Deliberately
// NOT blank-imported by any production binary (the operator imports only
// ngrok): PublicEndpointSpec.Provider has no CRD-level enum restricting it
// to known-good values, so if this package were ever linked into the
// operator, a live cluster's spec.provider: stub would resolve and silently
// open no tunnel at all — a fail-open a CRD-enum-gated field (like Channel's
// kind) doesn't have to guard against, and this one does.
package stub

import (
	"context"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/web/localtunnel"
	"github.com/authzed/openagentprimitives/pkg/web/localtunnel/registry"
)

func init() {
	registry.Register("stub", func(registry.Options) localtunnel.Tunnel {
		// Always a fresh, non-nil *Tunnel — see ngrok.go's init() for why
		// that matters.
		return &Tunnel{}
	})
}

// Tunnel is a test-controllable Tunnel. Configure URL, StartErr, StopErr
// before Start; inspect Started()/Stopped()/LocalAddr() afterward.
//
// Zero-value Tunnel returns "" from Start (the test should set URL).
type Tunnel struct {
	// URL is returned from Start when StartErr is nil.
	URL string
	// StartErr, if non-nil, is returned from Start instead of URL.
	StartErr error
	// StopErr, if non-nil, is returned from Stop.
	StopErr error

	mu        sync.Mutex
	started   bool
	stopped   bool
	localAddr string
}

// Start records the call (Started returns true afterward) and returns
// the configured URL or StartErr.
func (t *Tunnel) Start(_ context.Context, localAddr string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.started = true
	t.localAddr = localAddr
	if t.StartErr != nil {
		return "", t.StartErr
	}
	return t.URL, nil
}

// Stop records the call (Stopped returns true afterward) and returns
// the configured StopErr. Idempotent — safe to call multiple times.
func (t *Tunnel) Stop() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	return t.StopErr
}

// Started reports whether Start was called at least once.
func (t *Tunnel) Started() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.started
}

// Stopped reports whether Stop was called at least once.
func (t *Tunnel) Stopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopped
}

// LocalAddr returns the localAddr Start was called with (empty before
// Start). Tests use this to verify the port-forward wiring.
func (t *Tunnel) LocalAddr() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.localAddr
}

// Interface compile-time check.
var _ localtunnel.Tunnel = (*Tunnel)(nil)
