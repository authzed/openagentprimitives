# `pkg/platform/artifactstore`

The durable artifact storage abstraction the operator uses for tool
stdin/stdout/stderr and input/output artifact handoff. The root declares the
`Store` interface and the opaque, scheme-prefixed `Ref` type; the concrete
implementation lives in [`blob`](blob/).

A `Store` is chosen by DI at startup from `ARTIFACT_STORE_URL` — there is no
registry here, because the binary picks exactly one.

## Subpackages

| Package | What it is |
| ------- | ---------- |
| [`blob`](blob/) | `Store` over `gocloud.dev/blob`: one implementation across `gs://`, `s3://`, `azblob://`, `file://` and `mem://`. The scheme is blank-imported per backend. |

## Constraints

- **`Ref` is opaque and store-relative.** `Key(ref)` is the inverse of `Put`'s
  ref construction; a foreign ref (different scheme, bucket, or base path)
  returns an error naming both bases rather than silently resolving.
- **`Put` must read to EOF before returning**, and `Delete` is idempotent — no
  error when the object is already gone.
- `Get`/`Exists` return `ErrNotFound` for an unresolvable ref; callers should
  match on it rather than on message text.
- `List` pages with an opaque continue token; `""` for `next` means last page.
