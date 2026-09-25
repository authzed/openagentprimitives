# toolkitstream

Per-toolkit **stream parsers**. A `Parser` consumes an interactive tool's stdout
chunks and emits a small neutral `Event` vocabulary — `text_delta`,
`tool_use_start`, `tool_use_stop`, `result` — that the channel kinds render. The
package has zero dependencies on Kubernetes APIs or transport machinery, so a
parser plug-in is unit-testable in isolation.

| Package | Purpose |
| --- | --- |
| [`registry`](registry/) | The process-wide factory registry, keyed by `Factory.Kind()`. Panics on an empty or duplicate kind. |
| [`claude`](claude/) | The parser for `claude-stream-json`, the format `Toolkit.StreamFormat` names. |

[`contract.go`](contract.go) holds `EventType`, `Event`, `Parser`, `Factory` and
`Outcome`. A `Factory` builds a **fresh parser per session**, because a parser
carries a partial-line buffer.

## Constraints

- A blank import of a parser package in `internal/cmd/runner` is the whole
  wiring; nothing else is edited.
- The `claude` parser bounds its partial-line buffer at 1 MiB. A malformed
  multi-MB line would otherwise grow the heap until OOM, so on overflow it resets
  the buffer and emits a visible `[stream truncated]` `text_delta` rather than
  failing silently.

Part of [`pkg/tools`](../README.md). See the root
[`README.md`](../../../README.md) and [`AGENTS.md`](../../../AGENTS.md).
