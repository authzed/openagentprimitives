# agent-slack-dm — E2E test fixture agent

**This is not a deployable agent.** Its `model.provider` is `test`,
which the production operator refuses (`Valid=False
reason=TestProviderNotAllowed`). It exists solely as a fixture for
the `test/e2e/scenarios/slack_dm_threading` tests (Plan-2 Task 6).

## What it does

A trivial echo-style agent with no tools — it just replies to whatever
the user says via `respond_to_user`. Its purpose is to exercise the
**real** `kind: slack` channel implementation end to end (real listener
→ pipeline → runner → relay → real slack sender), backed by the e2e
harness's shared `fakeslack.Client` transport
(`slack.InstallTestTransport`), rather than the in-process `kind: fake`
transport the other e2e fixtures use.

## Layout

| File | Purpose |
|---|---|
| `00-secret.yaml` | Placeholder Anthropic Secret (required by `AgentClass.spec.model.apiKey`, unused by the `test` provider) + the Slack credentials Secret (`bot-token`/`app-token` — values are placeholders; the fake transport ignores them, but `resolve.ForChannel` requires the keys to be present). |
| `01-agent.yaml` | `AgentClass slack-dm-agent` (`provider: test`, no `mcpServers`/`agentIdentity`) + `Channel slack-dm-channel` (`kind: slack`, `role: both`). |

No `SpiceDBBootstrap` is needed: the DM/mention sender becomes the
session's `agentsession#owner` automatically (channel-provided starting
user), and `interact = owner + participant - denied` already admits
that same user's follow-up turns.

## Running

Used automatically by `test/e2e/scenarios/slack_dm_threading/*` tests.
