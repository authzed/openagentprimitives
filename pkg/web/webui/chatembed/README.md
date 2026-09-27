# `pkg/web/webui/chatembed`

The `webui` plug-in (`"chat-embed"`) serving the one-session browser chat as an
embeddable page: `GET /chat-embed/{ns}/{name}`, `AuthAuthenticated`, trusted
origin. It mounts the transcript data plane's `ChatView` plus the session
shell's `StartupLine`, with no tabs, header, or session list.

**`agentsession#interact` is the only gate**, checked in the page build on the
authenticated viewer — the framed page is the enforcement, not the frame around
it. It sets `EmbeddableSameOrigin` (never `FramesSandbox`, since it frames
nothing itself), so another same-origin page — the agent-builder's Test panel,
via `ap:chat` — may embed it; cross-origin framing stays blocked.
