# `pkg/platform/identity/idp/oidckind`

The generic OIDC [`idp.Kind`](../): issuer discovery, authorization-code
exchange, and ID-token verification via `go-oidc`. Registered as `oidc` from
`init()`.

`Options` (extra authorize params, a `VerifyClaims` callback) is what lets a
preset kind reuse this flow — [`googlekind`](../googlekind/) is exactly that
preset, not a fork.

## Files

| File | What it holds |
| ---- | ------------- |
| `oidc.go` | The `Kind`, `Options`, and the `Provider` implementing `Begin`/`Complete`. |
| `wizard.go` | The `oap idp setup oidc` flow, built on shared screens from [`../idpscreens`](../idpscreens/). `KeyIssuer` is part of the wizard's public vocabulary. |

## Subpackages

| Package | What it is |
| ------- | ---------- |
| [`oidctest`](oidctest/) | A fake OIDC issuer over `httptest.Server` — discovery, JWKS, token exchange with a test RSA key — used by both this kind's and google's tests. **Not production-safe**: it panics rather than returning errors. |

## Constraints

- Outbound calls use the SSRF-guarded client (`pkg/x/safehttp`): the issuer URL
  is operator-supplied, and discovery follows it.
- `Federation` in `idp.Config` requests `offline_access` so the refresh token is
  captured at login; the resulting `TokenSet` is plaintext at this boundary and
  identityd writes it straight into a Secret.
- A preset kind must re-check its own claims server-side. An authorize-URL hint
  (`hd=`) is UX only and is trivially removed by the user.
