# `pkg/authz/pinning/kinds`

The per-kind implementations of [`pinning.Kind`](../). Each adapts one
dependency type's existing ref format to the common `ParseRef` / `Resolve` /
`Verify` contract and self-registers into [`../registry`](../registry/) from its
`init()`.

| Kind | Adapts |
| ---- | ------ |
| [`skill`](skill/) | Agent Skill refs (`pkg/tools/skills/canonical`). |
| [`image`](image/) | OCI image refs — tag vs. digest is the strength signal. |
| [`mcp`](mcp/) | MCP server manifest assertions. |
| [`cli`](cli/) | CLI toolkit binary assertions. |
| [`oap`](oap/) | `.oap` agent-container OCI registry refs. |

Adding one is a new package plus a blank import in the binaries that need it —
no consumer changes.
