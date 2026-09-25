# `pkg/x`

Single-purpose utilities with no better home.

**Keep it small.** A package here earns its place by being a leaf that several
unrelated groups need and that no group owns. The moment one grows a domain —
its own types, its own sub-packages, a concern a real group already owns — it
belongs in that group, not here. `pkg/x` is not a staging area and not a
miscellany that may keep growing.

This README exists so you can tell at a glance whether the helper you are about
to write already lives here. **Read the table before adding a utility anywhere.**

## What's here

| Package | Use it for | Key API |
| ------- | ---------- | ------- |
| [`besteffort`](besteffort/) | The log-and-continue idiom, for error paths where the caller genuinely cannot return the error (a `defer`, a goroutine, a fallback inside an error handler). Prepends `("err", …)` so failures are grep-able. | `Log(info, op, err, kvs…) bool` |
| [`browser`](browser/) | Opening a URL in the host's default browser (the `runtime.GOOS` `open`/`xdg-open`/`start` dispatch). Used by the mcp/oauth flow, several identity setup builtins, `open_url`, and the github channel wizard's App-manifest flow — none of which own the concern. | `Open(url) error` |
| [`credmask`](credmask/) | Redacting credential material for operator-visible output (CR status, setup-engine lines). Shows the vendor prefix + `****` + last 4 so an operator can still tell *which* credential it is. | `Mask(v) string` |
| [`debug`](debug/) | The operator's **production** HTTP mux — see the note below; the name misleads. Also mints/loads the operator's debug bearer token Secret. | `NewHandler`, `NewHandlerWithMemAuth`, `EnsureToken` |
| [`externalurl`](externalurl/) | Reading a service's externally reachable base URL *live*, from a ConfigMap that changes after pod start (an ngrok tunnel coming up). Polls rather than watches, deliberately — a literal GET on one named ConfigMap needs far less RBAC than a list+watch. | `NewProvider`, `Provider.Get()`, `Provider.Run(ctx)` |
| [`keyid`](keyid/) | Deriving the content address of an Ed25519 public key — the ID a signature carries so a verifier can look the key up. | `For(pub) string`, `DecodePubKey(b64)` |
| [`kindregistry`](kindregistry/) | The generic implementation behind the project's many string-keyed *kind* registries. Each domain registry wraps a private `*Registry[T]` from here rather than re-implementing the map + mutex + duplicate-key panic. | `New(label, keyOf)`, `Register`/`Get`/`All`/`Keys`/`Reset` |
| [`llmpricing`](llmpricing/) | Reading per-model prices without importing a provider SDK. A pricing-only projection of the canonical registry in `pkg/agent/llm/models`. | `Tables()`, `WithCacheRatios`, `WithoutCacheRatios` |
| [`safehttp`](safehttp/) | **Any outbound fetch of an attacker-influenceable URL**, and hardening any `http.Server`. See the note below. | `Client() *http.Client`, `HardenServer(*http.Server)` |
| [`stringsx`](stringsx/) | Small dependency-free string helpers that would otherwise be duplicated one line at a time. | `CapRunes(s, maxRunes)` |

## Two you must not route around

**`safehttp.Client()` is the only correct client for a URL you did not
author.** An LLM-emitted `fetch_url` target, an MCP server URL, an OAuth
endpoint discovered from a remote document — unguarded, all of them can reach
the cloud-metadata address, loopback, or an in-cluster Service. The client
resolves the host, refuses the request if *any* resolved IP is
private/loopback/link-local, then dials the concrete vetted IP, so the address
checked is the address connected to (this is what closes the DNS-rebinding
gap). Redirects are re-validated per hop. It is stdlib-only on purpose —
~29 files import it, so it must stay dependency-free.

`HardenServer` lives in the same package as the project's single home for HTTP
hardening. It sets only `ReadHeaderTimeout` and `IdleTimeout`, leaving
`ReadTimeout`/`WriteTimeout` at zero so streaming and SSE handlers are never
truncated.

**`keyid` is a leaf on purpose: naming a key must not require the ability to
sign with one.** Putting it next to the audit-log `Signer` in
`pkg/memory/provenance` would drag signing capability into the import graph of
every subsystem that merely needs to *name* a key — and a browser-facing
process that can sign can forge a session's audit chain.
`pkg/web/webui/chat` has a test guarding exactly that import edge.
`provenance.KeyID` and `provenance.DecodePubKey` delegate here, so there is one
definition of what a key ID is.

## `debug` is misnamed and is not a dev affordance

`pkg/x/debug` builds the mux the operator serves on `--debug-bind-address`
(default `:8082`). It is enabled on every install and is load-bearing:
`internal/cmd/operator` mounts the memory handler, `secretoutsrv`, and the
admin console (`pkg/web/admind`) on it, and the operator's readyz gate blocks
Ready until the listener accepts. Disabling it to "harden" a deployment takes
the cluster down.

Authentication is bearer-token **per route**, never ambient — and each mounted
handler authenticates its own requests. This mux adds no auth to what it
forwards.

It is imported by exactly one binary (`internal/cmd/operator`). Being an
HTTP-serving concern, it sits oddly here rather than under `pkg/web`.
