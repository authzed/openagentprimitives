# `pipeline` — the shared inbound flow

`Pipeline` implements `channelkinds.InboundPipeline`. Every relayed channel
kind's listener hands it an `InboundEvent`; it correlates the message to a
session (creating one if needed), authorizes the sender, and returns an
`InboundDecision`. It also handles the return legs — interaction requests,
decisions and applied events coming back off the bus.

**This is where channel-kind-neutral policy lives.** A kind's listener supplies
raw transport facts and decides nothing.

| Concern | Files |
| ------- | ----- |
| **Core** | `pipeline.go` (`NewPipeline`, `Deliver`, `ResubmitAuthorized`) |
| **Session correlation** | `backfill.go`, `thread_owner.go`, `resolved_cache.go`, `statusapply.go`, `startup_status.go` |
| **Interaction legs** | `interaction_request.go` (`HandleInteractionRequest` / `HandleInteractionApplied`), `interaction_decision.go` (`HandleInteractionDecision`) |
| **Bound decision handlers** | `permission_interaction.go`, `tool_approval_interaction.go`, `provider_retry_interaction.go`, `queued_interrupt.go`, `approval_decision.go`, `identity_choice_decision.go` |
| **Credentials** | `credential_request.go`, `credential_linked.go`, `credential_update.go`, `identitylink.go`, `portal_access.go` |
| **Resurface** | `resurface.go`, `resurface_request.go` |
| **Other legs** | `decision.go` (permission-request TTL expiry), `restart_publish.go`, `view_message.go`, `viewdedup.go`, `attachments.go`, `agent_unavailable.go` |

## Non-obvious constraints

- **Decision handlers are `Bind`ed, not registered here.** Each
  `Bind*Handler(p *Pipeline)` attaches a handler to its
  [`channelinteractions`](../../channelinteractions/) category. The binary
  (`internal/cmd/channelsd`) calls them; the categories themselves are
  declarative rows with no behavior.
- **The Approve/Deny click and the TTL expiry are different legs.** A click
  arrives as a category-generic `KindInteractionDecision` and routes through
  `HandleInteractionDecision` to the bound handler. `decision.go` holds only the
  expiry leg — plus `grantInteract` / `denyInteract`, the SpiceDB-write helpers
  both legs share, so the grant is written in exactly one place.
- **Deny side effects live here, not in the host.** The blocklist, the dedup
  marker and the `permission_request` publish are in the pipeline's
  `handlePermissionDeny` branch; [`../pipelinehost`](../pipelinehost/) is a thin
  log adapter.
- **The pipeline is reachable from browser-facing code** via
  `pipelinehost` ← `pkg/web/webui/chat`. That constrains its import graph:
  a guard test fails the unit suite if `pkg/web/webui/chat` transitively
  imports `pkg/memory/provenance`, which is why the shared attachment size
  ceiling lives in the dependency-free `pkg/memory/assetlimits` rather than in
  `pkg/memory/httpsrv`.
