# `channelassets` — artifact renderer plug-ins

The renderer plug-in surface for agent-produced visual artifacts. A renderer
turns agent-authored source (HTML, CSS, SVG, an image, an MCP-UI widget) into
persisted, sanitized bytes. The `artifactrender` controller dispatches to the
registered kind at reconcile time; the runner uses the same registry for
capability negotiation.

This package holds only the [contract](contract.go) — `Renderer`,
`ExecutionMode`, `DeliveryMode`, and the hard output-MIME deny-list. It is
deliberately free of Kubernetes API dependencies so plug-ins stay
unit-testable in isolation.

## Kinds

| Package | Notes |
| ------- | ----- |
| [`html/`](html/) | Sanitizes via bluemonday, injects a restrictive CSP `<meta>`, warns on every dropped element/attribute |
| [`css/`](css/) | Standalone stylesheets, through the shared sanitizer |
| [`svg/`](svg/) | |
| [`image/`](image/) | |
| [`mcpui/`](mcpui/) | **Does not sanitize.** Widget HTML must run its own JavaScript for the protocol's postMessage bridge; safety comes entirely from the sandboxed serving route. Operator-only — not agent-selectable |
| [`registry/`](registry/) | The process-wide renderer registry |
| [`internal/`](internal/) | Sanitizer internals shared between `html` and `css` |

## Registering

Each kind's `init.go` calls `registry.Register(New())`. Blank imports live in
`internal/cmd/operator`, `internal/cmd/runner` and `internal/cmd/webd` —
except `mcpui`, which only the operator imports.

## Non-obvious constraints

- **`OutputMIMEs()` is validated at registration.** A denied MIME panics the
  registry unless the kind declares `Delivery() == DeliveryBundledOnly`. The
  check runs *before* the renderer is stored, which is why it lives in
  `registry/` rather than in `kindregistry`.
- **`DeliveryBundledOnly` means never a top-level browser document.** Such a
  kind's preview is composed as HTML and rendered by the `html` kind. It does
  not block channel file delivery — that is governed separately by asset
  capability and sanitization.
- **Every registered kind is `InProcess`.** `PodSpawn` is declared for a future
  heavy renderer and fails fast today with `FailureReason=RendererUnknown`, so
  the CRD shape does not change when it lands.
