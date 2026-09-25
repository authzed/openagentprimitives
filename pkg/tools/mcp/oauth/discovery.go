package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Metadata holds the OAuth authorization-server metadata we care about.
// Populated from RFC 8414 (/.well-known/oauth-authorization-server).
type Metadata struct {
	// Issuer is the authorization server's own identifier. RFC 8414 §3.3
	// requires it to be identical to the issuer the metadata was fetched from,
	// and this field existing at all is what makes that check possible — it was
	// absent, so the check could not be performed even in principle.
	//
	// Empty means the server declared none (a pre-RFC-8414 server), which is
	// not an attack and is not refused: the origin binding on the
	// resource_metadata URL is what carries the security weight here.
	Issuer string `json:"issuer,omitempty"`
	// AuthorizationEndpoint is where the user's browser is sent to consent.
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	// TokenEndpoint is where a code is exchanged, and a token refreshed.
	TokenEndpoint string `json:"token_endpoint"`
	// RegistrationEndpoint is the dynamic-client-registration URL. Empty means
	// the server offers no DCR, so a client must be registered by hand.
	RegistrationEndpoint string `json:"registration_endpoint,omitempty"`
	// ScopesSupported is what the server advertises. Empty means it declared
	// nothing, not that no scope may be requested.
	ScopesSupported []string `json:"scopes_supported,omitempty"`
}

