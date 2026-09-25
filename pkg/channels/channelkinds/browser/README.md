# `browser` — the web-chat channel kind

The channel kind for a session a **browser** is looking at: webd serves the
page, the page holds the socket, and this kind's `Listener`, `Sender` family
and `StreamDeltaSink` run in that host process rather than in channelsd
(`RelayedByChannelsd` is false).

A near-clone of [`local`](../local/), the TUI kind, which has the same
client-hosted shape with a terminal on the other end. The shared half — the
inbound `Listener` and the single-user `AudienceResolver` — lives in
[`../clienthosted`](../clienthosted/).

| File | Role |
| ---- | ---- |
| `kind.go` | The `channelkinds.Kind` implementation; returns inert no-op objects for interface compliance |
| `host.go` | `NewHost` — the real senders, bound to a caller-supplied `EventSink` |
| `events.go` | This kind's render-event vocabulary, as websocket JSON frames |
| `sender*.go` | One file per sub-channel sender (user echo, tool session, queued messages, live-view and session-view offers) |
| `listener.go`, `interaction.go`, `permission_request.go`, `session_owner.go`, `audience_resolver.go`, `stream_delta_sink.go` | The remaining capability implementations |

## Non-obvious constraints

- **The bare `Kind.NewListener` / `NewSender` return inert no-ops.** They exist
  only for interface compliance — registry lookup, `ValidateSpec`, the Channel
  controller's validation. Traffic-carrying objects come from `NewHost`, which
  binds an `EventSink` the generic `channelkinds.Deps` cannot carry.
- **The `EventSink` slot is what makes this transport-agnostic rather than
  browser-specific.** `pkg/web/webui/chat` supplies a websocket-backed sink;
  the `local` kind wraps a bubbletea program in the same slot.
- **The render-event vocabulary is deliberately not shared with `local`.** Each
  side translates an envelope into its own host's language — websocket JSON
  here, `tea.Msg` values there — so collapsing them would mean rewriting both
  consumers, not sharing a behaviour.
