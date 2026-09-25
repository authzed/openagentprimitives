# `pkg/web/webui/sessionview`

The `webui` plug-in (`"session-view"`) serving a **session-scoped** (not
artifact-scoped) shared live view: a read-only mirror of a session's
conversation for a subject who can interact with it but was not the session's
originating channel. It also hosts **MCP-UI widgets** on the sandbox origin.

The session is named entirely in the path, so there is no signed link parameter
to verify. There is no send path here — that is [`interact`](../interact)'s job.

## Routes

| Route | Origin | Auth |
| ----- | ------ | ---- |
| `GET /session-view/{ns}/{name}` | trusted | `AuthAuthenticated` (frames the sandbox) |
| `GET /session-view/{ns}/{name}/live` | trusted | `AuthAuthenticated` (websocket) |
| `GET /mcpui-content`, `/mcpui-host`, `/mcpui-host.js` | **sandbox** | `AuthNone` — the content token *is* the authorization |

## Authorization

**`agentsession#interact` is the only gate this page uses**, checked on the
shell build and again inline before the websocket upgrade. Both fail closed to
500 on a check error, 403 on a denial.

**Never `CheckArtifactView` / `CheckView`.** Those are a strict superset
(`parent->interact + platform->view_audit`) that would admit platform admins
into a session-scoped view as if they were a participant in it.

## Widget hosting is the inverse of artifact hosting

Read this next to [`artifactview`](../artifactview), because the sandbox posture
is deliberately **opposite**:

| | Artifact content | MCP-UI widget |
| --- | --- | --- |
| Inner frame | `sandbox="allow-same-origin"` — no scripts | `sandbox="allow-scripts"` — no same-origin |
| Effect | The bytes cannot execute | The bytes execute in a **unique opaque origin** |

Widgets are interactive, so their script must run; safety comes from the opaque
origin plus CSP instead of from script-disabling.

**Widget bytes are served verbatim** — no sanitize, no transform, no ref
rewriting. Safety comes entirely from the sandbox framing plus the response's
own CSP, *never* from transforming the bytes. This is also why `script-src`
includes `'unsafe-inline'`: the markup is never nonced, so a nonce-based policy
would simply block it.

Two subtleties that are easy to break:

- The widget CSP is `frame-ancestors 'self' <shellOrigin>` — **both** are
  required, because CSP3 checks *every* ancestor, and the chain is two deep
  (shell → host → content).
- `/mcpui-host`'s CSP is load-bearing because a `srcDoc` child with no CSP of
  its own **inherits its creator's**, so that header becomes the widget's
  effective policy.

The `@mcp-ui/client` library's `"src"` render mode unconditionally adds
`allow-same-origin` and is **never used** — the code forces the `srcDoc` path,
and a test asserts it.

## Content tokens are minted server-side

The browser holds no signing key, so widget host URLs are minted at relay time.
A mint failure degrades to an **empty** host URL, which the client skips — never
a fabricated one.

## Files

| File | What it holds |
| ---- | ------------- |
| `sessionview.go` | Plug-in skeleton and routes |
| `deps.go` | `Deps`, plus `WidgetRef` / `WidgetCSPMeta` / `WidgetMeta` |
| `page.go` | The shell build, sandbox origin resolution, active-widget props |
| `live.go` | The websocket mirror |
| `widgets.go` | Sandbox-origin MCP-UI serving and its CSP construction |

`widgets.go` builds its CSP by hand with `webui.SanitizeCSPSourceToken` rather
than through [`cspassets`](../cspassets), because a legitimate widget domain may
be a wildcard like `https://*.cdn.example.com` that origin reconstruction would
mangle.
