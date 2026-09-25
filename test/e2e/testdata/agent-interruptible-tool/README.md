# agent-interruptible-tool — E2E test fixture agent

**This is not a deployable agent.** Its `model.provider` is `test`, which the
production operator refuses (`Valid=False reason=TestProviderNotAllowed`). It
exists solely as a fixture for the `test/e2e` test framework.

## Why this fixture exists

The mid-turn interrupt round-trip tests need exactly one thing from a fixture:
**a Cancellable MCP tool whose declared `permission.stateImpact` is
`readonly`**, so `pkg/agent/runner/interrupt.go`'s `interruptibleReason`
classifies an in-flight call as truthfully cancellable and
`KindInterruptApplied.Outcome` can be `"interrupted"` rather than
`"rejected"`.

`stateImpact` does double duty: it drives authorization *and* runtime
interruptibility. Those tests previously borrowed
`agent-centerdot-companies`, whose `stateImpact` values are chosen by its
**approval** model. When `list_companies` moved `readonly` → `passthrough`
(as part of closing the `routeViaSessionGrant` ⇄ wildcard-leaf invariant),
two interrupt tests that care nothing about approval started failing —
`passthrough` is not interruptible.

This fixture severs that coupling. It carries no approval flow, no
`routeViaSessionGrant`, and no wildcard (`user:*`) leaf, so no change to any
approval shape anywhere can reach the interrupt tests.

## Consumer contract

- Start with `DefaultUser: "user@example.com"` (or send with
  `e2e.AsUser("user@example.com")`). `04-spicedbbootstrap.yaml` seeds
  `feed:activity-stream#reader` for exactly that subject; any other requester
  is denied and `watch_feed` never reaches the MCP stub.
- Register an MCP handler for `watch_feed`
  (`h.MCP.OnTool("watch_feed", ...)`). The stub synthesizes its `tools/list`
  response from registered handlers only, and the MCPServer reconcile's
  `allowlistDriftCheck` fails a declared tool the server does not advertise.
- Wait on both `WaitForAgentClassValid("interruptible-tool", …)` and
  `WaitForSpiceDBBootstrap(…)` before driving traffic — `watch_feed`'s
  `readonly` check needs the seeded tuple live.
- The LLM-facing tool name is `slowfeed_watch_feed` (`mcpServers[0].name` is
  `slowfeed`).

## Layout

| File | Purpose |
|---|---|
| `00-secret.yaml` | Placeholder model + fake-channel Secrets. Required by the CRD schemas; unread at runtime. |
| `01-identity.yaml` | `AgentIdentity` with one static MCP bearer credential. |
| `02-mcpserver.yaml` | `MCPServer slowfeed-tools`: one `readonly` tool (`watch_feed`) plus the minimal `feed`/`read` SpiceDB fragment its check resolves against. |
| `03-agent.yaml` | `AgentClass interruptible-tool` (`provider: test`) + `Channel interruptible-fake` (`kind: fake`). |
| `04-spicedbbootstrap.yaml` | Seeds `feed:activity-stream#reader` for the single requester. |

## Consumers

- `test/e2e/interrupt_test.go`
- `test/e2e/scenarios/queued_messages_interrupt/`
