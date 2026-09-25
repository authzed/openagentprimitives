// SessionCache keeps ONE persistent MCP ClientSession per (server URL,
// credential) for the lifetime of an AgentSession, so server-side per-session
// state survives across tool calls.
//
// Why this exists: a stateful MCP server (the dedicated-mcp sidecar) keys its
// per-request selection — which PermissionSystem / cluster this agent picked —
// by the MCP session id (req.Session.ID()). The plain CallTool opens a fresh
// session (a new initialize handshake → a new session id) for every tool call
// and Closes it, so a selection made on one call is invisible to the next: the
// server answers "no PermissionSystem selected for this session" even though the
// agent just selected one. Reusing one session per AgentSession fixes that at
// the source — one selection, one session, many calls.
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SessionCache maps a (server URL, credential) pair to its live MCP session. One
// instance is owned by the runner per AgentSession and Closed when the session
// ends. It is safe for concurrent use — the runner dispatches tool calls in
// parallel.
type SessionCache struct {
	mu     sync.Mutex
	byKey  map[string]*cachedSession
	closed bool
}

// cachedSession is the lazily-opened, reused session for one cache key. Its own
// mutex guards open/reopen (which does blocking network I/O) so the
// SessionCache-level lock is never held across a dial; concurrent calls on the
// same key serialize only on the open, then share the session for CallTool, which
// the go-sdk ClientSession supports concurrently (JSON-RPC id correlation).
type cachedSession struct {
	mu   sync.Mutex
	sess *mcp.ClientSession
	// tr is the auth transport baked into sess's HTTP client at open time. Held
	// so ensure can push a re-resolved value of the SAME credential onto a
	// session opened under the older one — the key no longer changes when a
	// value rotates, so without this the entry would keep presenting whatever
	// bytes its opener happened to hold. Valid only while sess != nil.
	tr *authTransport
}

// NewSessionCache returns an empty cache. Sessions are opened lazily on the
// first CallTool for a given (url, credential) and reused thereafter.
func NewSessionCache() *SessionCache {
	return &SessionCache{byKey: map[string]*cachedSession{}}
}

// sessionKey is the identity of a shareable session: the endpoint, the header
// the credential is injected under, and WHICH credential that is — never the
// bytes currently presented for it.
//
// The URL alone is NOT enough. Nothing stops two MCPServer CRs from naming the
// same spec.server.url while binding different credentials — no uniqueness
// marker, no admission webhook, no controller check — and the runner wires ONE
// SessionCache into every CR's tools. Since ensure bakes a credential into the
// session's HTTP transport, a URL-only key would hand the second CR the first
// CR's token: dispatch order would decide which credential executes upstream,
// invisible to the per-call SpiceDB use_token gate, which checks the credential
// the caller INTENDED to send.
//
// credID must therefore distinguish credentials without changing when a value
// rotates, so the value itself cannot serve. OAuth tokens refresh routinely
// (the operator's pre-expiry refresh, authTransport's reauth-on-401), and
// keying on presented bytes makes every refresh a cache miss: a second session,
// the server's per-session state silently gone one call later, and the
// superseded entry holding a live session until Close. Callers pass a stable
// identity of the credential SOURCE instead (pkg/agent/tool/mcp derives it from
// externaltoken.CredID). Empty credID means "no credential" — those callers are
// one anonymous principal and rightly share.
//
// Two callers on one endpoint that resolve to the same credID share a session
// even if their MCPServer CRs differ — they are one principal upstream and one
// externaltoken subject in SpiceDB, so a revocation reaches both alike.
func sessionKey(url, header, credID string) string {
	return url + "\x00" + header + "\x00" + credID
}

// entryFor returns the (get-or-created) cachedSession for this
// url+header+credential. Only the map lookup/insert is under the cache lock;
// opening happens under the entry lock.
//
// byKey is bounded by the number of distinct (url, header, credID) triples the
// AgentSession is CONFIGURED with — a static set derived from its MCPServer CRs —
// not by runtime events: no token refresh, revocation or re-resolution adds an
// entry, because none of them changes the key.
func (c *SessionCache) entryFor(url, header, credID string) (*cachedSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("mcp session cache is closed")
	}
	k := sessionKey(url, header, credID)
	e := c.byKey[k]
	if e == nil {
		e = &cachedSession{}
		c.byKey[k] = e
	}
	return e, nil
}

