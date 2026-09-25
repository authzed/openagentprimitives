# `channelkinds` — the transport plug-ins

The `Kind` interface every channel transport implements, plus each transport's
implementation. `internal/cmd/channelsd` consumes `Kind.NewListener` /
`Kind.NewSender`; `cmd/oap` consumes `Kind.Wizard()`.

## Registered kinds

| Package | Transport |
| ------- | --------- |
| [`slack/`](slack/) | Slack, via Socket Mode + the Web API |
| [`bento/`](bento/) | Cron-style triggers from an embedded Bento `generate` input — no human user |
| [`local/`](local/) | The `oap` chat TUI. Client-hosted |
| [`browser/`](browser/) | The built-in web chat. Client-hosted, served by webd |
| [`fake/`](fake/) | Recording kind for tests and e2e |

## Support packages

| Package | What it holds |
| ------- | ------------- |
| [`registry/`](registry/) | The process-wide kind registry, plus `IsBrowserSurface` |
| [`clienthosted/`](clienthosted/) | The one Listener and single-user `AudienceResolver` shared by `local` and `browser` |
| [`resolve/`](resolve/) | Loads the bound Channel CR, its Secret, and the registered Kind for a session |
| [`outputbind/`](outputbind/) | Resolves the dedicated output Channel for a `role=input` Channel |
| [`wizardkeys/`](wizardkeys/) | The wizard questions more than one kind asks: their answer keys, prompt text and validation (Channel name, authzSubject) |
| [`kindtest/`](kindtest/) | The conformance harness every kind must pass |

## The interface is wide and mostly optional

[`kind.go`](kind.go) declares `Kind`, `Listener` and `Sender` — the required
core — plus roughly two dozen **optional** capability interfaces a kind may
also implement: `AudienceResolver`, `SessionViewMinter`, `ConversationReader`,
`StreamDeltaSink`, `RestartCapable`, `TextFormatter`, `UserProfileProvider`,
`BrowserSurface`, and so on. Consumers type-assert; a kind that does not
implement one simply does not offer that capability.

**That is the extension point.** When you find yourself wanting
`if kind == "slack"` outside `slack/`, the abstraction is missing a method —
add an optional interface here, do not add a switch in the consumer.

## Registering

`func init() { registry.Register(&Kind{}) }` in the kind's `kind.go` (or
`init.go`). Blank imports go in the binaries that need it:
`internal/cmd/channelsd`, `internal/cmd/operator`, `internal/cmd/runner`,
`internal/cmd/webd`, `cmd/oap/internal/channelcmd`. The operator registers all
five so the Channel controller can validate a spec naming any of them.

## Client-hosted kinds

`Kind.RelayedByChannelsd()` reports false for `local` and `browser`. Their
`Listener`/`Sender`/`StreamDeltaSink` run in the process owning the surface —
`oap` and webd. channelsd still registers them, so it recognises and skips them
rather than erroring on an unknown kind.