// Discover performs OAuth endpoint discovery for an MCP server URL.
//
// Strategy (per MCP OAuth spec + RFC 9728 + RFC 8414):
//  1. GET mcpServerURL unauthenticated. If 401 with WWW-Authenticate header
//     containing resource_metadata="<URL>", fetch that URL (RFC 9728) to get
//     the resource-server metadata. Then follow authorization_servers[0] to
//     fetch /.well-known/oauth-authorization-server.
//  2. Fallback: GET <mcpServerURL>/.well-known/oauth-authorization-server
//     directly. A document found here that names a DIFFERENT issuer is
//     followed once, to that issuer's own well-known metadata — the same
//     decoupled resource-server/auth-server split step 1 handles via
//     authorization_servers[0], reached instead because the MCP server
//     itself serves (or mirrors) the well-known document rather than
//     returning 401+WWW-Authenticate for it. See fetchAuthServerMetadata's
//     doc comment for why following it is safe.
//
// Returns a descriptive error naming every URL that was tried.
func Discover(ctx context.Context, hc *http.Client, mcpServerURL string) (*Metadata, error) {
	// tried accumulates one annotation per URL attempted, each naming why it
	// failed (or that it was only the unauthenticated probe), so a total
	// failure explains every path instead of listing bare URLs with no reason.
	var tried []string
	annotate := func(u, reason string) { tried = append(tried, fmt.Sprintf("%s (%s)", u, reason)) }

	// --- Step 1: probe the MCP server for WWW-Authenticate ---
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mcpServerURL, nil)
	if err != nil {
		return nil, fmt.Errorf("discovery: build probe request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discovery: probe %s: %w", mcpServerURL, err)
	}
	_ = resp.Body.Close()

	annotate(mcpServerURL, fmt.Sprintf("probe status %d", resp.StatusCode))

	if resp.StatusCode == http.StatusUnauthorized {
		wwwAuth := resp.Header.Get("WWW-Authenticate")
		if resourceMetaURL := parseResourceMetadata(wwwAuth); resourceMetaURL != "" {
			// RFC 9728 §3.3: the resource metadata URL must belong to the
			// resource server's own origin. Checked BEFORE the fetch, because
			// the request itself is the harm — it is an SSRF from this process,
			// and it tells an attacker-chosen host that this deployment is
			// starting an OAuth flow.
			//
			// The header is per-response, so the attacker need not own the MCP
			// server: a compromised CDN in front of an honest one, or an honest
			// one with a response-header-injection bug, is enough. What follows
			// a successful spoof is a 302 of the user's browser FROM the
			// authenticated portal origin to an arbitrary attacker URL, plus
			// Dynamic Client Registration at the attacker's endpoint.
			if err := sameOrigin(mcpServerURL, resourceMetaURL); err != nil {
				annotate(resourceMetaURL, "resource metadata: "+err.Error())
				return nil, fmt.Errorf("discovery: %w", err)
			}
			meta, err := fetchResourceMetadata(ctx, hc, resourceMetaURL)
			switch {
			case err != nil:
				annotate(resourceMetaURL, "resource metadata: "+err.Error())
			case len(meta.AuthorizationServers) == 0:
				annotate(resourceMetaURL, "resource metadata: no authorization_servers")
			default:
				annotate(resourceMetaURL, "resource metadata: ok")
				// Deliberately NOT origin-checked against the MCP server. A
				// cross-origin AS is the normal shape — mcp.vendor.example
				// pointing at auth.vendor.example — and refusing it would be an
				// outage, not a hardening. What protects this hop is the issuer
				// check in fetchAuthServerMetadata: the AS must vouch for
				// itself. What protects the hop BEFORE it is the origin binding
				// above, which is where an attacker with only header injection
				// is stopped.
				authServerBase := meta.AuthorizationServers[0]
				wellKnown := strings.TrimRight(authServerBase, "/") + "/.well-known/oauth-authorization-server"
				// followIssuer=false: we already took one hop to get here (the
				// resource metadata's own authorization_servers[0]), so the
				// document AT that address must vouch for itself — no further
				// redirect.
				md, err := fetchAuthServerMetadata(ctx, hc, wellKnown, false)
				if err == nil {
					return md, nil
				}
				annotate(wellKnown, "auth server metadata: "+err.Error())
				// fall through to direct fallback
			}
		}
	}

	// --- Step 2: direct well-known fallback on MCP server base ---
	base, err := baseURL(mcpServerURL)
	if err != nil {
		return nil, fmt.Errorf("discovery: parse server URL: %w", err)
	}
	wellKnown := base + "/.well-known/oauth-authorization-server"
	// followIssuer=true: this well-known URL is derived directly from
	// mcpServerURL (a trusted, operator-configured address, not an
	// attacker-controllable redirect), so a document served there that names a
	// DIFFERENT issuer is treated as a pointer to the real authorization
	// server — the same decoupled resource-server/auth-server split RFC 9728's
	// authorization_servers[] conveys above — rather than a forgery. See
	// fetchAuthServerMetadata's doc comment for why this is safe.
	md, err := fetchAuthServerMetadata(ctx, hc, wellKnown, true)
	if err == nil {
		return md, nil
	}
	annotate(wellKnown, "auth server metadata: "+err.Error())

	return nil, fmt.Errorf("discovery: no OAuth metadata found; tried: %s", strings.Join(tried, "; "))
}

// resourceServerMeta is the RFC 9728 resource-server metadata shape.
type resourceServerMeta struct {
	AuthorizationServers []string `json:"authorization_servers"`
}

// parseResourceMetadata extracts the resource_metadata URL from a
// WWW-Authenticate: Bearer ... resource_metadata="<url>" header value.
// Returns "" if not present.
func parseResourceMetadata(wwwAuth string) string {
	// We look for resource_metadata="..." or resource_metadata=<token>.
	const key = `resource_metadata=`
	idx := strings.Index(wwwAuth, key)
	if idx < 0 {
		return ""
	}
	rest := wwwAuth[idx+len(key):]
	if strings.HasPrefix(rest, `"`) {
		// quoted
		rest = rest[1:]
		end := strings.Index(rest, `"`)
		if end < 0 {
			return rest
		}
		return rest[:end]
	}
	// unquoted token: ends at comma, space or end
	end := strings.IndexAny(rest, ", ")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func fetchResourceMetadata(ctx context.Context, hc *http.Client, u string) (*resourceServerMeta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("resource metadata %s: status %d", u, resp.StatusCode)
	}
	var m resourceServerMeta
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("resource metadata %s: decode: %w", u, err)
	}
	return &m, nil
}

