# llm

The `Provider` interface for a toolspec authoring loop, and its
implementations. Four methods, one per phase of that loop:
`SelectToolkit`, `GenerateSpec`, `GenerateTestCases`, `RefineSpec`.
[`provider.go`](provider.go) also owns the request/response types those phases
exchange, including the compact `ToolkitSummary` shape catalogs emit for the
selection prompt.

| Package | Purpose |
| --- | --- |
| [`promptlib`](promptlib/) | The shipping implementation: the shared prompt builders and JSON-response parsers, plus `NewProvider`, an adapter turning any [`pkg/agent/llm.Provider`](../../../agent/llm/) into this one. |
| [`fake`](fake/) | Fixture-driven provider for tests. `Select` and `Tests` look up by intent; `Generate` and `Refine` are consumed FIFO, so a test can script "invalid, then valid on retry". |

## Constraints

- **Chosen by DI, not by a registry** — exactly one concrete belongs per binary,
  but none is wired today: no binary constructs a `Provider` for this package.
  `promptlib.NewProvider` (the shipping adapter over the agent loop's own
  provider) has no production caller since the toolspec-authoring loop that
  wired it was retired; the interface and the adapter are retained for a
  future toolspec-authoring surface, exercised today only by `promptlib`'s own
  tests.
- **Prompts and response schemas live in `promptlib`, not in a backend.**
  Backends differ on transport, credentials and how they elicit JSON — some
  accept a response MIME type, others need instruct-then-parse — but they share
  the prompts, so a new backend adds a transport, never a second copy of the
  prompt.
- `promptlib` splices [`pkg/tools/cel`](../../cel/)'s `HelperDocs(ScopeToolspec)`
  into the authoring prompt, so what the model is told about CEL helpers comes
  from the same registry the compiler reads.

Part of [`pkg/tools/toolspec`](../README.md). See the root
[`README.md`](../../../../README.md) and [`AGENTS.md`](../../../../AGENTS.md).
