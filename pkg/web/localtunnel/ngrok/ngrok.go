// Package ngrok provides a Tunnel implementation backed by ngrok's
// Go agent SDK (golang.ngrok.com/ngrok/v2). Reads NGROK_AUTHTOKEN from
// env unless explicitly set on the Tunnel struct.
//
// Lifecycle: Start opens an HTTPS-fronted endpoint that ngrok's cloud
// service forwards to the caller-supplied localAddr. The agent SDK
// handles the reverse-proxying internally — no goroutine on our side.
// Stop closes the endpoint + the underlying agent session.
package ngrok

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	ngroksdk "golang.ngrok.com/ngrok/v2"

	"github.com/authzed/openagentprimitives/pkg/web/localtunnel"
	"github.com/authzed/openagentprimitives/pkg/web/localtunnel/registry"
)

// envAuthtoken is the env var the ngrok agent SDK conventionally reads
// for the cloud-service auth token. We honour the same name when the
// Tunnel.AuthToken field is empty.
const envAuthtoken = "NGROK_AUTHTOKEN"

func init() {
	registry.Register("ngrok", func(opts registry.Options) localtunnel.Tunnel {
		// opts.ReservedDomain is not yet wired to this Tunnel — the Go
		// agent SDK's reserved-domain support is a separate follow-up;
		// nothing today reads it back off a Tunnel built this way.
		//
		// Always a fresh, non-nil *Tunnel: never assign a possibly-nil
		// pointer through a variable here, since a typed-nil *Tunnel
		// boxed in this non-nil localtunnel.Tunnel interface would panic
		// on first method call rather than compare equal to nil.
		return &Tunnel{AuthToken: opts.AuthToken}
	})
}

// Tunnel is an ngrok-backed Tunnel. Configure AuthToken before Start
// (or leave empty to read NGROK_AUTHTOKEN from env). Safe to Stop
// before Start (no-op); Stop is idempotent.
type Tunnel struct {
	// AuthToken is the ngrok cloud-service auth token. When empty,
	// Start falls back to the NGROK_AUTHTOKEN env var. If neither is
	// set, Start returns a clear error rather than attempting an
	// anonymous connect.
	AuthToken string

	mu        sync.Mutex
	agent     ngroksdk.Agent
	forwarder ngroksdk.EndpointForwarder
}

// Start opens the tunnel pointing at localAddr (e.g.
// "http://127.0.0.1:8080"). Returns the public URL ngrok assigned.
// Blocks until the tunnel is ready or ctx cancels.
func (t *Tunnel) Start(ctx context.Context, localAddr string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.forwarder != nil {
		return "", errors.New("ngrok: tunnel already started")
	}

	token := t.AuthToken
	if token == "" {
		token = os.Getenv(envAuthtoken)
	}
	if token == "" {
		return "", fmt.Errorf("ngrok: missing auth token: set Tunnel.AuthToken or %s env var", envAuthtoken)
	}

	agent, err := ngroksdk.NewAgent(ngroksdk.WithAuthtoken(token))
	if err != nil {
		return "", fmt.Errorf("ngrok: new agent: %w", err)
	}

	// Forward asks ngrok's cloud service to push inbound traffic at
	// localAddr; the SDK runs the proxy loop internally (no goroutine
	// on our side). The agent auto-connects on first Forward call.
	forwarder, err := agent.Forward(ctx, ngroksdk.WithUpstream(localAddr))
	if err != nil {
		// Best-effort agent teardown — Disconnect on an unconnected
		// agent is documented as a no-op, and we never reached the
		// connected state if Forward failed before auto-connect.
		if discErr := agent.Disconnect(); discErr != nil {
			// Surface the disconnect failure alongside the primary
			// error so an operator sees both.
			return "", fmt.Errorf("ngrok: forward: %w (disconnect also failed: %v)", err, discErr)
		}
		return "", fmt.Errorf("ngrok: forward: %w", err)
	}

	t.agent = agent
	t.forwarder = forwarder
	return forwarder.URL().String(), nil
}

// Stop closes the tunnel + disconnects the agent session. Idempotent —
// safe to call multiple times and before Start.
func (t *Tunnel) Stop() error {
	t.mu.Lock()
	forwarder := t.forwarder
	agent := t.agent
	t.forwarder = nil
	t.agent = nil
	t.mu.Unlock()

	if forwarder == nil {
		return nil
	}

	// Close the forwarder first (stops accepting + forwarding), then
	// disconnect the agent session. Collect both errors so neither is
	// silently dropped (AGENTS.md: never silently drop errors).
	closeErr := forwarder.Close()
	discErr := agent.Disconnect()
	switch {
	case closeErr != nil && discErr != nil:
		return fmt.Errorf("ngrok: close: %w; disconnect: %v", closeErr, discErr)
	case closeErr != nil:
		return fmt.Errorf("ngrok: close: %w", closeErr)
	case discErr != nil:
		return fmt.Errorf("ngrok: disconnect: %w", discErr)
	}
	return nil
}

// Interface compile-time check.
var _ localtunnel.Tunnel = (*Tunnel)(nil)