// ensure returns the entry's live session, opening it on first use, with its
// transport carrying the credential THIS caller presents.
//
// On a reused session that means pushing header/value onto the transport the
// opener installed. The entry is keyed by credential identity, not value, so a
// hit can legitimately carry a different value for the same credential — a
// refresh, or a re-resolution after revocation. THE CALLER'S VALUE WINS: a
// caller that just re-resolved a revoked credential must not have the
// superseded bytes sent on its behalf. The session survives, which is the point
// of the cache and what authTransport's reauth-on-401 already does in place.
//
// Concurrent callers of one entry share one transport, so an overlapping
// in-flight request carries the last writer's value. That errs toward the
// FRESHER value of the same credential, and the per-call use_token gate has
// already run against the value its own caller intended.
//
// cred.Reauth is baked in at open time and belongs to the opening caller. Every
// caller of an entry is by construction the same credential, so its
// re-resolution is right for all of them.
func (e *cachedSession) ensure(
	ctx context.Context,
	url string,
	httpClient *http.Client,
	cred Credential,
) (*mcp.ClientSession, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sess != nil {
		e.tr.setAuth(cred.Header, cred.Value)
		return e.sess, nil
	}
	hc, tr, err := withAuth(httpClient, url, cred.Header, cred.Value, cred.Reauth)
	if err != nil {
		return nil, err
	}
	sess, err := openSession(ctx, url, hc)
	if err != nil {
		return nil, err
	}
	e.sess, e.tr = sess, tr
	return sess, nil
}

// invalidate drops the entry's session iff it is still the one the caller used,
// so a transport error on a stale session triggers exactly one reopen and does
// not race a concurrent reopen that already replaced it.
func (e *cachedSession) invalidate(used *mcp.ClientSession) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sess == used {
		_ = e.sess.Close()
		e.sess, e.tr = nil, nil
	}
}

// failureKind classifies a failed tools/call by what the JSON-RPC POST actually
// received, which is the only thing that says whether re-issuing the call could
// run the tool a SECOND time.
type failureKind int

const (
	// serverDispatched: the POST got a 2xx, so the server accepted the request
	// and answered it — a Go error here is a JSON-RPC *error response* (the
	// go-sdk returns one as a Go error too), which a low-level ToolHandler, an
	// unknown-tool/invalid-params protocol error, or any non-Go server can
	// produce AFTER mutating state. The session is alive and the tool ran.
	serverDispatched failureKind = iota
	// sessionGone: the POST got a 404. Per MCP §2.5.3 a server that terminated
	// the session MUST answer requests carrying that session ID with 404, so the
	// call was refused before dispatch — provably not executed.
	sessionGone
	// undetermined: any other answer, or none at all (connection reset, DNS
	// failure, 5xx). The call may or may not have reached the tool.
	undetermined
)

func classifyFailure(postStatus int) failureKind {
	switch {
	case postStatus >= 200 && postStatus < 300:
		return serverDispatched
	case postStatus == http.StatusNotFound:
		return sessionGone
	default:
		return undetermined
	}
}

// Credential is what a caller presents to an MCP endpoint, together with the
// identity of the credential it is presenting it FOR.
//
// The package-level CallTool takes the presentation alone as loose arguments,
// which is all a one-shot session needs. A cached session outlives any single
// presentation of its credential — a refresh changes Value without changing
// which credential it is — so SessionCache needs the two separated, and takes
// them as one value rather than as four more positional arguments.
type Credential struct {
	// ID identifies the credential SOURCE and MUST be stable across a rotation
	// of Value: it is what decides which cached session this call joins (see
	// sessionKey). Empty means no credential.
	ID string
	// Header is the request header Value is injected under. Empty means no auth
	// is sent at all.
	Header string
	// Value is the credential's presentation for THIS call — the exact bytes
	// that go upstream, which is what the caller's per-call authorization gate
	// checked. A reused session's transport is set to it before the call.
	Value string
	// Reauth re-resolves (Header, Value) when the endpoint answers 401, so a
	// token rotated out mid-session recovers inside the live session. Nil
	// disables the retry.
	Reauth func(context.Context) (header, value string, err error)
}

