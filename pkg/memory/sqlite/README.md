# `sqlite` — the SQLite memory Backend

`memory.Backend` over an on-disk SQLite file. Mirrors the postgres schema in
SQLite dialect and reports the **same** `Capabilities()` — it is a strict peer
of postgres, not a degraded inmem.

| File           | Role                                                         |
| -------------- | ------------------------------------------------------------ |
| `backend.go`   | The `memory.Backend` implementation and its `Capabilities()` |
| `client.go`    | Opens (creating if absent) the DB in WAL mode                |
| `query.go`     | `Query` → SQL, using JSON1 and the link table                |
| `schema.go`    | The migration                                                |
| `envconfig.go` | `MEMORY_SQLITE_PATH`                                         |

## Non-obvious constraints

- **The driver must stay `modernc.org/sqlite`.** It is the CGO-free driver, so
  the binaries build with `CGO_ENABLED=0`. Swapping in a cgo driver breaks the
  build shape the images depend on.
- **Storage types differ from postgres deliberately.** `created_at` is INTEGER
  unix-nanoseconds UTC — exact and unambiguously sortable, unlike a trimmed
  RFC3339Nano string. `tags` is a JSON array in TEXT, filtered with `json_each`.
  `links` is denormalized JSON on the entry for round-trip **plus** a
  `memory_link` table for traversal. `provenance` is JSON TEXT, NULL on mutable
  kinds.
- **WAL mode plus `busy_timeout` is load-bearing.** WAL permits concurrent
  readers alongside a single writer; the timeout absorbs the operator's
  concurrent `Put`s from controllers and signal hooks.
- **The path is used verbatim** — no URL-escaping, which would mangle it.
- **This backend stores entries differently enough from postgres that a
  comparison correct on raw bytes can pass here and fail there.** See
  [`../memtest`](../memtest/).
