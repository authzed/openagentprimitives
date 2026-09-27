# `pkg/platform/identity/passthroughlink`

Mints and verifies the HMAC-signed deep-links channelsd sends and identityd
consumes. A link is tamper-proof and expiry-gated: it scopes **what** the click
may do and for which session. It is _not_ identity proof — who the clicker is
comes from the channel kind's OIDC ceremony.

`link.go` owns the payload, the sign/verify pair, and the `Purpose` constants
(`artifact_view`, `cli_identity`, `admin_login`, `session_view`,
`credential_update`) that producers and verifiers share. `key.go` owns loading
the signing key.

## Subpackages

| Package                               | What it is                                                                                                                                                          |
| ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`viewlink`](viewlink/)               | Mints webd artifact-view deep-links from a `Signer` plus a live webd base URL.                                                                                      |
| [`sessionviewlink`](sessionviewlink/) | Mints `session_view` links: a long-lived (7-day), shareable bearer capability for the session-view page. Mint only — verification goes through the shared `Signer`. |

## Constraints

- **Every production reader of the signing-key Secret must call
  `DecodeHexKey`.** It is the single place the `MinKeyLen` (32-byte) floor is
  enforced. `hex.DecodeString("")` returns `([]byte{}, nil)` — a silent,
  publicly computable zero-length HMAC key — so the empty case needs its own
  arm. Three separate notions of "valid" is how that key became loadable twice.
- **The wire format is not a JWT.** Claim _names_ are JWT-style (`iss`, `aud`,
  `iat`, `nbf`, `exp`, `jti`, `sub`) so operators auditing the flow recognize
  the semantics, but the encoding is HMAC-SHA256 with a base64url body and a hex
  signature. No algorithm is negotiated in a header — that footgun is closed by
  construction.
- **Verify errors are typed and distinct**: `ErrExpired`, `ErrNotYetValid`,
  `ErrIssuedInFuture`, `ErrInvalidSignature`, `ErrMalformed`. The clock-skew
  ones exist so operators fix drift instead of chasing phantom "invalid link"
  reports.
- **The link never stands alone on a state-changing path.** identityd
  re-verifies the signed payload _and_ re-checks that the cookie subject matches
  `payload.Subject`.
