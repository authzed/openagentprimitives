# `pkg/web/uidemo`

Demo material for the agent-defined-UI stack. **Not platform code** — nothing in
any cluster service depends on it. It lives under the obviously-demo `uidemo/`
prefix, and outside `examples/`, so tests may import it (tests must never depend
on `examples/`).

## [`leadflow`](leadflow)

A fabricated CRM MCP server used to demonstrate the leads-console agent UI. It
is an in-process MCP server exposed over Streamable HTTP.

| File          | What it holds                                                                                     |
| ------------- | ------------------------------------------------------------------------------------------------- |
| `leadflow.go` | `Lead`, the stage list, the `Book` store, `Options`, and the `Server`                             |
| `seed.go`     | Twelve fixed leads with fixed timestamps and three owners — **deterministic, never `time.Now()`** |
| `tools.go`    | The four tools, their handlers, and the registration loop                                         |

Four tools: `list_leads`, `stage_breakdown`, `advance_lead_stage`, and
`search_accounts`. `stage_breakdown` always returns one entry per stage, in
stage order; `search_accounts` deliberately answers with a nested envelope
(`results[].properties`) so the demo exercises [`uiselect`](../uiselect)'s
selector language.

## Where it runs

- **`internal/cmd/leadflowfake`** — a `go run` dev binary, the only binary that
  imports it. It is deliberately **not a container image**: it is demo upstream
  for an example `MCPServer`, not a platform service, so it carries none of the
  shared-builder or install-surface obligations a real image target would add.
- **Tests** — a wiring test drives the real probe/synthesize/grant path against
  an `httptest` server, and several e2e scenarios import it for its constants.

It carries **no build tag**, deliberately, because both the e2e-tagged scenarios
and the plain unit suite import it.

## Constraints that look arbitrary but are not

- **Run it somewhere the cluster can reach by a public address.** The SSRF guard
  in [`pkg/x/safehttp`](../../x/safehttp) refuses loopback, link-local, and
  RFC1918 destinations, so an `MCPServer`'s URL cannot point at a
  cluster-internal Service. The package's own tests reach it over loopback
  through the plain-client seam, never the guarded client.
- **Every tool argument is string-typed, and the row cap is a server-side
  constant rather than a tool argument.** A binding parameter can only produce a
  JSON string, and the app-tool path does no input-schema validation, so a
  numeric argument would be a trap.
- **Every tool is app-only** (`visibility: ["app"]`) — none is ever offered to
  the model.
- **Results are a single text block.** The tool binding unwraps a response into
  a Go string and hands the raw JSON to the bound prop; a structured result with
  no text content would resolve to a prop value the renderer cannot use.
- **Errors use the `IsError` result shape**, never a protocol-level error.
- `Server.Calls()` exists so a test can prove the CRM observed specific
  arguments rather than inferring it from the response shape.
- Range filtering compares RFC3339 bounds **lexicographically**, which is valid
  only because the seed timestamps share one timezone and precision.