// fetchAuthServerMetadata fetches and validates the RFC 8414 metadata at u.
//
// followIssuer permits ONE hop: when the document is well-formed but its
// declared issuer names a DIFFERENT origin than u, and followIssuer is true,
// the document is treated as a POINTER to the real authorization server —
// mirroring the RFC 9728 authorization_servers[] hop in Discover — rather
// than a forgery. This fetches <issuer>/.well-known/oauth-authorization-server
// instead and requires THAT document to be self-consistent (its own issuer
// must match ITS OWN origin), with no further redirect permitted regardless
// of what it names. followIssuer is false for a document already reached via
// one such hop (Discover's RFC 9728 branch), so the terminal check there
// stays strict.
//
// This is safe against the threat the strict check defends against — an
// attacker-controlled redirect (header injection, a compromised proxy)
// pointing discovery at an arbitrary host — because u is never attacker
// data here: it is either the address in an operator-applied MCPServer's
// spec.server.url (Discover's direct fallback) or an authorization_servers[0]
// entry the resource server already named inside a document whose own origin
// was checked before the fetch (Discover's RFC 9728 branch). A forged
// document can, at worst, redirect discovery to a second origin it names —
// never bypass the self-consistency check, which still applies at that final
// destination.
func fetchAuthServerMetadata(ctx context.Context, hc *http.Client, u string, followIssuer bool) (*Metadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth server metadata %s: status %d", u, resp.StatusCode)
	}
	var m Metadata
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("auth server metadata %s: decode: %w", u, err)
	}
	if m.AuthorizationEndpoint == "" || m.TokenEndpoint == "" {
		return nil, fmt.Errorf("auth server metadata %s: missing required fields", u)
	}
	// RFC 8414 §3.3. A declared issuer naming a DIFFERENT server means the
	// document does not describe the server that served it — and what is in it
	// is a browser redirect target and a token endpoint, not something to take
	// on trust from a party that just disclaimed authorship. An absent issuer
	// is a pre-RFC-8414 server and is accepted.
	if m.Issuer != "" {
		if err := sameOrigin(u, m.Issuer); err != nil {
			if followIssuer {
				issuerWellKnown := strings.TrimRight(m.Issuer, "/") + "/.well-known/oauth-authorization-server"
				md, ferr := fetchAuthServerMetadata(ctx, hc, issuerWellKnown, false)
				if ferr != nil {
					return nil, fmt.Errorf("auth server metadata %s: issuer %q disagrees with where it was fetched from; following it failed: %w",
						u, m.Issuer, ferr)
				}
				return md, nil
			}
			return nil, fmt.Errorf("auth server metadata %s: issuer %q disagrees with where it was fetched from: %w",
				u, m.Issuer, err)
		}
	}
	return &m, nil
}

// sameOrigin reports whether candidate shares want's scheme, host and port.
//
// Origin, not host: an https resource server whose metadata is served over
// plain http has been downgraded, and a check that ignored scheme would pass it.
func sameOrigin(want, candidate string) error {
	w, err := url.Parse(want)
	if err != nil {
		return fmt.Errorf("parse %q: %w", want, err)
	}
	c, err := url.Parse(candidate)
	if err != nil {
		return fmt.Errorf("parse %q: %w", candidate, err)
	}
	if w.Scheme == "" || w.Host == "" {
		return fmt.Errorf("%q has no origin to compare against", want)
	}
	if !strings.EqualFold(w.Scheme, c.Scheme) || !strings.EqualFold(w.Host, c.Host) {
		return fmt.Errorf("%q is not same-origin with %q", candidate, w.Scheme+"://"+w.Host)
	}
	return nil
}

func baseURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	return u.Scheme + "://" + u.Host, nil
}

// userAgent is set on all outbound HTTP requests from this package.
const userAgent = "agentprimitives ap/1"
