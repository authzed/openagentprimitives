# kinds

The registered [`contract.Kind`](../contract/) implementations — the **CLI
presentation** layer for each tool CR. `oap tools <verb>` (list / get / edit /
apply / validate / lint / probe) is kind-agnostic and dispatches through the
registry here, so adding a kind is a package plus one blank import in
`cmd/oap/internal/toolscmd/root.go`. No CLI verb is edited.

| Package                             | CR kind            | Note                                                                                                                                                     |
| ----------------------------------- | ------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`registry`](registry/)             | —                  | The process-wide registry, over `pkg/x/kindregistry`. `Register` panics on a duplicate name; `DecodeAny` walks every kind asking it to claim a YAML doc. |
| [`mcp`](mcp/)                       | `MCPServer`        | Also backs the live `probe` verb, via [`pkg/tools/mcp/probe`](../mcp/probe/).                                                                            |
| [`sandbox`](sandbox/)               | `SpiceboxToolspec` | **The directory is named `sandbox`; the CR it registers is `SpiceboxToolspec`.**                                                                         |
| [`sidecartoolbox`](sidecartoolbox/) | `SidecarToolbox`   | Presentation only — see below.                                                                                                                           |

Each kind's `init.go` is a one-liner:
`func init() { registry.Register(New()) }`. Importing the package is the whole
wiring.

## `sidecartoolbox` here is the presentation Kind, not the synthesizer

There are two packages with this name and they must not be conflated.

- **[`pkg/tools/kinds/sidecartoolbox`](sidecartoolbox/) — this one.** The CLI
  presentation Kind: `Row`, `Detail`, `ValidateFile`. Blank-imported by
  `cmd/oap`. Touches no credential.
- **[`pkg/agent/tool/sidecartoolbox`](../../agent/tool/sidecartoolbox/) — the
  other one.** The execution synthesizer (`Synthesize`), imported directly by
  `internal/cmd/runner` to build sidecar tools from AgentSession status.

A new CLI kind gets the blank import; the synthesizer does not.

Part of [`pkg/tools`](../README.md). See the root
[`README.md`](../../../README.md) and [`AGENTS.md`](../../../AGENTS.md).
