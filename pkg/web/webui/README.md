# `pkg/web/webui`

The framework for `webd`'s browser UIs: a registry of scoped, **two-origin**
plug-ins served by one host binary. This package owns the plug-in contract, the
two muxes, the auth middleware, the shared HTML document, and the CSP — not any
particular page.

## The plug-in contract

```go
type WebUI interface {
	Name() string
	Routes(deps Deps) []Route
}
```

A plug-in registers itself with `func init() { registry.Register(ui{}) }` and is
pulled in by an import from `internal/cmd/webd`. The registry lives in a
[separate subpackage](registry) so it can import `webui` without a cycle.

Two plug-ins register from **outside** this tree: `pkg/platform/identityd`
(`"identity"`) and [`pkg/web/adminui`](../adminui) (`"admin"`).

Note that `webd` imports several plug-ins **by name** rather than blank — it
references e.g. `agentui.Deps` in a compile-time guard, so a signature drift
becomes a build failure instead of a runtime type-assert that silently 404s.

## Two origins, one binary

| Origin          | Serves                                        | Cookie                        |
| --------------- | --------------------------------------------- | ----------------------------- |
| `OriginTrusted` | The application: pages, JSON APIs, websockets | **Holds the auth cookie**     |
| `OriginSandbox` | Semi-trusted artifact and widget content      | **Never receives the cookie** |

Requests are dispatched by `Host` header, against getters resolved **per
request** — so a rotating tunnel URL needs no rebuild. With neither origin
configured, everything 404s rather than routing against an empty host.

`GET /healthz` short-circuits before Host dispatch, so probes work
host-agnostically.

**Shared-origin debug mode collapses the two into one host and is insecure by
construction** — artifact content then shares the auth origin, so the session
cookie is reachable by content. It is gated per request and off unless asked
for.

## Auth levels

`AuthNone`, `AuthAuthenticated`, `AuthAuthorized`, `AuthLoginIfNecessary`,
`AuthHandlerManaged`. `mount` validates every route at startup and rejects
malformed ones: a route with neither or both of `Handler`/`Page`, a `Page` with
no `App` or nil `Build`, an `AuthAuthorized` route with nil `Authorize`, or an
`AuthNone`/`AuthLoginIfNecessary` route declaring anything but GET/HEAD.

**An authorization _error_ must not be reported as a denial.** Return a typed
`*webui.PageError` with a 500/503 status; a bare error is collapsed to 403 by
`renderAuthorizeFailure`, which would tell a user "forbidden" when the truth is
"SpiceDB is down".

CSRF is centralized in `TrustedOriginMatch`, which **fails closed on a blank
trusted origin** — the reason it exists is that a hand-inlined
`r.Header.Get("Origin") != trusted` becomes a silent no-op precisely when the
deployment is misconfigured.

## Files

| File             | What it holds                                                                                 |
| ---------------- | --------------------------------------------------------------------------------------------- |
| `contract.go`    | `Origin`, `AuthLevel`, `Route`, `WebUI`, `Deps`, `SubjectFromContext`, `TrustedOriginMatch`   |
| `server.go`      | `Server` — mounts, validates, wraps, and dispatches per request by Host                       |
| `page.go`        | The declarative `Page` type (`App`, `FramesSandbox`, `Build`), `PageMeta`, `PageError`        |
| `document.go`    | Renders the shared HTML document, the nonce, `MarshalBootstrap`, and `buildCSP`               |
| `system.go`      | The styled system error/info page, via the built-in `system` app                              |
| `apprender.go`   | `AppRenderer` — the escape hatch letting a raw `http.Handler` render a React document         |
| `assets.go`      | The embedded-manifest loader, the `/assets/` file server, and `BundleHandler(appKey)`         |
| `inlineerror.go` | A self-contained, asset-free error page for the cookieless sandbox origin                     |
| `origin.go`      | `SanitizeOrigin`, `StrictOrigin`, `SanitizeCSPSourceToken`, and the shared breakout predicate |

## Subpackages

| Package                        | Kind    | What it is                                                                                                                 |
| ------------------------------ | ------- | -------------------------------------------------------------------------------------------------------------------------- |
| [`agentui`](agentui)           | plug-in | The agent-defined view of a session. Serves no page of its own.                                                            |
| [`artifactview`](artifactview) | plug-in | Artifact live view across both origins. **The security-critical one.**                                                     |
| [`chat`](chat)                 | plug-in | The transcript view's data plane.                                                                                          |
| [`sessions`](sessions)         | plug-in | The per-subject session dashboard — the only page that enumerates sessions.                                                |
| [`sessionview`](sessionview)   | plug-in | A session-scoped read-only shared mirror, plus MCP-UI widget hosting.                                                      |
| [`interact`](interact)         | plug-in | The browser's authorized _act-on-a-session_ path.                                                                          |
| [`health`](health)             | plug-in | A public `/healthz`, shadowed in practice by the server's own short-circuit; proves the registry wiring accepts a real UI. |
| [`browserstart`](browserstart) | helper  | The one `reserve → create → adopt` sequence shared by both start routes. Authorization is deliberately **not** here.       |
| [`contenttoken`](contenttoken) | helper  | Short-lived HMAC capability tokens gating the cookieless sandbox endpoints.                                                |
| [`cspassets`](cspassets)       | helper  | Assembles nonce-authorized tags and _derives_ the CSP from what was registered.                                            |
| [`livemirror`](livemirror)     | helper  | Session-agnostic mirror building blocks shared by `chat` and `sessionview`.                                                |
| [`registry`](registry)         | helper  | The process-wide plug-in registry.                                                                                         |
| [`webassets`](webassets)       | helper  | `//go:embed` of the committed Vite bundles. See its README.                                                                |

## Invariants that bite

- **A manifest miss is a loud 500, never a blank page.** `serveRoute` refuses to
  render a document with a zero-value manifest entry — that would be a 200 with
  an empty `<div id="root">` and nothing in the logs.
- **A missing bundle omits the route rather than panicking**, so the page's
  `<script src>` 404s: visible and diagnosable rather than silent.
- **CSP is delivered as a header, never `<meta http-equiv>`** —
  `frame-ancestors` and `form-action` are ignored in a meta tag.
- **`style-src` carries both `'self'` and `'unsafe-inline'`** for the component
  library's runtime inline styles. Deriving `style-src` from the nonce would
  make the browser _ignore_ `'unsafe-inline'` and break every page.
- **`BaseContext` is load-bearing for logging.** Without it,
  `log.FromContext(r.Context())` falls back to a global delegating logger that a
  transitive `init()` poisons with a Nop sink before `main()` runs — every
  handler log would silently vanish.
- **`StrictOrigin`, not `SanitizeOrigin`, wherever the value becomes a URL the
  browser navigates to or frames.** `SanitizeOrigin` hands back
  `javascript:alert(1)` verbatim, and an empty base — a live state while
  `oap install` manages external access — yields a _shell-relative_ URL that
  collapses the two-origin split.
