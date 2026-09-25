# render

Plain-language description of a ToolSpec — a pure function of
`(spec, toolkit, format)`. No network, no LLM.

Two stages. [`report.go`](report.go)'s `buildReport` projects the pair into a
`*report`, the structured intermediate every output format shares;
[`format_text.go`](format_text.go) and
[`format_markdown.go`](format_markdown.go) (or `json.MarshalIndent` for
`FormatJSON`) turn that into bytes. [`templates.go`](templates.go) holds the
per-rule fallback sentences used when `Generation.Descriptions` has no entry for
a rule path.

## Constraints

- **Raw CEL is never emitted.** A constraint is described by its authored
  `Message`; the expression itself is not shown to a reader who neither wrote it
  nor can act on it.
- Adding an output format means adding a renderer over `*report`, not a second
  traversal of the spec.
- [`pkg/tools/mcp/render`](../../mcp/render/) is the same two-stage shape for
  MCP specs. Keep the two parallel.

Part of [`pkg/tools/toolspec`](../README.md). See the root
[`README.md`](../../../../README.md) and [`AGENTS.md`](../../../../AGENTS.md).
