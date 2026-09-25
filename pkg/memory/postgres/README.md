# `postgres` — the Postgres memory Backend

`memory.Backend` over a single `memory_entry` table, keyed
`(scope_kind, scope_id, kind, entry_id)`. Content is `JSONB`, `created_at` is
`TIMESTAMPTZ`, tags are `TEXT[]`, and the append-only provenance envelope is a
`JSONB` column.

| File | Role |
| ---- | ---- |
| `backend.go` | The `memory.Backend` implementation and its `Capabilities()` |
| `client.go` | The `pgxpool` connection wrapper |
| `query.go` | `Query` → SQL, incl. tag/field/time/link predicates |
| `schema.go` | The `CREATE TABLE`/`CREATE INDEX` migration, applied on connect |
| `envconfig.go` | `POSTGRES_URI` |

## Non-obvious constraints

- **A round trip reshapes entries, and callers must survive that.** Content
  comes back re-emitted from JSONB (keys reordered, whitespace different),
  `created_at` truncated to microseconds by the pgx `TIMESTAMPTZ` encoder, and
  empty tag/link lists mapped back to nil. Any equivalence or idempotency
  comparison must therefore work on the **canonical** form, never on raw bytes —
  the append-only re-put check depends on exactly this.
  [`../memtest`](../memtest/)'s `RoundTripBackend` reproduces the reshaping with
  no Docker so the default suite catches a byte-comparison bug that inmem and
  sqlite would both hide.
- **Setting `POSTGRES_URI` turns on more than this backend.** In
  `internal/cmd/operator` it also enables shadow dual-write and the postgres
  search provider.
- **The real-server tests are behind `mage test:postgres`,** not the default
  unit suite.
