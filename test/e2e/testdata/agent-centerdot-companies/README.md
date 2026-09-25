# centerdot-companies — E2E test fixture agent

**This is not a deployable agent.** Its `model.provider` is `test`,
which the production operator refuses (`Valid=False
reason=TestProviderNotAllowed`). It exists solely as a fixture for
the `test/e2e` test framework.

## What it does

Mirrors the hubspot-companies workflow in miniature: list company
records, look up contacts for a specific company, gate the
contact-lookup behind owner approval. Self-contained — no real CRM
API, no real Slack, no OAuth.

## Layout

| File | Purpose |
|---|---|
| `00-secret.yaml` | Placeholder Anthropic Secret. Required because `AgentClass.spec.model.apiKey` is mandatory, but the `test` provider doesn't use it. |
| `01-identity.yaml` | `AgentIdentity` with a static credential + binding for `mcp:centerdot-companies`. |
| `02-mcpserver.yaml` | `MCPServer` with two tools (`list_companies`, `list_contacts_for_company`) and the SpiceDB schema fragment that mirrors hubspot's owner/grant/wildcard pattern. The MCP server URL is `{{MCP_URL}}` — the e2e harness substitutes the actual httptest endpoint before apply. |
| `03-agent.yaml` | `AgentClass centerdot-companies` (`provider: test`) + `Channel centerdot-fake` (`kind: fake`). |
| `04-spicedbbootstrap.yaml` | Seeds the owner identity relations the approval flow resolves approvers against. |

## Running

Used automatically by `test/e2e/scenarios/centerdot/*` tests. To
inspect manually against a kind cluster:

```bash
# Replace {{MCP_URL}} with your test MCP server URL, then:
kubectl apply -f test/e2e/testdata/agent-centerdot-companies/
# AgentClass will reach Valid=False with TestProviderNotAllowed —
# that's expected, the production operator refuses provider=test.
```
