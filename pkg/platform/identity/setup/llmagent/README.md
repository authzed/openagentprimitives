# `pkg/platform/identity/setup/llmagent`

The LLM-driven setup agent, used for any credential requirement that has no
builtin Go flow. It runs a bounded agent loop (`MaxTurns`) over a small,
purpose-built tool set: read the provider's docs, open a browser, run an
allowlisted shell command, ask the user, and finally store the credential.

## Files

| File             | What it holds                                                                                                                                                                                                                       |
| ---------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `agent.go`       | The loop: builds the tool set, drives the provider, enforces the turn budget.                                                                                                                                                       |
| `prompt.go`      | `BuildSystemPrompt` — role, provider prompt, docs URL, user intent, token shape, shell allowlist, stop conditions, in that order. Also serves the inline-prompt path where a toolkit supplies its own prompt instead of a provider. |
| `seams.go`       | `LLMProviderFactory` — the DI seam. Production is `pkg/agent/llm/anthropic`; tests inject a fake and restore `DefaultProviderFactory`.                                                                                              |
| `verifystore.go` | The verify-then-store callback the `store_credential` tool lands in, shared with [`../builtins`](../builtins/)'s verification.                                                                                                      |

## Subpackages

| Package           | What it is                                                      |
| ----------------- | --------------------------------------------------------------- |
| [`tools`](tools/) | The tool implementations the agent exposes. Has its own README. |

## Constraints

- **Storing still goes through the setup engine's single chokepoint.** The agent
  does not write Secrets; `store_credential` calls into `builtins`' store seam,
  which verifies before persisting.
- **The loop is budgeted.** `MaxTurns` is a `var` purely so tests can force
  budget exhaustion — do not raise it to work around a flow that will not
  converge.
- **`defaultModelID` tracks `pkg/agent/llm/anthropic`'s default.** Bump the two
  together.
- Status lines tolerate a nil writer: this runs inside a tool callback where the
  caller may have no terminal, and a missing one must not turn a verification
  notice into a panic mid-store.
