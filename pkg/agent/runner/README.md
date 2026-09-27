# `pkg/agent/runner` — the turn loop

One package, ~66 non-test files, all `package runner`. It is the body of an
agent session: replay prior turns, call the model, dispatch tool calls through
the authorization pipeline, publish progress and approvals, fold the lifecycle
log, and write status.

## Start here

| File                                                              | What it holds                                                                                                                                                       |
| ----------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`loop.go`](./loop.go)                                            | **The spine.** `Loop.Run` — the whole turn loop, top to bottom. Every other `loop_*.go` file holds a step it calls.                                                 |
| [`loop_deps.go`](./loop_deps.go)                                  | **The `Loop` struct** and its collaborator interfaces (`MemoryAppender`, …). This is the field list `Run` and every method operate on; read it alongside `loop.go`. |
| [`runner.go`](./runner.go)                                        | Entry helpers outside a `Loop` — e.g. `HandleStaleSession`.                                                                                                         |
| [`host.go`](./host.go) / [`host_approval.go`](./host_approval.go) | `runnerHost`, the runner's implementation of the pipeline `Host` interface: notices, status, halt, audit, and the approval publish/await round trip.                |
| [`pipeline_wiring.go`](./pipeline_wiring.go)                      | Builds the `pipeline.Registry` and `Executor` and every hook's dependency struct. The single place the loop is bound to the authorization pipeline.                 |

## The `loop_*.go` files

Each is one concern lifted out of `Run`. Names are the concern, not a layer.

| File                                           | Concern                                                                                                   |
| ---------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| [`loop_replay.go`](./loop_replay.go)           | Rebuild `[]llm.Message` from durable memory turns; repair orphaned tool_uses; replay session-state notes. |
| [`loop_coldstart.go`](./loop_coldstart.go)     | Whether this session is a cold start (no turn 0 yet) and may place the initial prompt.                    |
| [`loop_inbox.go`](./loop_inbox.go)             | Drain `inbox`-role turns channelsd wrote while the runner was away; strip untrusted blocks.               |
| [`loop_request.go`](./loop_request.go)         | Build the tool definitions for the request and mark prompt-cache breakpoints.                             |
| [`loop_dispatch.go`](./loop_dispatch.go)       | Execute tool uses — raw, or contained through the pipeline; auth-failure observation.                     |
| [`loop_metainspect.go`](./loop_metainspect.go) | Content-guard inspection of meta-tool input/output.                                                       |
| [`loop_untrusted.go`](./loop_untrusted.go)     | Nonce-wrapping of untrusted tool output.                                                                  |
| [`loop_subjects.go`](./loop_subjects.go)       | Resolve and advance the auth subject and per-turn requester.                                              |
| [`loop_activity.go`](./loop_activity.go)       | Activity/lifecycle signal emission, approval pauses, run-duration flush.                                  |
| [`loop_usage.go`](./loop_usage.go)             | Token accounting per session and per model; estimated-cost stamping.                                      |
| [`loop_failure.go`](./loop_failure.go)         | Terminal failure paths, refusal handling, stop-reason advisories.                                         |
| [`loop_uiresource.go`](./loop_uiresource.go)   | Persist an agent-declared UI resource as a Widget CR.                                                     |
| [`loop_secretout.go`](./loop_secretout.go)     | Apply a tool's declared secret output to the session's secret store.                                      |
| [`loop_autofill.go`](./loop_autofill.go)       | Bound-entity autofill types for a tool call.                                                              |
| [`loop_display.go`](./loop_display.go)         | Display names for the agent, class, and served model.                                                     |

## The rest, by concern

- **Tool call plumbing** — [`apptoolcall.go`](./apptoolcall.go)
  (browser/app-tool callbacks), [`apptools_grant.go`](./apptools_grant.go),
  [`apptool_ratelimit.go`](./apptool_ratelimit.go),
  [`toolguard_meta.go`](./toolguard_meta.go),
  [`permission_resolve.go`](./permission_resolve.go),
  [`argsenvelope.go`](./argsenvelope.go), [`interrupt.go`](./interrupt.go),
  [`operationactivity.go`](./operationactivity.go).
