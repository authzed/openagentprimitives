# pkg/tools

Tool **definition and execution**: what an agent's tools are, how they are
authored and validated, and the substrate they run in. Two spec formats live
here — `SpiceboxToolspec` for CLI invocations and `MCPServer` for JSON-RPC tool
calls — along with the CEL engine both constrain calls with, the sandbox
backends tools execute in, and the skills a session materializes.

| Package | Purpose |
| --- | --- |
| [`adoptkit`](adoptkit/) | Metadata-only server-side apply marking a CR-referenced Secret/ConfigMap as operator-adopted. Never reads or writes `.data`. |
| [`catalog`](catalog/) | Loads a directory of toolkit YAMLs (consumed by `oap tools toolspec describe`/`test`), plus a hardcoded MCP-server registry. |
| [`cel`](cel/) | `cel-go` wrapper: the two fixed environments (`call` for toolspec, `args` for MCP) plus the helper-function registry. |
| [`contract`](contract/) | The `Kind` plug-in surface the kind-agnostic `oap tools` CLI consumes. |
| [`exec`](exec/) | The `Executor` seam for running a command inside a sandbox, plus its kubelet and fake transports. |
| [`kinds`](kinds/) | The registered `contract.Kind` implementations — CLI presentation, one per tool CR. |
| [`mcp`](mcp/) | Everything MCP: OAuth, the session-aware client, the spec format, its validator and renderers. |
| [`redact`](redact/) | Replaces sensitive values with stable `<redacted id="N"/>` tokens plus an ID→descriptor map. |
| [`sandboxkinds`](sandboxkinds/) | The pluggable sandbox-backend seam (`pod`, `agent-sandbox`) and its conformance suite. |
| [`skillbundle`](skillbundle/) | Content-addressed store for skill-bundle tarballs; in-memory and postgres backends. |
| [`skills`](skills/) | Skill identity, fetch, parse, validation and materialization for the agentskills.io format. |
| [`toolchain`](toolchain/) | The seam for delivering a toolchain payload into a sandbox pod. |
| [`toolkitstream`](toolkitstream/) | Per-toolkit stream parsers turning an interactive tool's stdout into neutral events. |
| [`toolspec`](toolspec/) | The CLI-invocation validator: the toolkit and spec formats, the argv parser, CEL, the decision. |
| [`websearch`](websearch/) | Swappable web-search / web-fetch provider used by the identity-setup LLM agent (`pkg/platform/identity/setup/llmagent`). |

## Boundary

- Tool **definition and validation** (spec formats, parsers, CEL, validators)
  and the **substrate** they execute in (sandbox kinds, exec transports,
  toolchains) belong here.
- Tools **as the agent loop calls them** — the `tool.Tool` interface, dispatch,
  per-turn wiring, and synthesizers such as
  [`pkg/agent/tool/sidecartoolbox`](../agent/tool/sidecartoolbox/) — belong in
  [`pkg/agent`](../agent/).
- The shared `Decision` / `Trace` / redaction machinery both validators build on
  is [`pkg/authz/validator/core`](../authz/validator/core/); authorization
  *policy* is [`pkg/authz`](../authz/).
- CRD types are [`pkg/apis/v1alpha1`](../apis/v1alpha1/); the reconcilers that
  act on them are [`pkg/controllers`](../controllers/).

See the root [`README.md`](../../README.md) and [`AGENTS.md`](../../AGENTS.md).
