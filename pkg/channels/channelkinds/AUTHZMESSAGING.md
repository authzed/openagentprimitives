# Authz Messaging — Cross-Channel Guidelines

This document defines how **authz-messaging** works across *all* channel
transports. It is the generic contract: it fixes *what* an authz message is and
the *invariants* every channel must uphold. *How* a message is surfaced
(ephemeral, DM, thread, modal, email, …) is each channel's business and lives in
that channel package's own `AUTHZMESSAGING.md`.

> **Authz messaging** = the messages the authz / identity system uses to
> communicate with humans: approval requests, denials, pending/awaiting status,
> informational notices, and warnings. It is distinct from general agent
> conversation, streaming output, and tool-session I/O.

## Vocabulary

The vocabulary is defined in code — `pkg/channels/channelevents/authzclass.go` is the
**source of truth** for the types below. This doc is the prose companion.

A single authz-message event is described by three orthogonal axes, a
cross-cutting severity, and a lifecycle.

### Category — what the message is for (`channelevents.Category`)

| Value | Meaning |
| --- | --- |
| `decision_request` | An approve/deny gate on a guarded operation. |
| `action_request` | A non-binary call-to-action (link a credential, open a portal). |
| `pending_status` | An awaiting / in-progress state; may update over time. |
| `decision_outcome` | A resolution: approved / denied / timeout. |
| `notice` | Informational; no action required. |

### Severity — cross-cutting tone (`channelevents.Severity`)

`info` · `warning` · `danger`. **"Warnings" live here, not as a category.** For
example the `userPassthrough` elevated-risk framing is a `warning` *on a*
`decision_request` — same category, escalated tone.

### Audience — who should see it (`channelevents.Audience`)

| Value | Meaning |
| --- | --- |
| `approvers` | Resolved from a SpiceDB subject ref. |
| `requester` | The person who triggered the gated action. |
| `participants` | The thread / public surface. |
| `specific_user` | A named user (e.g. `started_by`). |

Audience is gated by the `Sensitive` flag on `AuthzClass` (see invariants).

### Affordance — interaction surface (`channelevents.Affordance`)

`none` · `buttons` · `modal` · `link`. The decision *return path* (block_action
vs response_url vs reply, etc.) is a channel implementation detail and is **not**
part of this cross-channel vocabulary.

### Lifecycle

`request → [pending updates] → outcome`. Every message is **rendered from its
payload** (so it survives a channel-daemon restart) and correlated across the
lifecycle by `RequestID`.

### Primary classification

`channelevents.Classify(kind)` returns the **primary** `AuthzClass` for a Kind
(`Category`, `DefaultAudience`, `Severity`, `Sensitive`), or `ok == false` for
non-authz kinds. A Kind classifies by its primary intent; a flow may still emit
*secondary* messages of other categories (e.g. a `decision_request` flow also
posting a `pending_status` to `participants`). That richer behavior belongs in
the per-channel descriptor, not in this table.

## Roster

| Kind / flow | Category | Default audience | Sensitive |
| --- | --- | --- | --- |
| `tool_approval` | decision_request → pending_status → decision_outcome | approvers (prompt) + participants (status/outcome) | no |
| `permission_request` | decision_request + pending_status → decision_outcome | specific_user `started_by` (prompt) + participants (pending) + requester (outcome) | no |
| `info_leakage_approval` | decision_request → decision_outcome | approvers | **yes** |
| `info_leakage_notice` | notice | requester / specific_user | yes |
| `credential_request` | action_request | specific_user | yes |
| `credential_linked` / `credential_revoked` | notice | specific_user | yes |
| `portal_access` | action_request | specific_user | yes |
| `provider_error_retry` | decision_request | approvers / specific_user | (confirm at migration) |

> `live_view_offer` is **not** authz messaging — it is an artifact/UX surface and
> is intentionally excluded from this roster.

> Only `tool_approval`, `permission_request`, and `info_leakage_approval` are
> classified in code today; the rest are migrated in later phases (each adds its
> `Classify` entry then).

## Invariants every channel MUST uphold

1. **Never silently drop.** Every authz message is delivered, logged, or
   surfaced to the user. No swallowed errors. (Repo-wide rule; see
   `AGENTS.md` → "Never silently drop errors".)
2. **Private-by-default for sensitive.** A `Sensitive` Kind MUST NOT be routed
   to `participants`. Broadcasting "Alice wants to share X with Bob" is itself a
   leak.
3. **Render from payload.** Pending and outcome messages re-render from the wire
   payload, not solely from in-process state — so a channel-daemon restart
   between request and resolution still shows the correct outcome.
4. **Name the actor.** Outcomes name the approver who decided; pending names who
   is being awaited. No faceless "request resolved".
5. **Correlate by `RequestID`.** All messages in one lifecycle share a stable id.
6. **Server-side decision is authoritative.** The channel render is best-effort;
   the authoritative grant/deny write is the source of truth. A failed render
   never changes the decision.
7. **Resolution degrades, never drops.** If approver resolution is unavailable
   (no resolver wired) or partially fails, surface fewer recipients and log it —
   never silently send nothing.
8. **Summarized-but-grounded.** Where a message summarizes (e.g. an LLM "what
   this does" line), always provide an escape hatch to ground truth (raw args /
   upstream description). The summarizer never sees primary-LLM output — that is
   a prompt-injection boundary.

## Adding a new authz message kind

1. Define the `Kind` + its payload struct in `pkg/channels/channelevents`.
2. Add a `Classify` entry in `pkg/channels/channelevents/authzclass.go` with the Kind's
   primary `Category`, `DefaultAudience`, `Severity`, and `Sensitive` flag.
3. In **each** channel that should surface it, implement and register a flow
   descriptor and document the channel-specific mapping in that channel's
   `AUTHZMESSAGING.md` (e.g. `pkg/channels/channelkinds/slack/AUTHZMESSAGING.md`).
4. Uphold every invariant above. If a channel genuinely cannot surface a Kind,
   that is a documented, logged degradation — not a silent no-op.

## Boundary

This cross-channel layer fixes **what** an authz message is and the
**invariants**. It deliberately does **not** prescribe surfaces, affordances, or
sub-channel mechanics — channels differ too much for that to be useful. Each
channel owns its own mapping.
