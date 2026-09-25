# `pkg/agent/tool/meta`

The meta tools: `tool.KindMeta` implementations that run in-process in the
runner, with no sandbox and no upstream server. One file per tool (or per small
family), each exporting a `New…` constructor.

Which of them a given session actually gets is decided by
[`capability`](./capability/) — that is the only caller of most of these
constructors.

| Area                    | Tools                                                                                                                                                                                                                                          |
| ----------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Session control         | [`agent_complete.go`](./agent_complete.go), [`await.go`](./await.go), [`new_operation.go`](./new_operation.go), [`update_status.go`](./update_status.go)                                                                                       |
| Answering the trigger   | [`trigger_status.go`](./trigger_status.go) — `claim_trigger_status`, `conclude_trigger_status`, offered only for an input channel kind that owns its trigger's status surface                                                                  |
| Talking to people       | [`respond.go`](./respond.go), [`mention.go`](./mention.go), [`set_thread_title.go`](./set_thread_title.go)                                                                                                                                     |
| Conversation history    | [`readhistory.go`](./readhistory.go), [`readchannelhistory.go`](./readchannelhistory.go)                                                                                                                                                       |
| Memory & knowledge      | [`query_memory.go`](./query_memory.go), [`search_memory.go`](./search_memory.go), [`query_knowledge.go`](./query_knowledge.go), [`recall_errors.go`](./recall_errors.go)                                                                       |
| Artifacts & attachments | [`artifact_prepare.go`](./artifact_prepare.go), [`artifact_await.go`](./artifact_await.go), [`artifact_history.go`](./artifact_history.go), [`artifact_offer_view.go`](./artifact_offer_view.go), [`show_attachment.go`](./show_attachment.go) |
| Agent-defined UI        | [`show_agent_ui.go`](./show_agent_ui.go), [`update_view.go`](./update_view.go), [`read_view.go`](./read_view.go), [`set_view_params.go`](./set_view_params.go)                                                                                 |
| Planning                | [`update_plan.go`](./update_plan.go)                                                                                                                                                                                                           |
| Skills & workspace      | [`load_skill.go`](./load_skill.go), [`workspace_tools.go`](./workspace_tools.go)                                                                                                                                                               |
| Credentials             | [`credential_update.go`](./credential_update.go)                                                                                                                                                                                               |
| Introspection           | [`introspect.go`](./introspect.go)                                                                                                                                                                                                             |

## Subpackages

- [`capability`](./capability/) — the AgentClass-grantable capabilities that
  decide which meta tools a session is offered, and construct them.

## The registry, and its actual scope

[`registry.go`](./registry.go) holds `Register` / `Load`. Registration panics on
a duplicate name, and `Load` returns tools sorted by name.

**Only two tools use it today** — `agent_work_complete` and `new_operation`, the
always-on pair, pulled in by the `core` capability's `Offer`. Every other tool
in this package is constructed explicitly by its capability through an exported
`New…` function. The package doc comment describes the `init()` registration
path as the general one; that is accurate for the terminal tools and not for the
rest.

## Constraints

- **Meta tools bypass the containment pipeline** for trivial permissions, so
  their guarding is special-cased in
  [`../../runner/toolguard_meta.go`](../../runner/toolguard_meta.go) and their
  content inspection in
  [`../../runner/loop_metainspect.go`](../../runner/loop_metainspect.go).
- **`recall_errors.go` decides `Result.Trusted` from error provenance.** A
  repo-authored memory sentinel keeps `Trusted` (the model must be able to read
  the corrective hint); a provider/transport error, which can interpolate an
  upstream response body, drops it. A new sentinel belongs in that file's
  allowlist.

## See also

- [`pkg/agent/tool`](../) — the `Tool` interface and the three kinds.
