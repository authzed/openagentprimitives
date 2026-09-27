# `pkg/agent/runner/approval/summarizer`

The isolated LLM that writes the one-sentence "What this tool call will do" line
an approver reads, and the annotation summaries alongside it. It runs inside the
runner pod but is a **second, separate call** — not a method on the primary
agent LLM.

| File                                 | Holds                                                                                                   |
| ------------------------------------ | ------------------------------------------------------------------------------------------------------- |
| [`summarizer.go`](./summarizer.go)   | `Provider` interface, `Request`, and the `Cache` keyed on a request hash.                               |
| [`annotations.go`](./annotations.go) | `AnnotationRequest`, its system prompt, `FallbackAnnotationSummary`, and text clamping.                 |
| [`anthropic.go`](./anthropic.go)     | Anthropic provider, the system/user prompt construction, output parse+validate, and per-arg truncation. |
| [`openai.go`](./openai.go)           | OpenAI and OpenRouter providers over the same shape; `NewForProvider` picks one by name.                |
| [`fake.go`](./fake.go)               | Canned provider for tests.                                                                              |

## Why the isolation matters

The primary LLM's output can be influenced by indirect prompt injection. If the
primary authored this line, a compromised agent could talk an approver into
rubber-stamping an attack.

**What it sees:** tool name, the upstream MCP description and input schema (both
operator-signed and immutable), the actual args, and the resource type/id/
permission.

**What it does not see:** the primary LLM's chat history, the agent's own
justification, any tool output, or the runner's system prompt.

On top of that scoping the implementation enforces: args framed as user data,
output constrained to one JSON object, a 30-word cap, zero tools, a single round
trip, and every string arg value truncated to 200 chars.

Contrast with [`../../identityadvisor`](../../identityadvisor/), which is
deliberately given _wide_ context because its output is only advisory and a
human confirms it.

## See also

- [`pkg/agent/runner`](../../) — the turn loop.
