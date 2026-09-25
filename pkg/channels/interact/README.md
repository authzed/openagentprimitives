# `interact` — the inbound interaction-kind registry

The generic "what did the human just do" seam that webd's `/interact` HTTP
endpoint dispatches to. Each `Kind` declares the SpiceDB permission it requires
and how to route the submitted payload.

| File                                           | Kind                                                               |
| ---------------------------------------------- | ------------------------------------------------------------------ |
| `interact.go`                                  | The `Kind` interface and the registry (`Register` / `Get` / `All`) |
| `user_message.go`                              | A typed message from the human                                     |
| `mcp_ui_action.go`                             | An action dispatched from an MCP-UI widget                         |
| `annotation_batch.go`                          | A batch of annotations                                             |
| `annotation_envelope.go`, `action_envelope.go` | The payload envelopes those kinds decode into                      |

## Non-obvious constraints

- **Registration is `init()`-time**, same shape as every other registry here. A
  kind file's `init` calls `Register(...)`.
- **Each kind names its own SpiceDB permission.** The endpoint does not hold a
  permission table; asking the kind is what keeps a new interaction from
  defaulting to whatever gate the previous one used.
- **The import surface is deliberately narrow.** This package imports only
  `channelkinds`, `channelevents` and `identity` — _not_ `pkg/agent/tool` — so
  webd can import it as cheaply as `pkg/agent/agentcaps`, without pulling in the
  tool graph. Keep it that way.
- **Not the same registry as [`channelinteractions`](../channelinteractions/).**
  That one describes _outbound prompt_ categories; this one describes _inbound_
  submissions from a view surface. The naming collision is why the other package
  carries the `channel` prefix.
