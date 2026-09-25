# `pkg/agent/llm`

The provider-neutral model interface. The runner speaks only these types; each
provider subpackage translates them to and from a vendor SDK.

| File                               | Holds                                                                                                                                             |
| ---------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`llm.go`](./llm.go)               | `Provider`, `Request`/`Response`, `Message`, `ContentBlock`, `ToolDef`, `ToolUseBlock`/`ToolResultBlock`, `Usage`, `ModelPricing`, `StreamEvent`. |
| [`capability.go`](./capability.go) | `Capability` / `CapabilitySet` — what a given model can do (e.g. native file in/out).                                                             |
| [`mimeset.go`](./mimeset.go)       | `MIMESet` — which MIME types a model accepts natively, and as which block type.                                                                   |
| [`routing.go`](./routing.go)       | OpenRouter's dynamic-routing subconfig, carried neutrally on `Request`.                                                                           |

`ContentBlock.Cacheable` is the portable prompt-cache hint: a provider maps it
to its native mechanism or ignores it.

## Subpackages

| Package                           | Purpose                                                                                                                              |
| --------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| [`providers`](./providers/)       | Name→`Provider` factory registry. Providers `Register` in `init()`; binaries blank-import the ones they need.                        |
| [`models`](./models/)             | Provider-neutral registry of built-in model metadata: per-token pricing and capability flags. Imports only `pkg/agent/llm` — no SDK. |
| [`anthropic`](./anthropic/)       | Anthropic Go SDK adapter.                                                                                                            |
| [`openai`](./openai/)             | OpenAI Go SDK (Chat Completions) adapter.                                                                                            |
| [`openrouter`](./openrouter/)     | OpenRouter adapter over the same Chat Completions wire, plus routing config and reported cost.                                       |
| [`openaicompat`](./openaicompat/) | The Chat Completions wire translation shared by `openai` and `openrouter`.                                                           |
| [`fake`](./fake/)                 | Scripted provider for tests.                                                                                                         |

## Constraints

- **Add a provider by registering it**, never by branching. The provider name
  comes from a CRD string (`EffectiveSettings.Model.Provider`), which is exactly
  the case the registry pattern exists for. `providers.New` errors on an unknown
  name.
- **New per-model facts go in [`models`](./models/)** — pricing and capability
  flags have one table. `pkg/x/llmpricing` projects from it; do not start a
  parallel map.
- **`fake` is not init-registered.** Tests wire it directly.

## See also

- [`pkg/agent`](../) — group overview.
