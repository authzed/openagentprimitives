# oauth

OAuth 2.0 + PKCE for MCP servers, on the **standard library only** — no
third-party OAuth client dependency.

One full login is Discover → (optionally) Register → Run:

| File | Step |
| --- | --- |
| [`discovery.go`](discovery.go) | RFC 8414 authorization-server metadata (`/.well-known/oauth-authorization-server`). |
| [`registration.go`](registration.go) | RFC 7591 Dynamic Client Registration, when the caller has no pre-registered client. |
| [`pkce.go`](pkce.go) | `NewPKCE` — the verifier, the S256 challenge, and the `state` nonce, all from `crypto/rand`. |
| [`flow.go`](flow.go), [`flow_helpers.go`](flow_helpers.go) | The browser round trip: build the authorization URL, serve the localhost callback, validate `state`, exchange the code. |
| [`token.go`](token.go) | Token exchange and refresh. `Token.Scope` is what was *granted*, which may be narrower than what was requested. |
| [`login.go`](login.go) | `Login` — the whole sequence as one call. Used by both `oap tools` and the gen agent's `mcp_oauth` tool. |
| [`browser.go`](browser.go) | `OpenBrowser`, a thin re-export of `pkg/x/browser.Open` (the actual `runtime.GOOS` dispatcher — opening a browser has nothing to do with OAuth). Kept here under this name so existing callers (identity setup flows, `open_url`) don't need to change. |

## Constraints

- An empty `ClientID` means "attempt DCR". If the server's metadata advertises no
  `registration_endpoint`, `Login` returns `ErrNoDynamicRegistration` naming the
  server — it never proceeds unauthenticated.
- An empty `Opts.Scope` falls back to `Metadata.ScopesSupported` joined with
  spaces. An empty `ScopesSupported` means the server *declared* nothing, not
  that no scope may be requested.
- HTTP calls go through [`pkg/x/safehttp`](../../../x/safehttp/) unless the
  caller injects its own client.

Part of [`pkg/tools/mcp`](../README.md). See the root
[`README.md`](../../../../README.md) and [`AGENTS.md`](../../../../AGENTS.md).
