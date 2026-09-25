# skillbundle

Content-addressed store for **skill bundles** — the gzipped tar of a skill
directory (`SKILL.md` plus `scripts/`, `assets/`, …). Keyed by content digest,
so a bundle is cached once per (repo, sha, skill).

| Package                 | Backend                                                                                                                                                   |
| ----------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`memory`](memory/)     | Process-local. Not durable across pod restarts and not visible to other pods; suitable for the controller's own materialization use and for tests.        |
| [`postgres`](postgres/) | Durable and shared: a single `skill_bundle` table keyed by digest. Takes a `*pgxpool.Pool` directly, so the caller may share a pool or construct its own. |

[`store.go`](store.go) holds the three-method `Store` interface (`Put`, `Get`,
`Has`) and `ErrNotFound`. [`tar.go`](tar.go) builds the archives.

## Constraints

- **`TarGz` is deliberately deterministic** — paths sorted, fixed mode, zero
  mtime — so identical inputs always produce identical bytes and therefore share
  a digest. Introducing any volatile field (a real mtime, a uid) would break
  content addressing.
- `Put` is idempotent: re-putting the same digest is not an error.

Part of [`pkg/tools`](../README.md). See the root
[`README.md`](../../../README.md) and [`AGENTS.md`](../../../AGENTS.md).
