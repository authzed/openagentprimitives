# `pkg/agent/llm/openaicompat`

The OpenAI Chat-Completions wire translation, shared by every OpenAI-compatible
provider — [`openai`](../openai/) and [`openrouter`](../openrouter/). It is not
itself a `Provider` and registers nothing.

| File | Holds |
| ---- | ----- |
| [`translate.go`](./translate.go) | `BuildParams` (neutral `llm.Request` → SDK params) and `TranslateResponse` (SDK completion → `llm.Response`), including tool_use/tool_result ↔ tool_calls. |
| [`stream.go`](./stream.go) | `RunStream` — the streaming accumulate loop; also reads the mid-stream raw-JSON usage/cost field. |
| [`stream_translate.go`](./stream_translate.go) | `StreamState` + `TranslateStreamChunk`: SDK chunk → neutral `llm.StreamEvent`s. |
| [`client.go`](./client.go) | `NewClient(apiKey, baseURL, headers)`. |
| [`doc.go`](./doc.go) | Package doc. |

## Constraints

- **Provider-specific concerns stay in the provider package** — base URL, extra
  body fields, reported dollar cost. Anything that differs between `openai` and
  `openrouter` does not belong here.
- **`RunStream` depends on SDK internals** to read the raw usage/cost JSON.
  Re-verify it against the vendored `github.com/openai/openai-go/v3` source on
  any SDK upgrade.

## See also

- [`pkg/agent/llm`](../) — the neutral types and the provider registry.
