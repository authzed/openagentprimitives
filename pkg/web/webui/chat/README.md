# `pkg/web/webui/chat`

The transcript view's **data plane** — the `webui` plug-in (`"chat"`) serving
the session-scoped read, write, and websocket routes a conversation needs. It
generalizes the single-session `oap agent chat` TUI to N concurrent
conversations in one `webd`.

**It creates nothing.** Session creation lives behind the authorized start
routes in [`sessions`](../sessions) and [`agentui`](../agentui); this package's
session builder is deliberately adopt-shaped, never create-shaped. It also owns
no page — [`sessions`](../sessions) owns the chrome and renders this data.

## Authorization

Every route gates on **`agentsession:<ns>/<name>#interact@user:<canonicalID>`**.
There are two layers, on purpose:

1. **Route level** (`AuthAuthorized`) — one shared closure, runs before any
   handler body.
2. **Inside `Registry`** — a second per-operation check, plus a durable-read
   door for sessions no longer live in this process.

The gate is `agentsession#interact`, **never a started-by comparison**. Every
sibling session surface gates identically.

A check _error_ returns a typed `*webui.PageError` with **503**, never 403 — a
bare error would be collapsed to "forbidden" and tell the user the wrong thing.

## Transport: websocket, push-only

`GET /sessions/api/{ns}/{name}/ws` upgrades via gorilla. The browser **sends**
over `POST .../message`; the only thing read from the socket is used to detect
close.

- Authorization is re-checked **before** the upgrade, so an unauthorized caller
  never reaches an upgraded connection.
- Origin is pinned in `CheckOrigin`.
- Client frames are capped at 1 KiB; POST bodies at 64 KiB.
- Ping every 30s with a pong-extended read deadline, to catch half-open sockets.
- After attach, the connection asks for a **resurface** so a tab that connects
  after a prompt was published still sees it. Attach-then-ask ordering matters.
- **The per-connection sink never blocks.** Frames go onto a bounded (256)
  buffered channel drained by one writer goroutine; overflow drops the _oldest_
  frame. A synchronous write would let one slow tab stall delivery for every
  other session sharing the NATS subscription goroutine.

The other five routes are plain JSON: `GET .../detail`, `GET .../messages`,
`POST .../message`, `POST .../interrupt`, `POST .../decision`.

## Durability and rehydration

**Two authorize doors, deliberately different:**

| Door            | Used by                | Behavior                                                                                             |
| --------------- | ---------------------- | ---------------------------------------------------------------------------------------------------- |
| `authorize`     | resume / write paths   | On an in-memory miss, **rehydrates**. Refuses a terminal phase.                                      |
| `authorizeRead` | `/detail`, `/messages` | Needs no live entry — a _finished_ conversation still replays instead of 404-ing into a blank panel. |

**Rehydration rebuilds only the in-process half** (host, listener, health
watcher) for a session left by a previous `webd` or dropped by the idle reaper.
It creates no cluster objects, and reserves its slot _before_ the slow wiring so
two concurrent requests cannot build two health watchers.

The transcript itself is not in memory here — it is read from operator memory
through [`livemirror`](../livemirror), using webd's **read-only** memory token.
A missing token yields a logged **501**, never a silently empty transcript.

**Idle reaping is in-process only and MUST NOT delete the `AgentSession` CR.**
Deleting there would silently wipe every ended conversation's transcript and
audit trail. This is deliberately unlike the CLI TUI, whose ephemeral sessions
_are_ deleted on quit.

## This package is multi-user; keep it that way

Several details exist specifically to stop single-user assumptions creeping
back:

- **Caps are per subject** (16), with a separate process ceiling (256) — the two
  map to _different_ refusals (429 vs 503). A process-wide-only cap is
  shared-fate: one viewer's tabs would lock out everyone else.
- **Sender identity is derived per call, never captured at construction.** A
  live entry is shared across every subject holding `interact` — a participant
  may be first to rehydrate a session someone else started — so a
  construction-time identity misattributes every later message and decision.
- **Sessions are keyed by `(namespace, name)`.** A bare name would silently
  return the wrong transcript.
- **`sender_resolver.go` scopes the shared relay.** `browser.Host` ignores its
  `AgentSession` argument — true for the one-session TUI, false here. Without
  per-envelope scoping, every conversation's messages, plans, tool sessions, and
  approval prompts reach every other conversation's sink.

## Files

| File                 | What it holds                                                                              |
| -------------------- | ------------------------------------------------------------------------------------------ |
| `chat.go`            | Plug-in skeleton, routes, the prerequisite gate, and the registry singleton                |
| `deps.go`            | The `Deps` collaborator interface webd satisfies                                           |
| `handlers.go`        | The six HTTP handlers, including the websocket upgrade                                     |
| `registry.go`        | Live-session table, caps, idle reaper, relay wiring, the two authorize doors               |
| `session.go`         | In-process wiring: `browser.Host`, listener, health watcher                                |
| `sink.go`            | The per-connection non-blocking websocket emitter                                          |
| `sender_resolver.go` | Scopes the process-wide outbound relay to the right conversation                           |
| `transcript.go`      | Thin wrapper over `livemirror.ReadHistory`; aliases its types rather than redeclaring them |
| `identity.go`        | Subject → `ExternalIdentity`                                                               |

## One import edge is tested, not just convention

`TestChatPackageNeverImportsProvenance` asserts this package never imports
`pkg/memory/provenance`. A browser-facing process that can **sign** can forge a
session's audit chain; naming a key is fine ([`pkg/x/keyid`](../../../x/keyid)),
signing with one is not.
