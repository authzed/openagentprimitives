# Product Manager Agent

A read-only agent that reviews GitHub pull requests and Linear issues, then
reports how the work in flight aligns with stated product goals.

## What it does

- Reads the GitHub repos you grant it (via the `repos` install question).
- Reads Linear issues in the linked workspace.
- Posts alignment summaries — it never writes to either system.

## What it needs

- **GitHub PAT** (`gh-pat` secret, `token` key): a read-only personal access
  token. Answered by the `githubToken` install question.

## Configuration

| Question | Purpose |
| --- | --- |
| `repos` | Which GitHub repositories the agent may read. |
| `githubToken` | The read-only GitHub PAT the agent authenticates with. |

This agent applies no privileged resources; it is safe to install into a shared
namespace.
