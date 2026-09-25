# `pkg/platform/identity/broker`

The token broker: the single seam that turns credential **descriptors** into
injectable tool tokens (env vars and HTTP headers). The root declares the
interface and its plain-data request/response types — no `client.Client`, no CRD
root objects — so an out-of-process implementation behind the same interface
stays a non-breaking change.

Chosen by DI in `internal/cmd/runner` and `internal/cmd/operator`, not by a
registry: exactly one concrete is picked at startup.

## Subpackages

| Package             | What it is                                                                                |
| ------------------- | ----------------------------------------------------------------------------------------- |
| [`inproc`](inproc/) | The in-process implementation. Reads the backing Secrets directly and caches resolutions. |

## Constraints

- **Implementations are the only code that touches Secret bytes.** Everything
  upstream passes descriptors (secret-ref pointers plus injection shape) — see
  [`../credresolve`](../credresolve/).
- **`InvalidateSecret` is the credential-revocation entry point.** The
  `credential` invalidator on the `ap.revocation` bus calls it within tens of ms
  of the operator observing a credential removal or replacement. It covers
  `UserIdentity` and `AgentIdentity` credentials alike, because both resolve to
  a backing Secret. A no-op when nothing is cached.
- Keep inputs and outputs serializable. Adding a Kubernetes type to `Request` or
  `Resolution` closes the door on an out-of-process broker.
