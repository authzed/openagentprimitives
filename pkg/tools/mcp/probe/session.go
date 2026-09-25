// Streamable-HTTP MCP client backed by the official MCP Go SDK
// (github.com/modelcontextprotocol/go-sdk). The SDK client speaks the full
// session protocol (initialize → Mcp-Session-Id → notifications/initialized →
// requests, plus SSE unwrapping and reconnects), which a hand-rolled stateless
// JSON-RPC POST does NOT — spec-compliant servers (the MCP go-sdk and TS SDK,
// e.g. the dedicated-mcp sidecar) reject bare tools/list with "invalid during
// session initialization". Both the reachability probe (ListTools) and the
// tool-call dispatcher (CallTool) go through here so every MCP integration —
// external MCPServers and in-cluster sidecars alike — is session-compliant.
//
// Auth (and OAuth-rotation reauth-on-401) live in a RoundTripper on the injected
// HTTP client, so the SDK client and the dispatcher's CEL/audit/toolguard
// middleware stay auth-agnostic.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// clientImpl is the identity we present in the initialize handshake.
var clientImpl = &mcp.Implementation{Name: "agentprimitives", Version: "0"}

// origin is the scheme+host an upstream MCP credential is scoped to. The
// credential is minted for ONE endpoint, so it must never travel to another —
// see authTransport.matchesOrigin.
type origin struct {
	scheme string
	host   string // lowercased, default port dropped
}

func (o origin) empty() bool { return o.scheme == "" || o.host == "" }

func (o origin) String() string { return o.scheme + "://" + o.host }

// originOfURL canonicalizes u's scheme+host: lowercase, and the scheme's
// default port dropped so https://x and https://x:443 are one origin.
func originOfURL(u *url.URL) origin {
	if u == nil {
		return origin{}
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port != "" && !((scheme == "https" && port == "443") || (scheme == "http" && port == "80")) {
		host = net.JoinHostPort(host, port)
	}
	return origin{scheme: scheme, host: host}
}

// originOf parses an endpoint URL into the origin its credential is pinned to.
// An endpoint with no scheme+host has no origin to pin against, which is an
// error rather than a wildcard: a blank pin would put us back to sending the
// credential to whatever host the redirect chain names.
func originOf(rawURL string) (origin, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return origin{}, fmt.Errorf("parse endpoint %q: %w", rawURL, err)
	}
	o := originOfURL(u)
	if o.empty() {
		return origin{}, fmt.Errorf("endpoint %q has no scheme+host to scope credentials to", rawURL)
	}
	return o, nil
}

// exchange is what the transport observed for ONE logical MCP exchange. It
// rides the request context (startExchange), never the transport, because a
// SessionCache transport is SHARED across parallel tool dispatch: a
// per-transport slot would be a lost update in one direction and a fabrication
// in the other — call A's opaque transport failure reading call B's concurrent
// 401 and surfacing a typed HTTPError{401} A never received. That value becomes
// agenttool.Result.HTTPStatus, which the credential-update path
// (pkg/agent/tool/authfail) treats as the platform's own evidence that a
// credential was rejected, so the fabrication manufactures corroboration.
//
// Context scoping does NOT separate the POST from the SDK's side requests, and
// assuming it did was a real hole. go-sdk builds the connection context with
// xcontext.Detach, which drops cancellation and keeps VALUES, so the
// session-teardown DELETE and the standalone-SSE GET both run carrying the
// opening caller's exchange — and both are on-endpoint, so the origin pin does
// not separate them either. recordExchange separates them explicitly, on
// method.
type exchange struct {
	mu sync.Mutex

	// errStatus / errBody hold the most recent auth-or-server-error response
	// (401/403/5xx) of this exchange. The go-sdk collapses such responses into
	// opaque connect/transport errors, so callers consult these to re-surface a
	// typed *HTTPError (auth vs server error) — a caller branches on IsAuth()
	// to tell "token expired, re-auth" from "server unhealthy".
	errStatus int
	errBody   string

	// postStatus is the status the JSON-RPC-carrying POST received, whatever it
	// was; 0 means no HTTP response reached us at all. SessionCache reads it to
	// decide whether re-issuing a failed tools/call could double-execute the
	// tool — see classifyFailure.
	postStatus int
}

