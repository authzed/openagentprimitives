# `local` — the `oap` chat TUI channel kind

The channel kind for a session a **terminal** is looking at. `oap` hosts the
page; this kind's `Listener`, `Sender` family and `StreamDeltaSink` run in that
process rather than in channelsd (`RelayedByChannelsd` is false).

A near-clone of [`browser`](../browser/), which has the same client-hosted shape
with a browser tab on the other end instead of a terminal. The shared half — the
inbound `Listener` and the single-user `AudienceResolver` — lives in
[`../clienthosted`](../clienthosted/).

| File                                                                                                                         | Role                                                                                         |
| ---------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------- |
| `kind.go`                                                                                                                    | The `channelkinds.Kind` implementation; returns inert no-op objects for interface compliance |
| `host.go`                                                                                                                    | `NewHost` — the real senders, bound to a caller-supplied `EventSink` (a bubbletea program)   |
| `events.go`                                                                                                                  | This kind's render-event vocabulary, in bubbletea's language                                 |
| `inert.go`                                                                                                                   | _What_ is dangerous: ANSI/OSC/control-character stripping                                    |
| `inert_sink.go`                                                                                                              | _Where_ the sweep runs: the single `Emit` door                                               |
| `sender*.go`                                                                                                                 | One file per sub-channel sender                                                              |
| `listener.go`, `interaction.go`, `permission_request.go`, `session_owner.go`, `audience_resolver.go`, `stream_delta_sink.go` | The remaining capability implementations                                                     |
| `NOTES.md`                                                                                                                   | The design note behind the inert-sink seam                                                   |

## Non-obvious constraints

- **The bare `Kind.NewListener` / `NewSender` return inert no-ops.** They exist
  for interface compliance — registry lookup, `ValidateSpec`, the Channel
  controller's validation. The objects that actually carry traffic come from
  `NewHost`, which binds an `EventSink` the generic `channelkinds.Deps` cannot
  carry.
- **The escaping sweep is structural, not a habit.** lipgloss preserves ANSI in
  the strings it renders, so untrusted text handed to the sink verbatim is
  _executed_, not merely displayed — cursor control can overwrite the lines a
  user reads a decision off. `Host` is the sole construction point of every real
  sender, all built from one `Host.sink`, and `NewHost` wraps the caller's sink
  once. The senders' `sink` field is typed `*inertSink` rather than `EventSink`,
  so `&localSender{sink: someRawSink}` **does not compile**. Do not widen that
  field type, and do not add a sender built outside `NewHost`.
- **An enumeration of sinks would be the wrong instrument.** It is correct only
  for the set that existed the day it was written; nothing about adding a render
  event or a payload field makes anyone open `inert.go`. The door sits at the
  boundary every render event must cross instead.
