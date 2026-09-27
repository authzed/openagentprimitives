# Authz Messaging — Slack channel

How the Slack channel surfaces authz messaging. This is the Slack-specific
companion to the cross-channel contract in
[`../AUTHZMESSAGING.md`](../AUTHZMESSAGING.md) (read that first for the
vocabulary + invariants). Slack must uphold every invariant there; this doc
covers the _how_.

## Architecture: descriptor + driver

Every Slack authz flow is an `authzFlow` descriptor (a composition of small
strategies) run by the shared `authzDriver`. The driver owns the
lifecycle/plumbing; each flow plugs in the parts that legitimately differ. A
`nil` strategy means "skip that step".

```
SubChannelSender(name) ── newAuthzShim(deps, flowX()) ── authzDriver.dispatch
                                                              ├─ runRequest  (Kind == requestKind)
                                                              ├─ runApplied  (Kind == appliedKind)
                                                              └─ Extra[Kind]  (spectator, render-error, …)
```

`runRequest` does: resolve audience → (optional) public post → render prompt →
deliver prompt per recipient → (optional) require-delivery check → (optional)
pending ticker → cache ref → return the request ref. `runApplied` cancels any
ticker, then calls the flow's `Outcome`.

### The descriptor (`authzmsg.go`)

| Field                         | Type                         | Purpose                                                                                                                                                            |
| ----------------------------- | ---------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `Name`                        | string                       | Log/error label.                                                                                                                                                   |
| `Kinds`                       | `[]channelevents.Kind`       | All kinds this flow handles (request, applied, extras).                                                                                                            |
| `requestKind` / `appliedKind` | `Kind`                       | Drive the request / applied dispatch.                                                                                                                              |
| `ResolveAudience`             | `AudienceStrategy`           | Who receives the prompt (subject fan-out). May also implement `DirectAudienceStrategy`.                                                                            |
| `PublicPost`                  | `PublicPostStrategy`         | Participants-visible message. **`nil` ⇒ no public post** (sensitive flows).                                                                                        |
| `Prompt`                      | `PromptStrategy`             | The per-recipient prompt: `Render(rc) ([]Block, error)`, `Surface()`, `NotifText(rc)`.                                                                             |
| `Pending`                     | `PendingStrategy`            | Live "awaiting" ticker: `Interval()`, `MaxDuration()`, `RenderTick(rc, elapsed)`. `nil` ⇒ no ticker.                                                               |
| `Outcome`                     | `OutcomeStrategy`            | `Apply(ac) error` — render + deliver the resolved view on the applied envelope.                                                                                    |
| `Extra`                       | `map[Kind]Handler`           | Flow-specific kinds (e.g. tool_approval's spectator / render-error).                                                                                               |
| `store`                       | `*refStore[authzRef]`        | In-process request cache. `nil` ⇒ no caching (flow re-renders from the payload / button refs).                                                                     |
| `RequireDelivery`             | bool                         | When true, `runRequest` errors if there were recipients/candidates but none received the prompt (single-surface flows where a failed prompt means a dead request). |
| `BuildRequestRef`             | `func(rc, perUserTS) string` | Flow-controlled `SubChannelSendResult.RequestRef` (button-packed/pipeline-echoed flows). `nil` ⇒ generic ref blob.                                                 |

`requestCtx` (request-phase, handed to render strategies) carries `ctx`, `deps`,
`cli`, `sess`, `env`, `sessRef`, `channelID`, `threadTS`, `publicMsgTS` (set
after the public post, before the prompt renders), `approvers`, `resolveErrs`.
`appliedCtx` (applied-phase) carries `ctx`, `deps`, `cli`, `httpClient`, `sess`,
`env`, `store`.

### Shared primitives

- `refStore[T]` — generic concurrent `requestID→ref` cache (replaced the bespoke
  per-flow caches).
- `approverResolver` — subject → canonical IDs (direct-user shortcut or
  `LookupSubjects`) → cap at `approverFanoutCap` (50) → Slack user_ids,
  collecting per-canonical `resolveError`s (never silently dropped).
- `deliverer` — posts a prompt under a surface policy (below).
- `encodeApprovalButtonValue` / `decodeApprovalButtonValue` — shared `{v,r,d,s}`
  button-value codec (tool_approval + info_leakage +
  content_inspection_approval).
- `postToResponseURL` — Slack interactive `response_url` POST (ephemeral edits).

## House-style grammar (Block Kit everywhere)

Every authz message is **Block Kit**. Plain text is used ONLY as the Slack
notification-preview string (`MsgOptionText`), never as the message body.

**Canonical block order:** Header → content sections (lead, `*Why*`, `*What*`,
`*Looking at:*`, `*Would share with:*`, `Tool · Permission`) → action block
(Approve / Deny [+ Show Details]) → context footer
(`Requested by … · session \`ns/name\``). The resolved view is the same minus
the action block, plus an outcome line.

Each flow's per-recipient prompt renderer (`toolApprovalPrompt.Render`,
`infoLeakagePrompt.Render`) produces this shape independently.
`BuildApprovalPromptBlocks` / `BuildApprovalResolvedBlocks` in
`approval_render.go` are the shared renderer used by `info_leakage_approval`;
`tool_approval` renders its own distinct block sequence inline.