// CallTool calls tool `name` on the MCP server at url over the cache's persistent
// session for that url AND credential (opening it on first use). See the package
// CallTool for the auth/reauth/httpClient contract, which is otherwise identical.
//
// A failed call is re-issued ONLY when the server provably did not dispatch it:
// a 404 on the JSON-RPC POST, which per MCP §2.5.3 is how a server reports that
// it terminated the session (the sidecar restarted, the server GC'd an idle
// session). There the session is invalidated, reopened once, and the call
// retried on the fresh session.
//
// Every other failure is returned as-is, because `cerr != nil` does NOT mean
// "the tool did not run": the go-sdk returns a Go error for a JSON-RPC *error
// response* as much as for a transport failure, so re-issuing on any error
// double-executes tools/call against a server that mutated state and then
// answered with a protocol error — and these are user-supplied sidecar MCP
// servers. A call the server answered (2xx) also leaves the session alone;
// tearing down the persistent session because a tool reported "unknown tool"
// helps nobody. An undetermined outcome (no response, 5xx, reset) drops the
// session so the NEXT call reopens, but is not re-issued.
//
// A tool that merely *reports* failure surfaces as CallOutcome.IsError, not a
// Go error, and never reaches any of this.
func (c *SessionCache) CallTool(
	ctx context.Context,
	url string,
	httpClient *http.Client,
	name string,
	args any,
	cred Credential,
) (*CallOutcome, error) {
	if httpClient == nil {
		httpClient = defaultGuardedClient
	}
	e, err := c.entryFor(url, cred.Header, cred.ID)
	if err != nil {
		return nil, err
	}

	// One exchange record per logical call, carried in the ctx the go-sdk builds
	// its POST with. The transport is SHARED across this cache's parallel tool
	// dispatch, so the record must not be.
	ctx, ex := startExchange(ctx)

	sess, err := e.ensure(ctx, url, httpClient, cred)
	if err != nil {
		return nil, ex.httpError(fmt.Errorf("mcp connect %s: %w", url, err))
	}

	res, cerr := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if cerr != nil {
		switch classifyFailure(ex.jsonRPCPostStatus()) {
		case serverDispatched:
			// The server answered the call; the error is its answer. Retrying
			// would run the tool twice.
			return nil, ex.httpError(fmt.Errorf("mcp call %q: %w", name, cerr))
		case undetermined:
			// We cannot tell whether the tool ran, so we must not re-run it.
			// The session is plausibly dead; drop it so the NEXT call reopens.
			e.invalidate(sess)
			return nil, ex.httpError(fmt.Errorf("mcp call %q: %w", name, cerr))
		}
		// sessionGone: refused before dispatch. Reopen once and retry, so a
		// mid-AgentSession sidecar restart is transparent to the agent rather
		// than a hard tool error.
		e.invalidate(sess)
		sess, err = e.ensure(ctx, url, httpClient, cred)
		if err != nil {
			return nil, ex.httpError(fmt.Errorf("mcp reconnect %s: %w", url, err))
		}
		res, cerr = sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if cerr != nil {
			return nil, ex.httpError(fmt.Errorf("mcp call %q: %w", name, cerr))
		}
	}

	var meta json.RawMessage
	if res.Meta != nil {
		meta, _ = json.Marshal(res.Meta)
	}
	return &CallOutcome{Content: contentBlocks(res.Content), IsError: res.IsError, Meta: meta}, nil
}

// Close closes every open session and marks the cache closed. Safe to call once
// at AgentSession end; subsequent CallTool returns an error rather than opening a
// new session.
func (c *SessionCache) Close() error {
	c.mu.Lock()
	sessions := make([]*cachedSession, 0, len(c.byKey))
	for _, e := range c.byKey {
		sessions = append(sessions, e)
	}
	c.byKey = map[string]*cachedSession{}
	c.closed = true
	c.mu.Unlock()

	for _, e := range sessions {
		e.mu.Lock()
		if e.sess != nil {
			_ = e.sess.Close()
			e.sess, e.tr = nil, nil
		}
		e.mu.Unlock()
	}
	return nil
}
