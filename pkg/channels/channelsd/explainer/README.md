# `explainer` — why this session needs these credentials

Produces a `{what, why}` pair describing why an agent session needs the user to
link specific credentials. channelsd's `credential_request` watcher uses it to
render a richer prompt than the operator's static template.

| Package          | What it holds                                |
| ---------------- | -------------------------------------------- |
| [`fake/`](fake/) | A deterministic `Explainer` for tests and CI |

## Prompt-injection boundary — read before changing an input

This is a **second, isolated LLM call**, not a method on the primary agent LLM.
The LLM here sees only:

- the human's initiating message (attacker-controlled — fenced in XML delimiters
  with close-tag escaping),
- static credential metadata (names and provider labels from the AgentClass and
  MCPServer specs),
- the AgentClass display name.

It **never** sees the primary LLM's chat history or output, tool outputs (the
primary injection surface), or any other dynamic LLM output. Widening the input
set is a security change, not a quality tweak.

## Failure is not optional to handle

On any failure — LLM error, parse failure, empty response — callers **must**
fall back to the operator-stamped static explanation, so `credential_request`
still works in degraded mode. The static template is the safe path, and it
stays.

Tests script `fake.Response` / `fake.ResponseErr` before the call and assert on
`fake.Calls` afterwards, which is how the injection boundary is verified — that
no agent output crept into the prompt.
