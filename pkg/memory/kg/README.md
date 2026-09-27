# `kg` — knowledge-graph providers

Implementations of `memory.KGProvider`.

| Package                  | Backend                                                                           |
| ------------------------ | --------------------------------------------------------------------------------- |
| [`graphiti/`](graphiti/) | [Graphiti](https://github.com/getzep/graphiti) over its REST API, backed by Neo4j |

**The `KGProvider` interface itself is not here** — it lives at
[`../kg.go`](../kg.go), in `package memory`, alongside `Backend` and
`SearchProvider`. This directory holds only implementations.

## Wiring

Chosen by dependency injection in `internal/cmd/operator`, gated on
`GRAPHITI_ENDPOINT`. When that variable is unset there is no KG provider, no
graphiti search provider, and no ingestion — the rest of the memory framework is
unaffected.

Graphiti runs as a sidecar and handles entity extraction, dedup and
contradiction detection **asynchronously**. Ingestion is signal-driven: the
[`kg_ingestion`](../kinds/kgingestion/) Kind's `ScopeHooks` reacts to
`lifecycle/turn.completed`, reads the turn content, and calls `Ingest`.
`KG_INGESTION_STRATEGY` selects `every_turn` (default), `content_gated` or
`batch`.

Graph-native queries (entity lookup, fact traversal, related entities,
communities) surface as `oap kg ...` and as the agent's `query_knowledge` tool.
