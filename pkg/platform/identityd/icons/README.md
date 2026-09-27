# `pkg/platform/identityd/icons`

The favicon pipeline behind `GET /icon/<credName>`: resolve a credential name to
a service site URL, discover that site's favicon, cache it, and serve either the
real icon or a deterministic generated fallback. Wired into [`identityd`](../)'s
route table as `Deps.IconHandler`; the route only appears when a handler is
supplied.

## Files

| File          | What it holds                                                                                                                                                                             |
| ------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `handler.go`  | The `http.Handler` for `GET /icon/<credName>`, including the credential-name validation.                                                                                                  |
| `resolver.go` | Cache → discovery → fallback ordering, with `singleflight` collapsing concurrent misses. Resolves the per-credential site URL from `MCPServer`s first, then the embedded toolkit catalog. |
| `discover.go` | Fetches the favicon: parse `<link rel="icon">` candidates from the HTML, rank by format then size, fetch the best; fall back to `<siteURL>/favicon.ico`.                                  |
| `cache.go`    | Concurrent-safe in-process LRU keyed by credential name, bounded by `Cap`, with separate positive and negative TTLs.                                                                      |
| `fallback.go` | `FallbackSVG` — a 64×64 rounded square tinted from `sha256(credName)` with the credential's first character centered.                                                                     |
| `types.go`    | `Entry`: bytes, content type, ETag, fetch time, and the `Negative` flag marking a fallback.                                                                                               |

## Constraints

- **`Discoverer.HTTPClient` must be `pkg/x/safehttp.Client` in production.** It
  follows an operator-supplied site URL. Only tests substitute a plain client,
  against `httptest` servers.
- **The route is deliberately public — no cookie gate.** The cookie protects
  rendered HTML; an icon leaks no more than the operator-supplied `SiteURL`
  already on the CRD.
- **It always answers 200 with bytes** — a real favicon or the generated SVG. A
  400 is reserved for a credential name that fails validation.
- **`FallbackSVG` must stay deterministic.** Identical bytes for an identical
  credential name is what lets a user build recognition across the portal, Slack
  Home, and DM surfaces. Changing the hue derivation reshuffles every icon.
- Fallback entries get the shorter negative TTL, so a site that gains a favicon
  is picked up without a restart.
