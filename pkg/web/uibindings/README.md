# `pkg/web/uibindings`

The seam a declared component prop's **binding** crosses to become a value.
Bindings are resolved **server-side, under the viewer's subject** — the browser
never holds a capability and never resolves a binding itself.

This package holds only the **contract**. The binding type itself lives in
[`uicomponents`](../uicomponents) (`Binding{Source, Ref, Args, Select}`); the
concrete resolvers live in the subpackages below.

```go
type Resolver interface {
	Source() string
	Resolve(ctx context.Context, d Deps, req Request) (Result, error)
}
```

`Request.Namespace` and `Request.Session` come **from the URL path, never the
request body**. `Subject` is the cookie-verified viewer.

## The registered sources

Dispatch goes through [`registry`](registry); **a consumer must never grow a
source-name switch of its own.** The live set is `registry.Keys()`.

| Package | `Source()` | What it resolves |
| ------- | ---------- | ---------------- |
| [`actionstate`](actionstate) | `"action"` | A declared action's current lifecycle **for this viewer**, read out of memory records. A read of a lifecycle — never an invoke. |
| [`artifactref`](artifactref) | `"artifact"` | An artifact handle (head, `#tag`, or revision) to its rendered bytes, scoped to the viewer's own session. |
| [`memoryref`](memoryref) | `"memory"` | A read-only memory query for the kind named in `Ref`, always scoped to the session from the URL path. |
| [`tool`](tool) | `"tool"` | A wire adapter turning the binding into an app-tool call answered over NATS by the runner. All authorization lives there, not here. |

Each subpackage is the same three lines: a `source` const, a `New()`, and
`func init() { registry.Register(New()) }`. Registration panics on an empty or
duplicate key.

**`internal/cmd/webd` is the only binary that uses these**, and it blank-imports
all four. Forgetting one is the silent-404 class of bug — the handler logs
"no resolver registered for source" rather than ever reaching a switch.

## Writes are never a data binding

This is the property the whole layer exists to hold, and it is enforced by
*shape*: an action is top-level in a declaration and can never appear in a
node's bindings. `actionstate` reinforces it at call time by refusing any
binding with non-empty `Args`.

## `SubstituteParams` — the only thing a browser may vary

`params.go` replaces every `{"$param":"<name>"}` object in a binding's args.
Source, ref, and template shape all come from the server-side declaration.

- A substituted value is **always re-encoded as a JSON string leaf**, never
  parsed or spliced — so a param can only ever fill a scalar slot the template
  itself marked substitutable.
- A referenced-but-absent param fails with `ErrUnknownParam`; it never
  substitutes an empty string.
- Depth is bounded (`maxSubstituteDepth = 64`).

`ErrRunnerUnreachable` is a typed sentinel **because the caller acts on it** —
an idle session's pods were reaped and the platform can wake it. Matching on
message text to decide whether to take a recovery path is how a copy edit
silently disables it.

## `Deps` has three different nil-check shapes, deliberately

| Accessor | Returns | Nil check |
| -------- | ------- | --------- |
| `Memory()` | an **interface** | A typed-nil `*memory.Local` assigned into it makes `!= nil` report **true**, the fail-closed check passes, and the resolver panics on first call. **Declare the variable as the interface and assign only once a real value exists.** |
| `Artifacts()` | a concrete pointer | Nil means what it says. |
| `NATSRequest()`, `ArtifactRenderBytes()` | func types | No interface wrapper for a typed-nil to hide behind. |

`ArtifactRenderBytesFunc` sits on `Deps` rather than being resolver-constructed
because the bytes live behind an operator route only `webd` holds a token for,
and `artifactref` must not learn that token's shape.

## Per-resolver constraints worth knowing

- **`actionstate`:** "no record yet" is **not** an error — it resolves to the
  empty-state copy. A record naming a different action or a different viewer
  hits the same fallback, so no case can leak another viewer's state. The newest
  record is chosen by explicit timestamp comparison, not by trusting list order.
- **`artifactref`:** a handle from another session is simply **not found**,
  rather than merely denied. Oversized results are **refused, not truncated**.
- **`memoryref`:** args decode into a **closed** struct with
  `DisallowUnknownFields` — there is deliberately no scope, namespace, or
  session field. **Append-only kinds are refused**, derived from
  `Kind.Retention()` rather than a transcribed list, and the refusal reuses the
  same copy as an unregistered kind so a browser cannot distinguish "doesn't
  exist" from "exists but is off-limits".
- **`tool`:** surfaces `resp.ViewerMessage`, **never** `resp.Message` — the
  latter is the operator/diagnostic channel carrying permissions, resource ids,
  and raw subjects, and is logged only. The request id is **server-minted**; a
  client-supplied one could collide with another viewer's in-flight request.