type exchangeCtxKey struct{}

// startExchange returns a context carrying a fresh exchange record, plus the
// record. Every entry point that drives one logical MCP exchange opens one and
// threads the returned context into the go-sdk.
func startExchange(ctx context.Context) (context.Context, *exchange) {
	ex := &exchange{}
	return context.WithValue(ctx, exchangeCtxKey{}, ex), ex
}

// exchangeFrom returns the exchange ctx carries, or nil when it carries none.
func exchangeFrom(ctx context.Context) *exchange {
	ex, _ := ctx.Value(exchangeCtxKey{}).(*exchange)
	return ex
}

// httpError returns a typed *HTTPError wrapping err when this exchange recorded
// an auth/server-error status, else err unchanged. Nil-receiver safe so callers
// that never opened an exchange still get their error through.
func (ex *exchange) httpError(err error) error {
	if ex == nil {
		return err
	}
	ex.mu.Lock()
	sc, body := ex.errStatus, ex.errBody
	ex.mu.Unlock()
	if sc == 0 {
		return err
	}
	return &HTTPError{StatusCode: sc, Body: body}
}

// jsonRPCPostStatus reports the status the JSON-RPC POST received (0 = none).
func (ex *exchange) jsonRPCPostStatus() int {
	if ex == nil {
		return 0
	}
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return ex.postStatus
}

// authTransport injects a static auth header on requests to the endpoint's own
// origin; on a 401 it re-resolves the credential via reauth (when set) and
// retries the request once with the new header. This keeps MCP OAuth-rotation
// handling in the transport layer so the SDK client + dispatcher never see it.
// header=="" means no auth.
type authTransport struct {
	base   http.RoundTripper
	reauth func(context.Context) (header, value string, err error) // nil = no reauth

	// endpoint is the origin the credential is scoped to. Injecting the header
	// in a RoundTripper defeats Go's cross-host Authorization stripping:
	// net/http snapshots the ORIGINAL request's headers before the redirect
	// loop starts, so a header added afterwards is not in that snapshot and is
	// re-applied, unstripped, on every hop. Pinning the origin here restores the
	// guarantee, so an open redirect on an otherwise-honest MCP host (or a CDN
	// in front of it) cannot disclose the enterprise OAuth/PAT credential.
	endpoint origin

	mu     sync.Mutex
	header string
	value  string
}

// setAuth replaces the credential this transport injects. Two writers use it:
// the 401 reauth below (a refresh this transport itself provoked) and
// SessionCache, which reconciles a reused session's transport against the
// credential the current caller intends to present — a caller that re-resolved
// the credential out-of-band (the revocation path) must not have the session's
// older value sent on its behalf.
func (t *authTransport) setAuth(header, value string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.header, t.value = header, value
}

// matchesOrigin reports whether u is the endpoint this transport's credential
// was minted for. Fail-closed: an unpinned transport matches nothing.
func (t *authTransport) matchesOrigin(u *url.URL) bool {
	if t.endpoint.empty() {
		return false
	}
	return originOfURL(u) == t.endpoint
}

