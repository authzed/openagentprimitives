# ledger-records — E2E test fixture agent

**This is not a deployable agent.** Its `model.provider` is `test`,
which the production operator refuses (`Valid=False
reason=TestProviderNotAllowed`). It exists solely as a fixture for the
`test/e2e` framework.

## What it does

One thing: reads a ledger entry, so that the read mints a **pt-tag**
(per-datum provenance). Everything else is stripped out — a single
tool, no approval flow, no slots, `stateImpact: stateless` so the
per-tool SpiceDB Check does not run.

That narrowness is the design. The claim the fixture supports is that a
tag's audience is **derived** from the resource the call touched rather
than named by the session, and a fixture with a second gate in the path
makes a failure ambiguous between "the check denied" and "the mint never
happened".

## The two entries

`04-spicedbbootstrap.yaml` seeds two entries with **disjoint** viewers:

| Entry | Viewer |
|---|---|
| `entry-100` | `auditor-1@example.com` — the session user |
| `entry-900` | `auditor-9@example.com` — nobody the session can speak for |

Both are needed. A tag minted from `entry-100` must resolve to
`auditor-1` and must *not* resolve to `auditor-9`; seed one audience and
every subject becomes a reader, at which point the scenario passes with
the lattice switched off. `entry-900` is never read — it exists so
`auditor-9` is a subject with real standing somewhere, because an
exclusion assertion against a subject that resolves to nothing passes
trivially.

## Layout

| File | Purpose |
|---|---|
| `00-secret.yaml` | Placeholder Anthropic Secret (mandatory field, unused by the `test` provider) + fake-channel credentials. |
| `01-identity.yaml` | `AgentIdentity` with one static MCP bearer credential. |
| `02-mcpserver.yaml` | `MCPServer` with one tool (`read_entry`), the `ledger_entry` schema fragment, and the `toolResourceMap` read declaration the mint hangs off. URL is `{{MCP_URL}}`; the harness substitutes the httptest endpoint before apply. |
| `03-agent.yaml` | `AgentClass ledger-records` + `Channel ledger-fake` (`kind: fake`). Info-leakage is `enforcing` — that gate is what carries the mint. |
| `04-spicedbbootstrap.yaml` | Seeds the two viewer sets above. |

## Used by

`test/e2e/bronzethread/testdata/pttag-audience-derived-from-source/`.
