# `webhooks/settings` — tiered-settings admission

Denies specs that contradict the effective tiered settings, or that are not
self-consistent with themselves. Five handlers share one `base` (a read-only
client plus a decoder).

| Handler | Path | Denies |
| ------- | ---- | ------ |
| `AgentClassWebhook` | `/validate-agentclass` | A **specified** (not inherited) model/toolkit/MCP that the effective settings disallow |
| `AgentSessionWebhook` | `/validate-agentsession` | A specified-but-disallowed value, or a model unresolvable at session runtime |
| `ClusterAgentSettingsWebhook` | `/validate-clusteragentsettings` | A non-singleton name, or defaults that contradict the object's own limits |
| `AgentSettingsWebhook` | `/validate-agentsettings` | The same self-consistency rules, namespaced |
| `ClusterIdentityProviderWebhook` | `/validate-clusteridentityprovider` | A non-singleton name, or a spec that is not fail-closed-valid |

| Validation helper | Checks |
| ----------------- | ------ |
| `SelfConsistencyError` | Defaults do not contradict limits within one spec |
| `PinningKindsError` | Every `limits.pinning` rule and bypass names a registered pinning kind |
| `ModelCatalogError` | The model catalog is well-formed (cluster and namespaced rules differ) |
| `ContentInspectorsError` | The declared content inspectors are valid |

## Non-obvious constraints

- **These handlers fail open; the controllers are the real gate.** When the
  AgentClass is not found or settings resolution errors, the webhook admits. It
  exists to turn a late runtime failure into an early, legible `kubectl apply`
  error — not to be the authorization boundary.
- **A *missing* model is not denied on AgentClass, but is on AgentSession.** A
  default tier may supply the model before any session runs; by session time it
  must resolve (`ForSession=true` makes a missing model fatal).
- **The validation helpers are shared with the controllers**, deliberately.
  `IdPSpecError` is called by both this package and
  [`../../clusteridentityprovider`](../../clusteridentityprovider/) so the
  webhook and the controller share one judgment and can never disagree.
- **The paths above must match the `ValidatingWebhookConfiguration`** in
  `config/`. Changing one is a two-file edit plus `mage manifests`.
- **Registration is by the pinning registry, not a list here.**
  `PinningKindsError` reads `pkg/authz/pinning/registry`, so a new pinning kind
  is accepted without touching this package.
