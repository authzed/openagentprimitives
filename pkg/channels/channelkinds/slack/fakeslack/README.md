# `fakeslack` — in-memory Slack Web API simulator

Models the pieces of Slack the [`slack`](../) kind depends on — most
importantly the **message/thread tree** (`ts` ↔ `thread_ts` parent/reply
relationships) — so tests can assert *where* a reply lands (a threaded child vs
a top-level sibling), not merely that an option string contained `thread_ts`.

Satisfies the `slack` package's injected `slackClient` and `historyClient`
interfaces by structural typing — there is no shared interface declaration to
keep in sync.

| File | Role |
| ---- | ---- |
| `client.go` | The Web API surface: posting, editing, the message/thread tree |
| `query.go` | Test-side accessors for asserting on what was posted |
| `inbound.go` | Driving inbound events at the listener |
| `socketsource.go` | A `SocketSource` stand-in for Socket Mode |
| `files.go` | The file-upload surface |

## Non-obvious constraints

- **Assert against the thread tree, not against option strings.** The whole
  point of modelling `ts`/`thread_ts` is that a string assertion passes for a
  reply posted to the wrong parent. Use the `query.go` accessors.
- **Block assertions need a real-client round-trip.** `slack-go`'s
  `UnsafeApplyMsgOptions` omits blocks, so a test that inspects blocks through
  that path sees nothing.
