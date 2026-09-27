# `pkg/platform/nats`

The credential and connection layer for the control-plane bus. The AP control
plane runs over an authenticated, TLS NATS bus; this package is the single
source of truth for the credential shapes: decentralized-JWT identity generated
once per install, per-client user-JWT minting with subject grants, the CA and
server cert material, and the connection helper every component dials with.

The bus itself is deliberately **not** pluggable — it is substrate, like the API
server. Do not add an abstraction over it speculatively.

## Files

| File         | What it holds                                                                                                              |
| ------------ | -------------------------------------------------------------------------------------------------------------------------- |
| `mint.go`    | `Identity` (operator + account + account signing key, generated once by `oap install`) and per-client `UserGrant` minting. |
| `tls.go`     | `TLSMaterial` — the install-time CA plus a NATS server cert/key.                                                           |
| `connect.go` | The dial helper, including the connection lifecycle handlers.                                                              |

## Subpackages

| Package                 | What it is                                                                                                                                                                  |
| ----------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`subjects`](subjects/) | The subject grammar. Every subject any component publishes on or subscribes to is spelled here once, and both the concrete and the wildcard form derive from that spelling. |
| [`natstest`](natstest/) | Answers, for tests, "does the real server permit this subject under this grant?" by running an embedded `nats-server` with the same trust material.                         |

## Constraints

- **An empty `PubAllow` is publish-_anywhere_, not deny-all.** This is the trap
  `natstest` exists for: `assert.Empty(g.PubAllow)` asserts the opposite of what
  it looks like, and `assert.NotContains(PubAllow, "…in.interaction_decision")`
  passes against a grant of `ap.session.<ns>.<name>.>` that permits exactly that
  subject. **Assert grants by observation through `natstest`, never by string
  matching.**
- **Compose subjects through [`subjects`](subjects/), never by concatenation.**
  A subscriber wildcards the two session tokens with `AnySession` and then calls
  the _same_ leaf method its publisher calls, so a subscription pattern and the
  subject it must match cannot drift.
- **`GenerateIdentity` is called once, by `oap install`**; idempotency is the
  caller's concern. The seeds are secret; the JWTs and public keys are not.
- **The connection's lifecycle callbacks are load-bearing, not diagnostics.** A
  disconnected subscriber means the server drops envelopes for want of a
  subscriber while the publisher's `Publish` still returns nil — nothing
  upstream errors, so nothing upstream logs. These handlers record the outage
  _duration_, which is what an operator correlating "the agent never replied"
  needs.