- **Approvals** — [`approval_outcome.go`](./approval_outcome.go),
  [`justification.go`](./justification.go), [`labelstore.go`](./labelstore.go),
  [`identitygate.go`](./identitygate.go),
  [`definitionreport.go`](./definitionreport.go).
- **Info-leakage gate** — [`leakage.go`](./leakage.go),
  [`leakage_notice.go`](./leakage_notice.go),
  [`respond_gate.go`](./respond_gate.go).
- **Status & progress** — [`status.go`](./status.go) (`StatusPatcher`, the only
  writer of the runner's slice of `AgentSession.status`),
  [`progress.go`](./progress.go), [`sequencer.go`](./sequencer.go) (lifecycle
  fold + event emission), [`runclock.go`](./runclock.go),
  [`budget.go`](./budget.go).
- **Inbound content** — [`attachments.go`](./attachments.go),
  [`inboundattachments.go`](./inboundattachments.go),
  [`speakerprofile.go`](./speakerprofile.go),
  [`queuedmessages.go`](./queuedmessages.go),
  [`resume_park.go`](./resume_park.go),
  [`delivery_guard.go`](./delivery_guard.go).
- **Prompt & tool assembly** — [`prompt.go`](./prompt.go) (`ComposeSystem`),
  [`modality.go`](./modality.go), [`toolkitresolve.go`](./toolkitresolve.go),
  [`sidecarsynth.go`](./sidecarsynth.go),
  [`bundleidentity.go`](./bundleidentity.go),
  [`descriptor.go`](./descriptor.go), [`uitooloptions.go`](./uitooloptions.go).
- **Provenance & pinning** — [`provenance.go`](./provenance.go),
  [`pindrift.go`](./pindrift.go),
  [`snapshot_request.go`](./snapshot_request.go),
  [`authfailorigins.go`](./authfailorigins.go).
- **Misc** — [`hookregistry.go`](./hookregistry.go),
  [`hooks_dataplane.go`](./hooks_dataplane.go), [`local.go`](./local.go)
  (in-process mode with no CR and no cluster),
  [`systemnote.go`](./systemnote.go), [`requestid.go`](./requestid.go),
  [`uiactionrecord.go`](./uiactionrecord.go),
  [`viewer_interact.go`](./viewer_interact.go).

## Subpackages

| Package                                         | Purpose                                                                         |
| ----------------------------------------------- | ------------------------------------------------------------------------------- |
| [`approval/summarizer`](./approval/summarizer/) | The isolated "what this tool call will do" LLM behind an approval prompt.       |
| [`channelhistorygate`](./channelhistorygate/)   | Shared decision: may this session get `read_channel_history`?                   |
| [`identityadvisor`](./identityadvisor/)         | Isolated advisory LLM for `identityMode: dynamic`. Recommends; never binds.     |
| [`leakagewiring`](./leakagewiring/)             | Shared wiring for the info-leakage gate's lookups.                              |
| [`userprofilegate`](./userprofilegate/)         | Shared decision: may this session's agent see profile detail about the speaker? |

The four gate/wiring packages exist so `internal/cmd/runner` and the e2e
in-process runner factory build the same `Loop` from the same code. If the
decision diverges between those two callers, the e2e suite is what notices — so
a new offer decision belongs here, not inlined in either binary.

## Registries

- **Hook factories** — [`hookregistry.go`](./hookregistry.go). A post-session
  reporter registers a `HookFactory` (see
  [`../postsession/cost`](../postsession/cost/)); the loop enumerates them when
  building the pipeline.

## See also

- [`pkg/agent`](../) — group overview and boundaries.
