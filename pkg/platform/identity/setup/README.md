# `pkg/platform/identity/setup`

The credential-acquisition engine. Given a set of prefix-typed targets
(`cli:<toolkit>`, `mcp:<server>`, `toolspec:<spec>`), it resolves each through
[`authkind`](../authkind/), asks that target for its credential requirements,
picks a provider, runs a flow to obtain the credential, verifies it live against
the provider, and stores it.

Two flow implementations sit behind one contract: hand-written Go flows for
curated providers, and an LLM-driven agent for everything else.

## Subpackages

| Package | What it is |
| ------- | ---------- |
| [`builtins`](builtins/) | The `Flow` contract, the flow registry, live token verification, and the curated Go flows. |
| [`llmagent`](llmagent/) | The LLM-driven setup agent used for any requirement with no builtin flow. |

## Files

| File | What it holds |
| ---- | ------------- |
| `engine.go` | `Run` — the per-requirement loop, provider selection, the decline→re-prompt cycle (bounded by `maxVerifyAttempts`), and the builtin-vs-LLM branch. |
| `store.go` | `Store` — **the single chokepoint** that writes credential bytes and mutates the `AgentIdentity`. |

## Constraints

- **`Store` is the only writer.** Builtin flows call it; the engine wraps every
  per-requirement run in it. Do not add a second write path — a flow that
  persists its own Secret bypasses idempotency and verification.
- **Verification is live, and a rejected token is not silently kept.** When the
  provider rejects a token the user is shown the rejection and may decline to
  store it; the engine then re-runs the flow so they can correct it.
- **Credential material is masked in operator-visible output** via
  `pkg/x/credmask` (vendor prefix + last 4), never printed raw.
- The engine describes flows as screens and lets `pkg/cli/tui` present them — no
  flow owns terminal I/O.