// recordExchange captures what one round trip observed into the exchange the
// request carries (if any), resetting resp.Body so the SDK can still read it.
//
// ONLY the JSON-RPC POST may set the exchange's error status. Context scoping
// does not separate the POST from the SDK's side requests, though this file
// used to claim it did: go-sdk builds its connection context with
// xcontext.Detach, which drops CANCELLATION and keeps VALUES, so the
// standalone-SSE GET issued inside Connect — and the session-teardown DELETE —
// run carrying the opening caller's exchange. Both are on-endpoint, so the
// origin pin does not separate them either; it defends against off-endpoint
// hops, not against a side request to the endpoint itself.
//
// That mattered because errStatus is the platform's own evidence a credential
// was rejected: dispatch copies it to agenttool.Result.HTTPStatus and
// credupdate.classify matches it against [401] to raise a card telling a person
// their credential no longer works. A server answering 200 to initialize and
// 401 to the standalone-SSE GET could manufacture that attestation for a
// working credential, and carry 512 bytes of its own text into the error.
//
// Restricting to POST also subsumes the older 404/405 carve-out — those were
// the standalone-SSE probe's own benign answers, and no side request reaches
// this at all now.
func recordExchange(req *http.Request, resp *http.Response) {
	ex := exchangeFrom(req.Context())
	if ex == nil || resp == nil {
		return
	}
	if req.Method != http.MethodPost {
		return
	}
	sc := resp.StatusCode
	ex.mu.Lock()
	ex.postStatus = sc
	ex.mu.Unlock()
	if sc != http.StatusUnauthorized && sc != http.StatusForbidden && sc < 500 {
		return
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	b := string(body)
	const maxBody = 512
	if len(b) > maxBody {
		b = b[:maxBody] + "...(truncated)"
	}
	ex.mu.Lock()
	ex.errStatus, ex.errBody = sc, b
	ex.mu.Unlock()
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Buffer the body so a reauth retry can resend it (the SDK sends a fresh
	// JSON body per call; without buffering the retry would send an empty body).
	var bodyBytes []byte
	if req.Body != nil {
		var rerr error
		bodyBytes, rerr = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if rerr != nil {
			// Discarding this sent a SILENTLY TRUNCATED body upstream as if it
			// were complete, on a request that carries a credential. It fails
			// safe in practice — a truncated JSON-RPC body is rejected — but the
			// caller then sees an unexplained upstream protocol error with
			// nothing naming the real cause.
			return nil, fmt.Errorf("mcp probe: reading the request body for %s %s: %w",
				req.Method, req.URL.Redacted(), rerr)
		}
	}
	onEndpoint := t.matchesOrigin(req.URL)

	t.mu.Lock()
	h, v := t.header, t.value
	t.mu.Unlock()

	if !onEndpoint && (h != "" || t.reauth != nil) {
		// A redirect took us off the endpoint. Withholding the credential is
		// the whole point of the pin, but it must not be silent: the call will
		// now fail as unauthenticated, and an operator needs to be able to tell
		// that from a genuinely bad token. No credential value is logged.
		slog.Warn("mcp probe: withholding upstream credential on an off-endpoint hop",
			"endpoint", t.endpoint.String(), "hop", originOfURL(req.URL).String(), "method", req.Method)
	}
	attempt := func(header, value string) (*http.Response, error) {
		r := req.Clone(req.Context())
		if bodyBytes != nil {
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			r.ContentLength = int64(len(bodyBytes))
		}
		if header != "" && onEndpoint {
			r.Header.Set(header, value)
		}
		return t.base.RoundTrip(r)
	}

	resp, err := attempt(h, v)
	// reauth only for a 401 on the JSON-RPC POST from the endpoint itself.
	//
	// Off-endpoint is excluded because a 401 there is not a rejection of our
	// credential — minting a fresh token would deliver it (and persist it) to a
	// host that was never supposed to see one.
	//
	// Non-POST is excluded for the same reason recordExchange excludes it: the
	// SDK's standalone-SSE GET and teardown DELETE reach here on-endpoint, and a
	// 401 on either is the server talking about a side request, not about the
	// caller's credential. Left in, a hostile server drove reauth →
	// reauthPersist → setAuthLocked on every session open — for a minted
	// credential (ID-JAG federation, a GitHub App) a fresh token exchange at the
	// IdP each time, and SessionCache reopens on every sessionGone.
	if err != nil || resp == nil || resp.StatusCode != http.StatusUnauthorized ||
		t.reauth == nil || !onEndpoint || req.Method != http.MethodPost {
		recordExchange(req, resp)
		return resp, err
	}
	// 401 mid-session is the OAuth-token-rotated-out signature: re-resolve the
	// backing credential and retry exactly once. Drain+close the first response.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	nh, nv, rerr := t.reauth(req.Context())
	if rerr != nil {
		// The 401 is about to surface as an auth failure; without this line the
		// reason the platform could not re-resolve the credential (broker down,
		// Secret gone, refresh rejected) is lost, and the operator sees only
		// "HTTP 401" and blames the token.
		slog.Warn("mcp probe: could not re-resolve the upstream credential after a 401",
			"endpoint", t.endpoint.String(), "err", rerr.Error())
		resp, err = attempt(h, v) // can't reauth — resend original so the 401 surfaces
		recordExchange(req, resp)
		return resp, err
	}
	t.mu.Lock()
	t.header, t.value = nh, nv
	t.mu.Unlock()
	resp, err = attempt(nh, nv)
	recordExchange(req, resp)
	return resp, err
}

// openSession dials one MCP server over Streamable HTTP and returns a connected
// session (initialize handshake done). Auth is baked into httpClient via
// withAuth before calling. Callers MUST Close the returned session.
func openSession(ctx context.Context, url string, httpClient *http.Client) (*mcp.ClientSession, error) {
	client := mcp.NewClient(clientImpl, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: httpClient}, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp connect %s: %w", url, err)
	}
	return sess, nil
}

// withAuth returns a copy of hc whose transport injects the given auth header
// (and reauths on 401 when reauth != nil) on requests to endpoint's own origin,
// plus that transport itself — SessionCache retains it to keep a long-lived
// session's credential in step with what its callers present (see setAuth);
// one-shot callers discard it.
//
// The transport is installed unconditionally — even with no auth (header=="",
// reauth==nil) — because it is also what records the HTTP status behind the
// SDK's opaque transport errors.
//
// It errors when endpoint has no scheme+host to scope the credential to, rather
// than falling back to an unpinned transport: such an endpoint cannot be dialled
// anyway, and an unpinned transport is exactly the redirect leak the pin exists
// to close.
func withAuth(hc *http.Client, endpoint, header, value string, reauth func(context.Context) (string, string, error)) (*http.Client, *authTransport, error) {
	o, err := originOf(endpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: %w", err)
	}
	if hc == nil {
		hc = &http.Client{}
	}
	base := hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	tr := &authTransport{base: base, reauth: reauth, header: header, value: value, endpoint: o}
	c := *hc
	c.Transport = tr
	return &c, tr, nil
}

// ContentBlock is one normalized block of a tool-call result — the shape the
// dispatcher renders (mirrors the MCP content variants it handles). Sourced from
// the SDK's typed Content by contentBlocks so the dispatcher's rendering loop is
// unchanged by the SDK migration.
type ContentBlock struct {
	Type     string // "text" | "image" | "audio" | "resource" | "unknown"
	Text     string
	MIMEType string
	Data     []byte
	URI      string
	// Resource* carry the embedded resource's own body, populated only for
	// "resource" blocks (empty otherwise). Added so the MCP dispatcher can
	// intercept mcp-ui / MCP Apps ui:// UI resources — whose document lives in
	// Text or Blob and whose CSP lives in ResourceMeta (_meta.ui.csp) — instead
	// of flattening them to a bare [resource: URI] placeholder.
	ResourceMIMEType string
	ResourceText     string
	ResourceBlob     []byte
	ResourceMeta     json.RawMessage
}

// CallOutcome is the normalized result of a tools/call.
type CallOutcome struct {
	Content []ContentBlock
	IsError bool
	// Meta is the raw response `_meta` object (SEP-1913 annotations etc.),
	// nil when the server emits none. Callers parse the subset they consume.
	Meta json.RawMessage
}

// contentBlocks converts the SDK's typed Content list to []ContentBlock.
func contentBlocks(cs []mcp.Content) []ContentBlock {
	out := make([]ContentBlock, 0, len(cs))
	for _, c := range cs {
		switch v := c.(type) {
		case *mcp.TextContent:
			out = append(out, ContentBlock{Type: "text", Text: v.Text})
		case *mcp.ImageContent:
			out = append(out, ContentBlock{Type: "image", MIMEType: v.MIMEType, Data: v.Data})
		case *mcp.AudioContent:
			out = append(out, ContentBlock{Type: "audio", MIMEType: v.MIMEType, Data: v.Data})
		case *mcp.EmbeddedResource:
			cb := ContentBlock{Type: "resource"}
			if v.Resource != nil {
				cb.URI = v.Resource.URI
				cb.ResourceMIMEType = v.Resource.MIMEType
				cb.ResourceText = v.Resource.Text
				cb.ResourceBlob = v.Resource.Blob
				if len(v.Resource.Meta) > 0 {
					if raw, err := json.Marshal(v.Resource.Meta); err == nil {
						cb.ResourceMeta = raw
					}
				}
			}
			out = append(out, cb)
		default:
			out = append(out, ContentBlock{Type: "unknown"})
		}
	}
	return out
}

// CallTool opens a session to the MCP server at url, calls tool `name` with
// `args`, and returns the normalized outcome. header/value are the optional auth
// header pair (pass "" / "" for none); reauth re-resolves the credential on a
// 401 (nil = no reauth). httpClient selects the reach path (plain for
// operator-controlled sidecar/loopback, SSRF-guarded for agent-supplied URLs).
func CallTool(
	ctx context.Context,
	url string,
	httpClient *http.Client,
	name string,
	args any,
	header, value string,
	reauth func(context.Context) (string, string, error),
) (*CallOutcome, error) {
	if httpClient == nil {
		// Agent-supplied MCPServer URLs default to the SSRF-guarded client;
		// operator-controlled reach paths (sidecar pod IP / loopback) pass a
		// plain client explicitly.
		httpClient = defaultGuardedClient
	}
	hc, _, err := withAuth(httpClient, url, header, value, reauth)
	if err != nil {
		return nil, err
	}
	ctx, ex := startExchange(ctx)
	sess, err := openSession(ctx, url, hc)
	if err != nil {
		return nil, ex.httpError(err)
	}
	defer sess.Close()

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, ex.httpError(fmt.Errorf("mcp call %q: %w", name, err))
	}
	var meta json.RawMessage
	if res.Meta != nil {
		meta, _ = json.Marshal(res.Meta)
	}
	return &CallOutcome{Content: contentBlocks(res.Content), IsError: res.IsError, Meta: meta}, nil
}

