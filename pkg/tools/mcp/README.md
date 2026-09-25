# mcp

Everything on the **MCP (Model Context Protocol)** side of tools: authenticating
to a server, speaking its session protocol, the spec format that constrains what
an agent may call, and the runtime validator that enforces it. The directory
root holds no code of its own — only a cross-package integration test.

| Package | Purpose |
| --- | --- |
| [`oauth`](oauth/) | OAuth 2.0 + PKCE against an MCP server: RFC 8414 discovery, RFC 7591 dynamic client registration, the localhost-callback code flow, token exchange and refresh. Standard library only. |
| [`probe`](probe/) | The Streamable-HTTP MCP client, backed by the official MCP Go SDK. Reachability probes, tool discovery, tool-call dispatch, and the per-AgentSession `SessionCache`. |
| [`spec`](spec/) | The flat MCP authoring document: server, auth, the closed tool allowlist, per-tool arg constraints. Load plus CEL type-check. |
| [`validator`](validator/) | Per-tool-call validation of an invocation against a spec, producing a `Decision`. |
| [`render`](render/) | Plain-language description of one tool's effective contract (text / markdown / JSON). Never emits raw CEL. |
| [`trust`](trust/) | Renders SEP-1913 trust annotations, from either a probe result or a persisted spec. |
| [`testing`](testing/) | httptest-backed fake MCP servers for probe, dispatch, controller and CLI tests. |

## Constraints

- **`probe` speaks the full session protocol** (initialize → `Mcp-Session-Id` →
  `notifications/initialized`). A bare stateless JSON-RPC POST is rejected by
  spec-compliant servers, so nothing here hand-rolls one.
- **A server URL is agent-influenceable** — it comes from
  `MCPServer.spec.server.url`. `probe.Client` defaults to the SSRF-guarded
  [`pkg/x/safehttp`](../../x/safehttp/) client; only tests and
  operator-controlled targets (a sidecar pod IP, loopback) set `HTTP` explicitly.
- **A tool's `Description` reaches the model verbatim** unless a spec overrides
  it. Treat it as untrusted prompt text.
- `pkg/tools/mcp/testing` shadows the stdlib `testing` package name; import it
  aliased as `mcptest`.
- The MCP and [`toolspec`](../toolspec/) validators are deliberately parallel and
  share their `Decision` / `Trace` / redaction machinery through
  [`pkg/authz/validator/core`](../../authz/validator/core/). Keep them so.

Part of [`pkg/tools`](../README.md). See the root
[`README.md`](../../../README.md) and [`AGENTS.md`](../../../AGENTS.md).