**Styling conventions:**

- Status icons: 🟡 pending · ✅ approved · 🛑 denied · ⏹️ timeout · ⚠️ warning.
- Buttons: Approve = `Primary` (green) under default (operator) identity mode;
  under `userPassthrough` Approve drops Primary (renders neutral) and is
  relabeled; Deny = `Danger` (red) for all flows; Show Details = default.

`TestAuthzFlows_PromptsFollowHouseStyle` in `authzmsg_conformance_test.go`
enforces the common house-style properties on the driver's flows' real rendered
output: header-first block order, exactly one ActionBlock (not first), and
Approve=Primary / Deny=Danger under the default identity mode.

## Surface policy (`deliverer`)

| Policy                           | Behavior                                                                             |
| -------------------------------- | ------------------------------------------------------------------------------------ |
| `surfaceEphemeralWithDMFallback` | `chat.postEphemeral` in-channel; on `user_not_in_channel`, open a DM and post there. |
| `surfaceDMOnly`                  | Always open a DM and post there (single named recipient).                            |
| `surfaceEphemeralOnly`           | `chat.postEphemeral` only; surface any error.                                        |

Mapping of `(Category, Audience)` to surface, today:

| Category / Audience              | Slack surface                                  |
| -------------------------------- | ---------------------------------------------- |
| decision_request → approvers     | ephemeral in-thread, DM fallback               |
| decision_request → specific_user | DM only                                        |
| pending_status → participants    | thread message (chat.update on tick / resolve) |
| decision_outcome → participants  | edit the thread message                        |
| decision_outcome → clicker       | `response_url` (replace_original)              |
| Show Details (modal)             | `views.open`                                   |

## Restart resilience — two strategies

A channelsd restart loses the in-process `refStore`. Each flow stays correct via
one of:

1. **Re-render from payload** (tool_approval): on a cache miss the `Outcome`
   re-renders the resolved view from the self-contained applied payload.
2. **Self-contained applied payload + `response_url`** (info_leakage,
   content_inspection_approval): no cache at all; the applied payload carries
   the original-request context.

A third strategy — **button-packed + pipeline-echoed ref** — existed for the
Slack-bespoke `permission_request` flow (the message refs travelled in the
button value and in `SubChannelSendResult.RequestRef`). It was retired along
with that flow: `permission_request` (the multiplayer session-join approval) no
longer runs on this driver at all — its publisher moved onto the generic
Interaction model
(`channelevents.KindInteractionRequest`/`KindInteractionApplied`, category
`permission_request`), rendered by `interaction.go`'s `interactionSender` on the
`"interaction"` sub-channel instead. See `../../channelinteractions/` and
`interaction.go`'s file-top comment for that model's contract.

## Flows on this driver today

| Flow                          | Audience                      | Public post          | Prompt surface          | Pending | Outcome target                          | Restart                | Buttons                       |
| ----------------------------- | ----------------------------- | -------------------- | ----------------------- | ------- | --------------------------------------- | ---------------------- | ----------------------------- |
| `tool_approval`               | subject fan-out               | thread Block Kit     | ephemeral + DM fallback | ticker  | RefStore → chat.update / payload repost | re-render from payload | shared codec (+ Show Details) |
| `info_leakage_approval`       | subject fan-out / direct user | **none (sensitive)** | ephemeral + DM fallback | —       | `response_url` edit                     | self-contained payload | shared codec                  |
| `content_inspection_approval` | subject fan-out               | **none (sensitive)** | ephemeral + DM fallback | —       | `response_url` edit                     | self-contained payload | shared codec                  |

**info_leakage safety properties** (enforced in code + tests): never posts
publicly (`PublicPost: nil`); fails the request if the approver candidates
resolve to no reachable Slack user (`RequireDelivery`); fails the request if the
"would share with" audience can't be named (`Prompt.Render` returns an error).

## Adding a Slack authz kind

1. Define the `Kind` + payload in `pkg/channels/channelevents` and add a
   `Classify` entry (see `../AUTHZMESSAGING.md`).
2. Write `flow_<name>.go`: a `flow<Name>()` returning an `authzFlow` whose
   strategies render via the house-style grammar. Choose `PublicPost: nil` for
   sensitive kinds; pick a surface policy; set `RequireDelivery` if a failed
   prompt means a dead request; reuse `refStore`/`BuildRequestRef` for the
   restart strategy that fits.
3. Add a `case` in the Kind's `SubChannelSender` (`kind.go`) returning
   `newAuthzShim(deps, flow<Name>(...))`.
4. If the prompt has buttons, add a decode branch in `listener.go` (reuse
   `decodeApprovalButtonValue` for the shared codec, or a flow-specific value).
5. Add a row to the tables above and uphold every cross-channel invariant.
