# `pkg/platform/identityd`

The browser-facing identity surface: human login (OIDC, or the local password
IdP), the credential-link pages a parked session sends people to, the standing
`/my/accounts` portal, and the CLI login exchange.

It runs two ways from one route table. `routes()` is the **single**
registration, consumed both by the standalone mux (`Handler()`, used by tests
and the e2e harness) and by `webd`, which mounts it through the `webui.WebUI`
seam in `webui.go`. Adding a route in `routes()` mounts it on every surface, so
the two cannot drift.

## Layout

| Area                     | Files                                                                                                                                            |
| ------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| Server + wiring          | `server.go` (Deps, mux, `routes()`), `webui.go` (the `webui.WebUI` registration webd mounts)                                                     |
| Login                    | `handlers_oidc_login.go`, `handlers_oidc.go`, `handlers_idp.go`, `handlers_password.go`, `handlers_admin_login.go`, `idp_loader.go`              |
| Credential linking       | `handlers_link.go` (`/link`, `/link/submit`), `handlers_oauth.go` (`/link/oauth/…` + callback), `oauth_discovery.go`, `credupdate_agentowned.go` |
| Standing portal          | `handlers_portal.go` (`/my/accounts…`), `suggested.go`, `suggested_cache.go`                                                                     |
| CLI                      | `handlers_cli.go` (`/cli/login`, `/cli/exchange`)                                                                                                |
| Anti-replay / CSRF state | `state.go` (link flows), `oauth_state.go` (PKCE + MCPServer ref), `consumed_links.go`, `consumed_links_secret.go`                                |
| Rendered props           | `templates.go` (link menu), `templates_portal.go` (portal + one-credential form), `instructions.go`                                              |
| Session keepalive        | `handlers_heartbeat.go`                                                                                                                          |

## Subpackages

| Package           | What it is                                                                                                                                     |
| ----------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| [`icons`](icons/) | The favicon discovery + cache + serve pipeline behind `GET /icon/<credName>`. Has its own README.                                              |
| [`ui`](ui/)       | React/TSX sources for the four apps these handlers bootstrap: `link`, `linkform`, `portal`, `verifywarn`, with `shared/` between them. Not Go. |

## Constraints

- **The cookie alone is never trusted.** Every state-changing path re-verifies
  the HMAC-signed payload _and_ re-checks that the cookie subject equals
  `payload.Subject`. `/link` additionally gates on the session's starter
  canonical.
- **A person's credential and the agent's own credential are different writes.**
  A `credential_update` link covers both; routing the agent-owned case
  (permission `agentidentity#update_credential`, value belongs in the
  AgentIdentity's Secret) through the person path would overwrite the clicker's
  personal credential. `credupdate_agentowned.go` is that split.
- **The Go prop structs and the TypeScript prop interfaces must stay in sync.**
  `templates.go` ↔ `ui/link/types.ts`; `templates_portal.go` ↔
  `ui/portal/types.ts` and `ui/linkform/types.ts`. The JSON tags are the
  contract.
- **Single-use is enforced in one place** — `consumed_links.go`. The in-process
  store is backed by a Secret (`consumed_links_secret.go`) so a restart does not
  reopen a consumed link.
- **The OAuth state store is process-local by design.** A restart mid-flow
  invalidates every outstanding state, which fails the callback closed and costs
  only a re-link.
- **Production HTTP clients are `pkg/x/safehttp`.** `handlers_oauth.go` exposes
  `newOAuthHTTPClient` as a test seam and `e2e_seam.go` (built only under
  `-tags=e2e`) overrides it, so the SSRF guard on the production path is never
  relaxed.
- **`/healthz` is deliberately absent from `routes()`** — webd's health WebUI
  owns it, and `registerRoutes` adds it standalone-only.
