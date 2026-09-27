# `pkg/web`

Everything served over HTTP (plus the one gRPC service that shares the group's
"a client reaches us over the wire" concern).

**The boundary rule:** a package belongs here when its job is _serving a request
to a client outside the process_ — routing, page assembly, authorization of an
HTTP request, response encoding, browser asset delivery. Domain logic behind
that surface belongs to the group that owns the domain: sessions to `pkg/agent`,
transports to `pkg/channels`, memory and audit to `pkg/memory`, identity to
`pkg/platform`. A handler here should read as _authorize, delegate, encode_.

## `web/` and `pkg/web/` are different things

There is a **root-level [`web/`](../../web)** directory. It is not this.

|                        | What it is                                                                                                                                                                                                                                                                                |
| ---------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **`web/`** (repo root) | The **TypeScript** side. A pnpm + Vite workspace: shared packages (`packages/agentui`, `packages/design`, `packages/runtime`), the Tailwind/Vite/Vitest config, and the build that produces the browser bundles. Carries its own `go.mod` sentinel so Go tooling's `./...` walks skip it. |
| **`pkg/web/`** (here)  | The **Go** side. The servers, handlers, authorization gates, and page assembly that deliver those bundles and answer their API calls.                                                                                                                                                     |

They meet at exactly one place: Vite writes its output into
[`webui/webassets/dist/`](webui/webassets), which
[`webui/webassets`](webui/webassets) `//go:embed`s. See that package's README —
`dist/` is committed build output and must never be hand-edited.

Per-feature TypeScript is **colocated** with the Go handler that serves it, in a
`ui/` directory (`webui/chat/ui/`, `adminui/ui/`, …). An `app.json` in a `ui/`
directory declares a bundle entry point; a `ui/` directory without one is a
component library imported by some other entry.

## Who serves what

| Binary                  | Uses                                                                                                                   |
| ----------------------- | ---------------------------------------------------------------------------------------------------------------------- |
| `internal/cmd/webd`     | [`webui`](webui) and its plugins, [`adminui`](adminui), [`browsersession`](browsersession), [`uibindings`](uibindings) |
| `internal/cmd/operator` | [`admind`](admind), [`gateway`](gateway), [`secretoutsrv`](secretoutsrv)                                               |
| `internal/cmd/runner`   | [`uiview`](uiview), [`uigrant`](uigrant) — the _agent_ side of agent-defined UI                                        |
| `cmd/oap`               | [`localtunnel`](localtunnel), [`viewurn`](viewurn)                                                                     |

The admin console is split across two binaries on purpose: **`adminui` (webd)
serves the console's HTML and JS and reverse-proxies `/admin/api/<rest>` →
`<admind>/admin/v1/<rest>`; `admind` (operator) is the API and holds the
authorization gate.** The browser never talks to `admind` directly.

## Subpackages

| Package                            | What it holds                                                                                                                                                                                                                                                                                                                    |
| ---------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`admind`](admind)                 | The admin console **backend API**, mounted by the operator: live sessions, audit, config, cost, cluster health, kill. SpiceDB-gated.                                                                                                                                                                                             |
| [`adminui`](adminui)               | The admin console **frontend** — React shell under `ui/`, login, and the reverse proxy to `admind`.                                                                                                                                                                                                                              |
| [`browsersession`](browsersession) | The single writer for the cluster objects a browser-channel `AgentSession` is made of: ephemeral `Channel`, owner-ref'd credentials `Secret`, the `AgentSession`, and the SpiceDB `started_by` relation. Readers it does not own (channelsd correlation, the operator's owner resolver, the chat registry) depend on that shape. |
| [`gateway`](gateway)               | A bidirectional gRPC byte-pump into a live process in a session's sandbox pod. **Not the Kubernetes Gateway API and not grpc-gateway** — see [its README](gateway/README.md).                                                                                                                                                    |
| [`localtunnel`](localtunnel)       | Exposes an in-cluster port as a public HTTPS URL for a cluster with no ingress, so OAuth `redirect_uri`s and webhook URLs resolve. `Tunnel` interface + provider registry, driven by `PublicEndpoint`; `ngrok` implemented, `stub` for tests.                                                                                    |
| [`secretoutsrv`](secretoutsrv)     | The operator endpoint a runner POSTs a captured secret-output value to. The **operator**, not the runner, writes the Secret — the runner has no Secret-write RBAC.                                                                                                                                                               |
| [`uibindings`](uibindings)         | Resolves an agent-UI binding _source_ to a value. Registry of source kinds: `actionstate`, `artifactref`, `memoryref`, `tool`.                                                                                                                                                                                                   |
| [`uicomponents`](uicomponents)     | The agent-UI component vocabulary: component schemas, declaration shape, prop/binding validation, and the view model handed to the browser.                                                                                                                                                                                      |
| [`uidemo`](uidemo)                 | A worked agent-UI demo (`leadflow`) used to exercise the stack end to end.                                                                                                                                                                                                                                                       |
| [`uiselect`](uiselect)             | The binding **selector language** — a deliberately tiny, total, non-executing path expression (`results[].properties`) that says which part of a source value fills a prop. No filters, wildcards, indices, or recursion.                                                                                                        |
| [`uiview`](uiview)                 | Merges an `AgentUI` CR plus the agent's stored fragments into the one declaration everything downstream reads: browser bootstrap, binding lookup, action lookup, live push, and the runner's write-time validation.                                                                                                              |
| [`uigrant`](uigrant)               | Computes, fail-closed, which tools an agent-defined UI may invoke directly from a browser — the intersection of what the bundle _requested_, what the tool's origin _permits_, and what the deployment _granted_. Three separate objects written by three separate people.                                                       |
| [`viewurn`](viewurn)               | The view-URN grammar (`urn:ap:view:<type>:<nss>`) naming the surface an inbound came from. **One `Parse`/`Format` pair in the codebase** — a drifted URN is both a Slack markup-injection and an LLM prompt-injection surface, because a Via string is rendered into a Slack message _and_ into the agent's context.             |
| [`webui`](webui)                   | The browser server: transcript/chat, agent-UI pages, artifact viewing, the session shell, and the embedded assets.                                                                                                                                                                                                               |

## Things to know before adding a handler

- **Authorize every request at the handler.** The browser surfaces
  (`webui/chat`, `webui/agentui`, `webui/sessions`) each check
  `agentsession#interact` on every request; nothing here relies on an ambient or
  upstream check.
- **Authentication is never ambient.** Handlers mounted onto a shared mux
  authenticate their own requests — the mux adds nothing. That is true of the
  operator's mux (`pkg/x/debug`) in particular.
- **Untrusted content is isolated, not trusted.** Anything agent- or
  user-authored that reaches a browser goes through the artifact host's
  origin/CSP isolation rather than being rendered into a first-party page. See
  [`webui/artifactview`](webui/artifactview).
- **`dist/` is generated.** Never hand-edit it; `mage web:check` fails on drift.
