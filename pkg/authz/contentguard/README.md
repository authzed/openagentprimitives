# `pkg/authz/contentguard`

The pluggable content-inspection seam over the tool-call hook pipeline. An
`Inspector` is a registry-keyed factory referenced from settings by ID; its
`Configure` parses and validates the per-instance config **once** and returns an
`Instance` that inspects tool arguments (input) and/or results (output) and
returns a `Finding` (`Pass` | `Block` | `Approve`).

Plugin authors implement only `Configure` + `Inspect`. The framework adapter in
[`hook.go`](hook.go) maps `Finding`s onto pipeline `Decision`s, so no inspector
deals with pipeline types.

Content guards run at order 18 in [`../hooks/order.go`](../hooks/order.go):
after the toolguard circuit breaker (content that would trip a breaker is
already denied) and before the SpiceDB check (inspection runs before an approval
ask is raised).

| Package                 | What it does                                  |
| ----------------------- | --------------------------------------------- |
| [`kinds`](kinds/)       | The built-in inspectors.                      |
| [`registry`](registry/) | The process-wide registry of inspector kinds. |

## The byte cap is a decorator, not a call-site check

`MaxInspectBytes` (32 KiB) is enforced by `Capped(Instance)`
([`cap.go`](cap.go)) wrapping the instance, **not** by the callers. Both
inspecting paths — the pipeline adapter for gated tools and the runner's
meta-tool inspection — therefore cap identically, and no third caller can opt
out by reaching for `Inspect` directly. Keep new call paths going through the
decorator.

## Adding an inspector

1. New package `kinds/<name>/` implementing `contentguard.Inspector`.
2. `func init() { registry.Register(New()) }`.
3. Blank-import it from the binaries that need it.

If the inspector needs a sidecar (as the prompt-injection classifier does),
implement `DetectorProvider` so the operator knows which image to inject.
Nothing outside the new package changes.
