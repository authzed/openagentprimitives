# `pkg/agent/llm/anthropic`

`llm.Provider` over the official Anthropic Go SDK.

| File                                           | Holds                                                                                                                                           |
| ---------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| [`anthropic.go`](./anthropic.go)               | `Provider`, `New`/`NewFromEnv`, `Send`, `BuildParams` (neutral request → SDK params), and the response translation back.                        |
| [`stream_translate.go`](./stream_translate.go) | Per-event translation from the SDK's `MessageStreamEventUnion` to neutral `llm.StreamEvent`s, so a caller's `OnEvent` never sees the SDK union. |
| [`capabilities.go`](./capabilities.go)         | `Capabilities` and `NativeInputMIMEs` for a given model.                                                                                        |
| [`pricing.go`](./pricing.go)                   | `Pricing`, read from [`../models`](../models/).                                                                                                 |
| [`register.go`](./register.go)                 | `init()` → `providers.Register("anthropic", …)`.                                                                                                |

## Constraints

- **Cache control is explicit.** Cacheable system blocks, tool defs and message
  content blocks emit `cache_control: {type: "ephemeral"}`; everything else
  passes through unmarked.
- **The API key stays in the SDK client and is never logged.** The SDK's debug
  logging is off by default and this adapter does not enable it.

## See also

- [`pkg/agent/llm`](../) — the neutral types and the provider registry.
