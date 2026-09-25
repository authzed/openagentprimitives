# `pkg/platform/identity/federation`

Derives an upstream resource access token from a user's **enterprise identity**,
rather than from a credential the user pasted in. The root declares the `Minter`
seam and its request/response shapes; the exchange itself lives in
[`idjag`](idjag/).

This is the enterprise-managed-auth client seam. It is DI'd — exactly one
concrete is chosen by the binary at startup, like `llm.Provider` — with a fake
for tests.

## Subpackages

| Package           | What it is                                                                                                                                                                                                                    |
| ----------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`idjag`](idjag/) | The vendor-neutral ID-JAG minter. Leg 1: RFC 8693 token-exchange at the IdP yields an ID-JAG audienced to the resource. Leg 2: present it as a JWT authorization grant at the resource's AS (discovered via RFC 9728 → 8414). |
| [`fake`](fake/)   | A deterministic `Minter` for tests. Returns a token derived from the request, or a configured error.                                                                                                                          |

## Constraints

- **A nil `Minter` reaching a federated credential is a fail-closed
  configuration error, never a panic.** Callers must check and surface, not
  assume.
- **Token bytes must never be logged.** The user's IdP token arrives wrapped in
  [`sensitive.SensitiveValue`](../sensitive/); keep it wrapped.
- **Errors must name which leg failed.** A two-leg exchange that reports only
  "federation failed" is undiagnosable against an unfamiliar IdP.
- Both legs go through the SSRF-guarded HTTP client the caller supplies — the
  resource AS is discovered from a remote document, so the URL is not ours.
