# `pkg/web/admind/config`

The **contract and registries** behind the admin console's Config tab. This
package holds no CRD knowledge: it defines the presentation-agnostic shapes and
the two registries, and [`projectors/`](projectors) holds every concrete
implementation.

## Two registries, two interfaces

| | List | Detail |
| --- | --- | --- |
| Interface | `Projector` — `Resource() string`, `List(ctx, client)` | `DetailProjector` — `Resource()`, `Detail(ctx, client, ns, name)` |
| Returns | `[]ResourceRow` | `*ResourceDetail` (sections of fields/text/list) |
| Register | `Register` / `Get` / `All` / `Reset` | `RegisterDetail` / `GetDetail` / `AllDetail` / `ResetDetail` |
| File | `registry.go` | `detail.go` |

Both are backed by [`pkg/x/kindregistry`](../../../x/kindregistry), keyed on the
URL slug. **A duplicate or empty slug panics at process start** — a programmer
error that must surface at `init()`, not become a runtime 404.

`registry.go` also owns the shared list shapes `Badge`, `Count`, and
`ResourceRow`; `detail.go` owns `ResourceDetail`, `Section` (kinds `fields`,
`text`, `list`), `Field`, `Link`, and `ListItem`.

## The one contract that is easy to get wrong

**A detail projector returns `(nil, nil)` for "does not exist"** → the handler
renders 404. Any other error → 500. Returning an error for a missing resource
turns a normal not-found into a server fault.

## Why `projectors/` is a separate package

`config` owns the contract and the machinery; `projectors` holds the concrete
implementations and is **blank-imported by `admind.go`** so its `init()`
registrations run. Keeping them apart is what lets the handler dispatch by slug
without importing any CRD-shaping code.

## Related

- [`projectors`](projectors) — the concrete implementations. See its README for
  the slug list and the UI-schema sync invariants.
- [`pkg/web/admind`](..) — the API that mounts these behind `view_config`.
