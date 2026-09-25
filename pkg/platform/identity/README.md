# `pkg/platform/identity`

Two concerns, one tree.

The **root package** is the single source of truth for *who a user is*: build a
`Principal`, call `.Canonical()`, and every SpiceDB write and `CheckPermission`
in the system agrees on the resulting id. `ids.go` and `subject.go` give the
surrounding types (`CanonicalUserID`, `RawExternalID`, `Email`, `Subject`) as
**defined** types, not string aliases, so passing a raw channel id where a
canonical is expected is a compile error rather than a production authz bug.

The **subpackages** cover *what that user (or agent) can authenticate as*:
acquiring credentials, storing them, resolving them at runtime, logging humans
in, and federating tokens upstream.

## Subpackages

| Package | What it is |
| ------- | ---------- |
| [`agentidentity`](agentidentity/) | Persists a replacement value for a credential an **AgentIdentity** owns — the agent's own shared secret. |
| [`useridentity`](useridentity/) | Persists **per-person** credentials on the cluster-scoped `UserIdentity` CR and its master Secrets, plus the naming helpers every component derives those names with. |
| [`authkind`](authkind/) | The per-prefix plug-in surface for AgentIdentity setup routing (`cli:`, `mcp:`, `toolspec:`, `toolbox:`). Setup only. |
| [`broker`](broker/) | The token broker: the single seam that turns credential descriptors into injectable tool tokens. Runtime resolution. |
| [`credresolve`](credresolve/) | Kind-agnostic mapping of `authkind.CredentialRequirement`s onto `CredentialDescriptor`s — secret-ref pointers and injection shape, never secret bytes. |
| [`credupdate`](credupdate/) | Decides whether an agent's claim that a credential has died justifies putting a credential-entry form in front of a human. |
| [`externaltoken`](externaltoken/) | Derives the SpiceDB object id and value hash for the external-token token-use authorization grant. |
| [`federation`](federation/) | Derives an upstream resource token from a user's enterprise identity via the ID-JAG two-legged exchange. |
| [`idp`](idp/) | The pluggable human-login identity-provider seam and its kinds (oidc, google, password, fake). |
| [`passthrough`](passthrough/) | The canonical resolver for "which credentials does this AgentClass require under `identityMode=userPassthrough`?" |
| [`passthroughcatalog`](passthroughcatalog/) | MCPServer-by-credential-name lookup, shared by identityd's link routing and channelsd's credential-request watcher. |
| [`passthroughlink`](passthroughlink/) | Mints and verifies the HMAC-signed deep-links channelsd sends and identityd consumes. |
| [`provider`](provider/) | The `/providers/<id>.yaml` library shape and the compile-time embedded catalog loaded from the repo-root [`providers/`](../../../providers/) directory. |
| [`refresh`](refresh/) | Runs RFC 6749 refresh-token grants against an oauth credential's stored `token_endpoint` and writes the result back to the Secret atomically. |
| [`sensitive`](sensitive/) | `SensitiveValue`: wraps secret bytes so every rendering path (`String`, `%v`, `%#v`, JSON) yields `[REDACTED]`. Bytes are reachable only via `UnderlyingValue()`. |
| [`setup`](setup/) | The credential-acquisition engine: builtin Go flows, the LLM-driven fallback agent, and the single `Store` chokepoint that writes credential bytes. |

## Constraints

- **Canonicalize before SpiceDB.** A raw email or channel id must never become a
  SpiceDB object id. `subject.go` is the *only* file permitted to write the
  `"user:"` literal; everyone else goes through `Subject.CanonicalUserID()` or
  `CanonicalUserID.Subject()`.
- **`Principal` has no usable struct literal.** Fields are unexported; construct
  through the constructors so the email-vs-synthetic decision lives in one place.
- **Synthetic subjects are opt-in.** `Canonical`/`Subject` return
  `ErrSyntheticSubject` for a principal with no verified email unless the caller
  explicitly calls `AllowSynthetic`. Silently minting one writes a subject no
  real grant resolves to.
- **`DecodeForDisplay` is for display only.** Comparisons, SpiceDB writes and
  credential resolution keep the canonical form.
- **Setup and runtime are separate seams.** `authkind` covers acquiring a
  credential; `broker` covers resolving one at tool-call time. Do not add
  resolution to a `Kind`.
