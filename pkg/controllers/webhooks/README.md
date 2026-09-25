# `webhooks` — admission webhooks

The author checks the API server cannot express. Each package registers a
`ValidatingWebhook` handler at a path that must match the
`ValidatingWebhookConfiguration` in `config/`.

| Package | Guards |
| ------- | ------ |
| [`agentsession/`](agentsession/) | Refuses a session **runner** rewriting AgentSession fields the operator and channelsd treat as authority |
| [`toolcall/`](toolcall/) | Pins the runner's already-made authz decision so it cannot be tampered with between creation and execution |
| [`settings/`](settings/) | Denies specs that contradict the effective tiered settings, or that are not self-consistent |
| [`skill/`](skill/) | Name / frontmatter validation and the provenance check, for `Skill` and `ClusterSkill` |

## Why a webhook and not CEL

Two limits recur across these packages:

- **A CRD structural schema may only describe `name` and `generateName` under
  `metadata`**, so no `x-kubernetes-validations` rule can observe annotations or
  labels at all.
- **CRD validation rules are identity-blind.** `self` / `oldSelf` carry no
  requester, so a transition rule strong enough to stop the runner also stops the
  legitimate writer. Several fields here have two legitimate writers.

## `failurePolicy` is a per-webhook judgement

The `agentsession` and `toolcall` webhooks use **`Fail`**. Their
`ValidatingWebhookConfiguration` narrows them with `matchConditions` to requests
from `*-runner-sa` service accounts; `matchConditions` are evaluated inside the
API server *before* the webhook is dialed, so a non-matching request is never
sent and `failurePolicy` never applies to it. A webhook outage therefore cannot
block the operator, channelsd, webd, or a human with kubectl. `Ignore` is not an
option for a gate whose whole job is refusing a forgery — an attacker who can
crash or saturate the webhook, or who simply waits for a rolling update, would
walk straight through it.

The `settings` webhooks deliberately **fail open** on resolution errors: the
corresponding controller is the real gate, and this handler is an early,
better-error convenience.
