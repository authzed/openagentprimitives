# `pkg/channels` — transports and the conversation surface

Everything between a human on some transport (Slack, a terminal, a browser tab,
a cron trigger) and the agent runner: the transport plug-ins, the daemon that
hosts them, the wire vocabulary they speak over NATS, and the rendering seams
for prompts, notices and artifacts.

| Package                                        | What it holds                                                                                                 |
| ---------------------------------------------- | ------------------------------------------------------------------------------------------------------------- |
| [`channelassets/`](channelassets/)             | Renderer plug-in surface for agent-produced visual artifacts (html, css, svg, image, mcpui)                   |
| [`channelevents/`](channelevents/)             | The NATS wire format: `Envelope`, the closed sub-channel `Kind` enum, and each kind's payload type            |
| [`channelfeatures/`](channelfeatures/)         | Kind-agnostic vocabulary mapping agent _capabilities_ to the transport _permissions_ a channel needs          |
| [`channelinteractions/`](channelinteractions/) | Semantic interaction-category registry — one row per prompt/notice type, dispatched generically by every kind |
| [`channelkey/`](channelkey/)                   | The single canonical channel-key → label-value function                                                       |
| [`channelkinds/`](channelkinds/)               | The transport plug-ins themselves, plus the `Kind` interface they implement                                   |
| [`channelsd/`](channelsd/)                     | The inbound listener + outbound relay daemon's libraries                                                      |
| [`interact/`](interact/)                       | Interaction-kind registry for webd's `/interact` endpoint — "what did the human do"                           |
| [`notice/`](notice/)                           | Authoring seam for one-way user-facing messages (errors, denials, warnings, lifecycle events)                 |

## Channel kinds are a registry, never a switch

Each kind package calls `registry.Register(&Kind{})` from an `init()`. Nothing
outside the kind's own package branches on the kind name — the relay, the wizard
dispatcher and the status reconcilers all dispatch through
[`channelkinds.Kind`](channelkinds/kind.go).

Adding a transport:

1. New package `channelkinds/<name>/` with `kind.go`, `listener.go`,
   `sender.go`, `wizard.go`.
2. `func init() { registry.Register(&Kind{}) }` in `kind.go`.
3. Add the blank import (`_ ".../channelkinds/<name>"`) to the binaries that
   need it — `internal/cmd/channelsd`, `internal/cmd/operator`,
   `internal/cmd/runner`, `internal/cmd/webd`, `cmd/oap/internal/channelcmd`.
   Which binaries need it depends on the kind; the operator registers every kind
   purely so the Channel controller can validate a spec that names it.

A kind missing from a binary's blank imports is a wiring bug, not a silent
degradation: the Channel controller reports `unknown kind %q`.

## Client-hosted vs. relayed

`Kind.RelayedByChannelsd()` splits the kinds in two. Slack and bento run their
listener and sender inside `channelsd`. `local` (the `oap` TUI) and `browser`
(the built-in web chat) run theirs inside the process that owns the surface —
`oap` and `webd` respectively — and `channelsd` registers them only so it
recognises and _skips_ them instead of erroring. The shared half of that shape
lives in [`channelkinds/clienthosted/`](channelkinds/clienthosted/).

## The subject is the routing authority

`Envelope.Session` is advisory publisher-controlled JSON. The NATS subject an
envelope arrived on is the only session identity a publisher's per-session JWT
authorized. Any consumer taking an envelope off the bus must cross-check the two
and drop a mismatch — see the `Session` field's doc comment in
[`channelevents/envelope.go`](channelevents/envelope.go).
