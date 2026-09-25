# `pkg/authz/extract`

The entity-extraction seam used by session binding: given a user message and the
entity types an AgentClass declares, produce the concrete entities to bind into
the session scope.

`Provider` is the interface; per-provider implementations live in subpackages.
The root defines only `Provider`, `ExtractInput`, `ExtractedEntity`, and
`SessionBinding` — no LLM SDK.

| Package | Backend |
| ------- | ------- |
| [`anthropic`](anthropic/) | The default. Backed by the shared `pkg/agent/llm` `Provider` seam rather than a direct SDK call, so it reuses the agent loop's provider wiring. Defaults to Claude Haiku for low-latency extraction; the model is configurable per `New()` call. |

Extraction is not free, so the runner does not always call it: `authz.Prefilter`
([`../prefilter.go`](../prefilter.go)) is a cheap pass that skips extraction
when no declared entity type could plausibly appear in the message.
