# `pkg/agent/postsession`

Reporters that run once, at the end of a session. Each is an ordinary
`pipeline.Hook` on the `SessionEnd` point, registered through the runner's
hook-factory registry ([`runner/hookregistry.go`](../runner/hookregistry.go)) —
not a special case in the loop.

The directory itself holds no Go code; it is the named home so future reporters
(usage export, webhooks) have somewhere to land next to the first one.

## Subpackages

- [`cost`](./cost/) — end-of-session LLM cost estimate.

## See also

- [`pkg/agent`](../) — group overview.
