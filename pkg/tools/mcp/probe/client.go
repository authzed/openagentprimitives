// Package probe is the Streamable-HTTP MCP client used by the runner (sidecar +
// MCPServer tool synthesis and dispatch), the operator controllers (MCPServer /
// SidecarToolbox reachability), and the oap CLI. It is backed by the official MCP
// Go SDK (see session.go), so it speaks the full session protocol; a bare
// stateless JSON-RPC POST is rejected by spec-compliant servers.
package probe

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

const defaultTimeout = 10 * time.Second

// defaultGuardedClient is the process-wide SSRF-guarded HTTP client for MCP
// probe requests. Allocated once so callers share a single connection pool.
var defaultGuardedClient = safehttp.Client()

// Client wraps an MCP server URL.
//
// URL is an agent/attacker-influenceable value — it originates from an
// MCPServer CR's spec.server.url. When HTTP is nil, requests default to the
// SSRF-guarded safehttp.Client() so a hostile URL cannot reach loopback /
// link-local / private-network destinations. Only tests, the e2e harness, and
// operator-controlled reach paths (a sidecar pod IP / loopback, which the guard
// would correctly refuse) should set HTTP explicitly to a loopback-permitting
// client.
type Client struct {
	HTTP    *http.Client
	URL     string
	Timeout time.Duration // 0 → defaultTimeout
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultGuardedClient
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultTimeout
}

// ListTools issues `tools/list` over a fresh MCP session and returns the tool
// array. header/value are the optional auth header pair (e.g. "Authorization",
// "Bearer xyz"); pass "" / "" for no auth.
func (c *Client) ListTools(ctx context.Context, header, value string) ([]Tool, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	hc, _, err := withAuth(c.httpClient(), c.URL, header, value, nil)
	if err != nil {
		return nil, err
	}
	ctx, ex := startExchange(ctx)
	sess, err := openSession(ctx, c.URL, hc)
	if err != nil {
		return nil, ex.httpError(err)
	}
	defer sess.Close()

	res, err := sess.ListTools(ctx, nil)
	if err != nil {
		return nil, ex.httpError(fmt.Errorf("probe: tools/list: %w", err))
	}
	out := make([]Tool, 0, len(res.Tools))
	for _, t := range res.Tools {
		if t != nil {
			out = append(out, toolFromSDK(t))
		}
	}
	return out, nil
}

// Initialize opens a session (which performs the MCP `initialize` handshake) and
// returns the server's self-reported identity. Header/value are sent verbatim;
// pass "" / "" for no auth. A server that omits serverInfo yields zero
// ServerInfo with nil error (audit metadata only — self-reported).
func (c *Client) Initialize(ctx context.Context, header, value string) (ServerInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	hc, _, err := withAuth(c.httpClient(), c.URL, header, value, nil)
	if err != nil {
		return ServerInfo{}, err
	}
	ctx, ex := startExchange(ctx)
	sess, err := openSession(ctx, c.URL, hc)
	if err != nil {
		return ServerInfo{}, ex.httpError(err)
	}
	defer sess.Close()

	ir := sess.InitializeResult()
	if ir == nil || ir.ServerInfo == nil {
		return ServerInfo{}, nil
	}
	return ServerInfo{Name: ir.ServerInfo.Name, Version: ir.ServerInfo.Version}, nil
}
