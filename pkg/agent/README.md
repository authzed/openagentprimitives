# `pkg/agent` — the runner

Everything that happens _inside_ an agent session: the turn loop, the harness
that hosts it, the LLM providers it calls, the session state machine, and tools
as the agent calls them.

The entry point is [`runner`](./runner/) — `Loop.Run` is the turn loop the
`runner` binary drives. Everything else in this group is either something the
loop consumes (providers, tools, session state) or something a _different_
component reads about a session (`agentcaps`, `restartmarker`).

## Subpackages

| Package                             | Purpose                                                                                                                                                           |
| ----------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`agentcaps`](./agentcaps/)         | Resolves AgentClass capability grants. Dependency-free (API types only) so any component can ask "is this capability granted?" without pulling in the tool graph. |
| [`harness`](./harness/)             | The swappable component that runs a session's outer loop. Registry with one registered backend, `ap-native`.                                                      |
| [`llm`](./llm/)                     | Provider-neutral request/response/streaming types, plus one subpackage per provider and the name→provider registry.                                               |
| [`modality`](./modality/)           | Capability-aware surfaces (tools + prompt text) that plug into a turn when the resolved model supports them — today, native file in/out.                          |
| [`postsession`](./postsession/)     | End-of-session reporters, registered as `SessionEnd` pipeline hooks.                                                                                              |
| [`preview`](./preview/)             | Isolated secondary-LLM helpers for artifact previews.                                                                                                             |
| [`restartmarker`](./restartmarker/) | Signs and verifies `AgentSession.status.pendingRestart`, the fork/takeover marker channelsd writes and the operator acts on.                                      |
| [`runner`](./runner/)               | The turn loop itself: replay, dispatch, approvals, status, lifecycle sequencing, pipeline wiring.                                                                 |
| [`secretout`](./secretout/)         | Per-session out-of-band store for tool-produced secret values. The model only ever sees opaque handles.                                                           |
| [`session`](./session/)             | The pure session state machine and the per-session in-memory state framework.                                                                                     |
| [`tool`](./tool/)                   | The `Tool` interface the loop sees, and the three runtime kinds behind it: meta, sandbox, MCP.                                                                    |
| [`userprofile`](./userprofile/)     | Kind-neutral user-profile vocabulary and the single field filter that decides what an agent may see.                                                              |

## Boundaries

- **vs. [`pkg/tools`](../tools/)** — `pkg/tools` is tool _definition_: toolspec
  authoring and parsing, toolkits, MCP spec/probe/render, sandbox backend kinds,
  skills, the CLI-facing tool CRD kinds. `pkg/agent/tool` is tool _invocation_:
  it consumes those specs and synthesizes the `tool.Tool` values the loop
  dispatches. Dependencies point one way — `pkg/agent/tool/mcp` imports
  `pkg/tools/mcp/{probe,render,spec}`, never the reverse.
- **vs. [`pkg/channels`](../channels/)** — channels own transport and the
  conversation surface (Slack, browser, local; listeners, senders,
  interactions). The runner publishes envelopes and consumes turns; it does not
  know how a message reaches a person. Channel-shaped formatting belongs in the
  channel kind, not in the runner's prompt.
- **vs. [`pkg/controllers`](../controllers/)** — the AgentSession reconciler
  builds the runner Pod, resolves credentials, and owns `status`. The runner
  patches a narrow slice of status through `runner.StatusPatcher`.

## See also

- Repo [`README.md`](../../README.md)
- [`AGENTS.md` § Where code lives](../../AGENTS.md#where-code-lives)
