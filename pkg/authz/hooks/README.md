# `pkg/authz/hooks`

The concrete authz lifecycle hooks for the runner. Each hook is constructed with
its own dependency struct (DI) and registered into a per-`Loop`
`pipeline.Registry` by `buildPipeline` in `pkg/agent/runner`. Hooks share no
state with each other — everything a hook needs arrives through its `*Deps`
([`deps.go`](deps.go)).

| Hook | File | Pipeline point(s) |
| ---- | ---- | ----------------- |
| `McpTrust` | [`mcptrust.go`](mcptrust.go) | `PreToolCall` |
| `RevocationGuard` | [`revocation_guard.go`](revocation_guard.go) | `PreToolCall` |
| `ToolCallAuthz` | [`toolcallauthz.go`](toolcallauthz.go) | `PreToolCall` |
| `Scope` | [`scope.go`](scope.go) | `PreToolCall`, `PostToolCall` |
| `InfoLeakRead` | [`infoleakread.go`](infoleakread.go) | `PostToolCall` |
| `InfoLeakAudience` | [`infoleakaudience.go`](infoleakaudience.go) | `PostToolCall`, `PreResponse` |
| `Interact` | [`interact.go`](interact.go) | `InboundTurn` |
| `EntityBind` | [`entitybind.go`](entitybind.go) | `SessionStart`, `InboundTurn` |
| `ColdStartScope` | [`coldstartscope.go`](coldstartscope.go) | `SessionStart` |
| `SessionCleanup` | [`sessioncleanup.go`](sessioncleanup.go) | `SessionEnd` |
| `SessionFork` | [`session_fork.go`](session_fork.go) | `SessionFork` |
| `MetaagentReceived` / `Extract` / `Decide` / `Apply` | [`metaagent_received.go`](metaagent_received.go), [`metaagent_extract.go`](metaagent_extract.go), [`metaagent_decide.go`](metaagent_decide.go), [`metaagent_apply.go`](metaagent_apply.go) | one metaagent point each |

[`activation.go`](activation.go) answers which hooks are live for a given
session — `ActiveHooks(ActivationConfig) []HookDescriptor`, with each descriptor
carrying `Yes` / `No` / `Conditional`. `McpTrust` is the conditional one: it is
active only when the session actually has MCP tools.

## Ordering is code-owned, not configurable

[`order.go`](order.go) fixes hook order as integer constants — lower runs first.
**Operators do not reorder these; reordering security gates is a footgun.** A
hook that attaches at two points reuses the same constant at both, which is why
`Scope` runs third at `PreToolCall` and first at `PostToolCall`.

| Order | Hook | Why here |
| ----- | ---- | -------- |
| 10 | `McpTrust` | A distrusted server should fail as a trust error. |
| 12 | `RevocationGuard` | A revoked origin is denied before it consumes a rate slot or a breaker probe. |
| 15 | ToolGuard | A breaker-open denial must not burn a SpiceDB check or an approval ask. Its `PostToolCall` recorder reuses 15, so it runs before `Scope` and records the raw `Execute` outcome, not the post-gated one. |
| 18 | ContentGuard | Content that would trip a breaker is already denied; inspection runs on the survivors, before an approval ask is raised. |
| 20 | `ToolCallAuthz` | The SpiceDB permission check itself. |
| 30 | `Scope` | The sole dispatch-time enforcer of the session scope policy. |
| 40 | `InfoLeakRead` | |
| 50 | `InfoLeakAudience` | |

The ToolGuard and ContentGuard hooks themselves live in
[`../toolguard`](../toolguard/) and [`../contentguard`](../contentguard/); only
their order constants are here, so the whole sequence is readable in one file.

Separate, smaller order blocks in the same file cover `SessionStart`,
`SessionEnd`, `InboundTurn`, `SessionFork`, and the metaagent points.
