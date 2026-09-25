# `pkg/web/webui/artifactview`

The `webui` plug-in (`"artifact-live-view"`) that shows a session's rendered
artifacts in the browser. **This is the most security-sensitive package in
`pkg/web`** — it renders agent-authored bytes, so nearly everything here is
containment. Read this file before changing any of it.

## It spans both origins

| Origin | Routes | Auth |
| ------ | ------ | ---- |
| **Trusted** (holds the cookie) | `GET /artifact-view`, `/artifact-view/revision`, `/artifact-view/ws`, `/artifact-download` | authenticated |
| **Sandbox** (never sees the cookie) | `GET /content`, `/artifacts/a/`, `/artifact-host`, `/artifact-host.js` | `AuthNone` — **the capability token is the authorization** |

The sandbox base URL is reduced to a bare `scheme://host` through
`webui.StrictOrigin` in exactly one place, and **`urlsFor` fails closed on an
empty or non-`http(s)` base**. That matters because empty is a *live* state:
`oap install` seeds the external-URL ConfigMap with `""` while it manages
external access. Concatenating onto `""` would yield a shell-**relative**
`/artifact-host?ct=…`, loading sandbox content in the trusted origin and
collapsing the two-origin split entirely.

## Three nested frames, three different postures

```
trusted shell  ──frame──▶  sandbox host page  ──frame──▶  artifact content
sandbox="allow-scripts        sandbox="allow-same-origin"
       allow-same-origin"     (NO allow-scripts)
```

The **inner** frame omits `allow-scripts`, so the artifact's own JavaScript can
never run — structurally, not by policy. The host page is same-origin with it,
so it reads the DOM and preserves scroll directly, and its **only** exit is a
`postMessage` to the parent shell: it is granted no `connect-src`, so it has no
network capability at all.

## Four layers of CSP

1. **The trusted shell page** — the framework policy from `webui/document.go`,
   plus `frame-src <sandbox origin>` because the page declares `FramesSandbox`.
2. **The sandbox host page** — `default-src 'none'; frame-src 'self';
   frame-ancestors <shell origin>; base-uri 'none'; form-action 'none'`, with
   `script-src`/`style-src` nonces **derived** by
   [`cspassets`](../cspassets) from what was actually registered.
3. **`/content` sets no CSP at all** — deliberately. Its safety comes entirely
   from the script-disabled framing above it.
4. **A `<meta http-equiv>` policy baked into the artifact HTML itself**, by the
   renderer at render time.

Two fail-closed details in the host page are load-bearing:

- The shell origin is sanitized **before** it reaches either the CSP sink or a
  JS string literal. The CSP sink is a raw, unquoted token — a `;` or a space
  would inject an extra directive or widen `frame-ancestors` to another host.
- An unknown shell origin becomes `'none'`, never an empty source list. An empty
  list is invalid CSP, so the UA *discards* the directive — and `frame-ancestors`
  has no `default-src` fallback, making a discarded directive identical to an
  omitted one: any origin may frame the page.

## Sanitization happens elsewhere

**This package never sanitizes.** Artifact HTML is sanitized at *render* time by
the renderer in `pkg/channels/channelassets/html` (bluemonday under a custom
policy, with diff-based warnings naming every dropped element). `artifactview`
serves already-sanitized bytes **inert**; its `ServeTransform` returns content
unchanged for every kind today.

`ensureUTF8Charset` exists because the renderer strips `<meta charset>` during
head sanitization and this handler sends `nosniff` — without an explicit HTTP
charset the browser would fall back to Latin-1.

Every byte-serving route sets `X-Content-Type-Options: nosniff`.

## Two authorization gates, not one

`bindView` is the single choke point for the trusted routes and gates on
`CheckView`. The **session-scoped mirrors** (status, plan, conversation) on the
websocket require an *additional* `CheckInteract`: holding `view_audit` lets a
platform admin look at any artifact, but that is not a licence to read the
session's conversation, plan, and notifications as if they were a participant.
An error **disables** mirrors, never grants them.

**`/artifacts/a/` has no per-secondary `CheckView`, on purpose.** `artifact#view`
is session-granular in the schema (`view = parent->interact + platform->view_audit`,
with every artifact's parent being its owning session and no per-artifact ACL),
so the one check plus the asset capability token is exactly equivalent. **If the
schema ever adds a per-artifact `view` component, this route must add a
per-secondary check.**

## Content tokens

One token is minted per push, fresh, and gates **both** the host and content
URLs; a *different* token kind gates assets. See
[`contenttoken`](../contenttoken) — the kind field pins a token to exactly one
route, so a `/content` token can never reach the script-enabled widget route.

## Why download may live on the trusted origin

The bytes are untrusted, agent-generated content.
`Content-Disposition: attachment` forces the browser to **download** rather than
render them, so they never execute in the trusted origin, and `nosniff` blocks
MIME-sniffing a `text/plain` body into executable HTML. That pairing is the only
reason a download may be served from the trusted origin at all, while inline
render remains sandbox-origin only.

## Files

| File | What it does |
| ---- | ------------ |
| `artifactview.go` | Plug-in skeleton and the route table, with per-route origin/auth assignment |
| `deps.go` | The collaborator interface, plus the status/mirror/revision types |
| `bind.go` | **`bindView`** — the authorization choke point, plus `viewDenial` and `mirrorsAllowed` |
| `helpers.go` | Param resolution (signed link vs unsigned), `urlsFor`, `sandboxOrigin` |
| `page.go` | The trusted-origin React shell's `Page.Build` |
| `host.go` | Builds the sandbox host document, its CSP, the annotator CSS, and the bridge |
| `hosthandler.go` | Serves the host page after verifying the content token |
| `content.go` | Serves artifact bytes: verify token, fetch, transform, rewrite refs, set headers |
| `asset.go` | Serves a same-session secondary artifact, gated by an asset-kind token |
| `rewrite.go` | Rewrites `artifact:HANDLE` refs to token-gated URLs, dispatching generically |
| `revision.go` | Resolves the render to frame for a pinned revision |
| `download.go` | Attachment download with filename sanitization |
| `live.go` | The websocket: snapshot, poll for head changes, plus the gated mirrors |

A missing `artifact-host` bundle **omits the route and logs**, rather than
panicking — the page's `<script src>` then 404s, which is visible and
diagnosable rather than silent.
