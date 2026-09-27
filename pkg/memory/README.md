# `pkg/memory` — storage, search, knowledge graph, and the append-only audit kinds

The agent-memory framework. Three layers stack here, each with its own pluggable
interface:

| Layer                | Interface               | Backends today                                                                                                                                       |
| -------------------- | ----------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Storage**          | `memory.Backend`        | [`inmem/`](inmem/), [`sqlite/`](sqlite/), [`postgres/`](postgres/), [`shadow/`](shadow/) (dual-write)                                                |
| **Ranked retrieval** | `memory.SearchProvider` | [`search/inmem/`](search/inmem/), [`search/postgres/`](search/postgres/), [`search/sqlite/`](search/sqlite/), [`search/graphiti/`](search/graphiti/) |
| **Knowledge graph**  | `memory.KGProvider`     | [`kg/graphiti/`](kg/graphiti/)                                                                                                                       |

All three are chosen by dependency injection in `internal/cmd/operator`, not by
a registry — there is exactly one concrete set per process, picked from env
(`POSTGRES_URI`, `EMBEDDING_ENDPOINT`, `GRAPHITI_ENDPOINT`,
`MEMORY_READ_SOURCE`).

`Local` ([`facade.go`](facade.go)) is the in-process `Memory` implementation: it
wraps a `Backend`, owns the per-`(Scope, Kind)` `ScopeHooks` materialization,
and fans out signal dispatch. `httpclient` speaks the same `Memory` interface
over HTTP, so the runner, `oap` and channelsd get the facade's semantics without
an in-process backend.

## `Query` and `Search` coexist deliberately

- **`Query`** is boolean filtering: tags, indexed content fields, time ranges,
  links. It returns what matches, unranked.
- **`Search`** is scoring. It delegates to `search.CompositeSearcher`, which
  fans out to every registered `SearchProvider` concurrently, merges with
  Reciprocal Rank Fusion, and post-filters through SpiceDB.

Neither subsumes the other. Do not "unify" them.

## Append-only kinds are Ed25519-signed into a hash chain

**This is the invariant a newcomer is most likely to break.**

The transcript (`turn`), `authz_decision`, `tool_session` and the `*_audit`
kinds declare `Retention{AppendOnly: true}`. The `Local` facade enforces it:

- A `Put` that **changes** existing content returns `ErrAppendOnlyConflict`. A
  byte-identical re-put is idempotent (compared on _canonical_ form, so a
  postgres round trip does not count as a change).
- Per-entry `Delete` is **refused**. Scope-level deletion (retention, session
  GC) is still allowed.
- Every such entry carries a `Provenance` envelope — publisher, keyID, a
  per-`(scope, publisher)` monotonic seq, prevHash, and a signature over a
  canonical digest. Seq + prevHash form a hash chain per `(scope, publisher)`,
  making modification, fabrication, gaps, reordering and (with the tail anchor)
  truncation detectable.
- The facade **verifies on write**. Unsigned or forged append-only writes are
  rejected from token callers _and_ in-process writers.

Operational consequence: **`oap memory put` / `oap memory delete` of an
append-only kind is rejected.** Only the runner, the components and the operator
may write those kinds, and only through the signing path.
`oap audit verify <session>` walks every chain offline and exits non-zero on
hard findings.

See [`provenance/`](provenance/) and [`publisherkeys/`](publisherkeys/).

## Subpackages

| Package                                    | What it holds                                                                    |
| ------------------------------------------ | -------------------------------------------------------------------------------- |
| [`assetlimits/`](assetlimits/)             | One shared size ceiling for inbound attachments. Zero dependencies, deliberately |
| [`httpclient/`](httpclient/)               | `memory.Memory` over the operator's HTTP API                                     |
| [`httpsrv/`](httpsrv/)                     | The bearer-authed HTTP API serving a `memory.Memory`                             |
| [`inmem/`](inmem/)                         | In-memory `Backend` — structured filters only                                    |
| [`kg/`](kg/)                               | Knowledge-graph provider implementations                                         |
| [`kinds/`](kinds/)                         | The ~31 registered memory Kinds                                                  |
| [`memcopy/`](memcopy/)                     | Turn-anchored memory-copy primitive used by the restart/fork reconciler          |
| [`memtest/`](memtest/)                     | Backend test doubles, incl. the postgres-reshaping `RoundTripBackend`            |
| [`postgres/`](postgres/)                   | Postgres `Backend`                                                               |
| [`provenance/`](provenance/)               | Ed25519 signing + verification + chain heads for append-only entries             |
| [`publisherkeys/`](publisherkeys/)         | Publisher → keyID → Ed25519 pubkey store for component publishers                |
| [`search/`](search/)                       | `CompositeSearcher`, the RRF ranker, and the search providers                    |
| [`sentinel/`](sentinel/)                   | Single source of truth for how a memory sentinel error crosses the HTTP wire     |
| [`shadow/`](shadow/)                       | Dual-write `Backend` (inmem primary + postgres secondary) with switchable reads  |
| [`spicedbauthorizer/`](spicedbauthorizer/) | `memory.Authorizer` over SpiceDB's `memory_entry` type                           |
| [`sqlite/`](sqlite/)                       | SQLite `Backend` — a strict peer of postgres, not a degraded inmem               |
| [`tokens/`](tokens/)                       | Per-AgentSession bearer-token registry used by the HTTP handler                  |

Note: the `KGProvider` interface itself lives at [`kg.go`](kg.go) in this
package, not under `kg/` — `kg/` holds only the implementation.
