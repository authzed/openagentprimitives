# `pkg/web/webui/sessions`

The `webui` plug-in (`"sessions"`) serving the **per-subject session
dashboard**: a single page, `GET /sessions`, listing every `AgentSession` the
viewer holds `interact` on.

**It is the only page that enumerates sessions across the viewer's own
standing.** It owns the chrome, the sidebar, the view-kind decision, and one of
the two start routes. The content region is filled by another plug-in —
[`chat`](../chat) for a transcript, [`agentui`](../agentui) for an
agent-defined view.

## Routes

| Route | Auth | Note |
| ----- | ---- | ---- |
| `GET /sessions` | `AuthLoginIfNecessary` | **`Authorize` is nil on purpose** — the list itself is the authorization boundary |
| `GET /sessions/api/sessions` | `AuthAuthenticated` | |
| `POST /sessions/api/start` | `AuthAuthenticated` | Mounted only when a start dep is wired |

## Three different authorization questions

| Question | Call | Object |
| -------- | ---- | ------ |
| *Which sessions may I see?* | `LookupInteractableSessions` | `agentsession` / `interact` (lookup form) |
| *May I open this one?* | `CheckInteract` | `agentsession:<ns>/<name>#interact` |
| *Which classes may I start from scratch?* | `LookupStartableClasses` | `agentclass` / `start_session` |

Sidebar reads use `fullyConsistent=false`; the start gate uses `true`.

The startable-class lookup asks about `agentclass`, not `platform`, even though
`platform#start_session` is the only populated arm today — the permission
resolves through `platform->start_session`, and asking the resource keeps the
question right when per-class grants appear.

**The start route's authorization is derived, not a declared permission.** A
viewer may start `(namespace, class)` iff they may interact with at least one
existing session of that class in that namespace. **The pair is checked, never
the bare class**, it is recomputed server-side at request time with
`fullyConsistent=true`, and the browser's offered list is never an input. An
*incomplete* standing list produces 503, never a refusal — a truncated lookup
must not read as "you may not".

`CheckInteract` failures return a typed `*webui.PageError` with a 500 status
rather than a bare error, which would be collapsed to 403.

## Files

| File | What it holds |
| ---- | ------------- |
| `sessions.go` | Plug-in skeleton and routes |
| `deps.go` | The `Deps` collaborator interface |
| `page.go` | `shellProps` and the page build, including the per-selection `CheckInteract` |
| `list.go` | Builds the session list and derives startable classes |
| `view.go` | Dispatches the content region: agent-UI, chat, or unavailable |
| `start.go` | The derived-authorization start handler |
| `start_workshopcap.go` | The per-starter builder-workshop cap the start route refuses at |

The chat view's props are deliberately minimal (`{ns, name}`) because the
transcript is read through the [`chat`](../chat) data plane, gated on the *same*
`interact` standing the page build already confirmed.

## Related

- [`browserstart`](../browserstart) — the shared `reserve → create → adopt`
  sequence this route and `agentui`'s both use. Authorization is deliberately
  not there.
- [`sessionview`](../sessionview) — one already-named session, read-only. This
  package is the only one that *enumerates*.
