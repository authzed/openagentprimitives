# `pkg/web/webui/agentui`

The `webui` plug-in (`"agent-ui"`) backing the **agent-defined view** of a
session: the resolution ladder that turns an `AgentSession` into a validated
declaration, and the session-scoped API routes a browser drives that view
through.

**It serves no page of its own.** The view renders inside the session shell's
content region ([`sessions`](../sessions)), which owns the chrome.
`GET /agent-ui/{ns}/{name}` is just a redirect into that shell. Its `ui/`
directory has no `app.json`, so it ships no bundle of its own — the shell
imports its components.

`{name}` is always an **AgentSession** name, never an AgentClass name.

## Routes

| Route | Auth |
| ----- | ---- |
| `GET /agent-ui/{ns}/{name}` | `AuthNone` — a redirect that reads nothing |
| `POST .../bindings` | `AuthAuthenticated` |
| `POST .../actions` | `AuthAuthenticated` |
| `GET .../live` | `AuthAuthenticated` (websocket) |
| `POST .../start` | `AuthAuthenticated` — mounted only when a start dep is wired |

## Authorization

One gate: **`agentsession:<ns>/<name>#interact@user:<canonicalID>`**, always
**fully consistent**. `gateAgentUIPost` is the reference implementation
(auth → CSRF origin pin → `CheckInteract`); the live websocket runs the same
check inline **before** the upgrade. A check error is **503**, never 403.

**Never `CheckArtifactView` / `CheckView` here.** Those resolve to
`parent->interact + platform->view_audit`, a strict superset that would admit
platform admins into a session-scoped agent UI as if they were participants in
it.

The redirect route is `AuthNone` deliberately: it reads nothing, so a
404-on-unknown variant would *add* an existence oracle.

The `start` route's authorization is **derived, not separate** — a viewer may
start a session of class C in namespace N iff they already hold `interact` on
some session of class C in N. The class is read from the server's copy of the
named session, never from the request body.

## The two tiers

| Tier | Author | Where it lives |
| ---- | ------ | -------------- |
| **Tier 0** | A human or bundle author | The `AgentUI` CR — `spec.view` (the page tree; `spec.slots` is the legacy flat shape, compiled into hooks), `spec.actions`, `spec.tools`, `spec.chrome`. An `AgentClass` points at it and grants tools via `spec.agentUI.grantedTools`. |
| **Tier 1** | The **agent**, at runtime | Per-hook fragments — targeting an `oap:generative` node in the page tree — written through the `update_view` tool, stored in memory kinds. |

Per request this package walks the ladder (session → `ResolveSession` → class →
`AgentUI` → require `Valid`), merges the two tiers via `uiview.Resolve`,
re-validates, and serves **one** merged document to all four consumers through a
single producer. Rejected fragments are logged, never surfaced.

The ladder's failure codes are meaningful: an `Ended` session is 410, a
`Valid=Unknown` AgentUI is 503, and `Valid=False` is 422 carrying the
reconciler's own message.

## What the browser may vary — and may not

This is the package's core safety property.

- **Bindings:** the browser sends **only** `{"params": {...}}`. Source, ref, and
  args-template always come from the server-side declaration.
- **Actions:** the browser sends **only** `{"action","params","inputs"}`. The
  tool comes from the declaration's `action.Tool`.

## Files

| File | What it does |
| ---- | ------------ |
| `agentui.go` | Plug-in skeleton, the `Deps` cast (fail-closed and logged), route registration |
| `deps.go` | The collaborator interface `webd` must satisfy |
| `page.go` | The session → class → UI resolution ladder |
| `session.go` | Pure `ResolveSession` decision → `Attached` / `Asleep` / `Ended` |
| `viewmodel.go` | Merges Tier-0 with Tier-1, re-validates, produces the browser wire document |
| `bindings.go` | The bindings route, plus `gateAgentUIPost` and the shared JSON helpers |
| `actions.go` | Invokes one *declared* action over NATS and relays the reply |
| `live.go` | Websocket: subscribe first, then snapshot, then relay updates |
| `start.go` | Starts a **replacement** session for an `Ended` one |
| `redirect.go` | Redirects into the session shell |
| `wake.go` | Stamps a wake-request annotation when a binding hits an unreachable runner |

## Collaborators

It composes the `pkg/web/ui*` packages rather than reimplementing them:
[`uibindings`](../../uibindings) + its registry (it **never** branches on a
source string), [`uicomponents`](../../uicomponents),
[`uiselect`](../../uiselect), [`uiview`](../../uiview), and
[`uigrant`](../../uigrant). It does **not** use
[`viewurn`](../../viewurn) — its actions carry a server-minted request ID over
NATS rather than a Via string.

**Known gap**, documented in `viewmodel.go`: webd computes its tool ceiling from
the two-party `uigrant.Ceiling` only, which is a strict subset of what the
AgentUI reconciler evaluates.
