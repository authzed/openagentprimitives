# `search` — ranked retrieval

`Memory.Search` delegates to `CompositeSearcher`, which fans out to every
registered `SearchProvider` **concurrently**, merges the results with
Reciprocal Rank Fusion, and post-filters through SpiceDB.

| File | Role |
| ---- | ---- |
| `searcher.go` | `CompositeSearcher` and its `WithProviders` / `WithRanker` / `WithAuthorizer` options |
| `rrf.go` | `RRFRanker` — Reciprocal Rank Fusion (Cormack et al., 2009). `K` defaults to 60 |

## Providers

| Package | Capabilities |
| ------- | ------------ |
| [`inmem/`](inmem/) | Structured filters only (tags, time range); every hit scores 1.0 |
| [`postgres/`](postgres/) | tsvector FTS + pgvector cosine similarity + structured filters + link traversal |
| [`sqlite/`](sqlite/) | FTS + tags + time range. **No** field-content predicates — those are reported back via `SearchResult.DroppedFilters` |
| [`graphiti/`](graphiti/) | Graph-aware search over the Graphiti REST API, plus the ingest client |
| [`embedding/`](embedding/) | Not a provider — the OpenAI-compatible embedder the postgres provider uses for vector search |

## Non-obvious constraints

- **Each provider owns its own `SearchCapabilities()`, and they differ.** The
  composite does not paper over the gaps: a provider that cannot evaluate a
  filter must report it in `DroppedFilters` rather than silently returning
  results as if the filter had applied.
- **Vector search is conditional on the embedder.** The postgres provider
  reports `VectorSearch: true` only when an embedder was injected
  (`EMBEDDING_ENDPOINT` set); otherwise it degrades to FTS + structured.
- **`Search` does not replace `Query`.** `Query` is boolean filtering, `Search`
  is scoring; both are on `memory.Memory` deliberately.
- **Authorization is a post-filter, not a provider concern.** A provider returns
  what matched; the composite drops what SpiceDB says the caller may not see.
- **Providers are chosen by DI in `internal/cmd/operator`**, gated on
  `POSTGRES_URI` / `EMBEDDING_ENDPOINT` / `GRAPHITI_ENDPOINT` — there is no
  registry here.