// toolFromSDK converts one SDK tool to our probe.Tool (raw-JSON schemas so
// callers introspect shape without a JSON-Schema parser).
func toolFromSDK(t *mcp.Tool) Tool {
	var in, out json.RawMessage
	if t.InputSchema != nil {
		in, _ = json.Marshal(t.InputSchema)
	}
	if t.OutputSchema != nil {
		out, _ = json.Marshal(t.OutputSchema)
	}
	tl := Tool{Name: t.Name, Description: t.Description, InputSchema: in, OutputSchema: out}
	if t.Annotations != nil {
		tl.Annotations = Annotations{
			Title:           t.Annotations.Title,
			ReadOnlyHint:    t.Annotations.ReadOnlyHint,
			OpenWorldHint:   deref(t.Annotations.OpenWorldHint),
			IdempotentHint:  t.Annotations.IdempotentHint,
			DestructiveHint: deref(t.Annotations.DestructiveHint),
		}
	}
	// SEP-1913 trust annotations (maliciousActivityHint / attribution /
	// input+returnMetadata) have no home in go-sdk's typed ToolAnnotations, which
	// silently drops unknown fields. A go-sdk server therefore carries them in the
	// tool's _meta; recover them here so probe.Annotations stays complete for any
	// server that adopts that convention.
	if len(t.Meta) > 0 {
		if raw, err := json.Marshal(map[string]any(t.Meta)); err == nil {
			var sep struct {
				MaliciousActivityHint bool            `json:"maliciousActivityHint"`
				Attribution           []string        `json:"attribution"`
				InputMetadata         json.RawMessage `json:"inputMetadata"`
				ReturnMetadata        json.RawMessage `json:"returnMetadata"`
			}
			if json.Unmarshal(raw, &sep) == nil {
				tl.Annotations.MaliciousActivityHint = sep.MaliciousActivityHint
				tl.Annotations.Attribution = sep.Attribution
				tl.Annotations.InputMetadata = sep.InputMetadata
				tl.Annotations.ReturnMetadata = sep.ReturnMetadata
			}

			// MCP Apps' visibility ("model" / "app") lives one level deeper
			// than the SEP-1913 fields above, nested under "ui" (a sibling of
			// "ui.csp" — see the widget CSP recovery elsewhere in this
			// package). Decode it separately rather than folding it into the
			// flat `sep` struct.
			var uiMeta struct {
				UI struct {
					Visibility []string `json:"visibility"`
				} `json:"ui"`
			}
			if json.Unmarshal(raw, &uiMeta) == nil {
				tl.Annotations.Visibility = uiMeta.UI.Visibility
			}
		}
	}
	return tl
}

func deref(b *bool) bool { return b != nil && *b }
