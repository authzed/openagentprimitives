# `channelsd` — the inbound listener + outbound relay daemon's libraries

`channelsd` is the daemon that hosts every *relayed* channel kind: it runs each
bound Channel's listener, feeds inbound messages through a shared pipeline, and
relays the agent's outbound envelopes back to the right kind's `Sender`.

The **binary** lives at `internal/cmd/channelsd` — it owns `main`, the
per-session maps, the tick loops and the Kubernetes clients. This directory
holds the parts that are importable and unit-testable without it.

| Package | What it holds |
| ------- | ------------- |
| [`pipeline/`](pipeline/) | The shared inbound flow: session correlation, authz, session creation, and every prompt/decision leg |
| [`pipelinehost/`](pipelinehost/) | channelsd's implementation of `pipeline.Host` — the hook-point adapter |
| [`outbound/`](outbound/) | The `ap.session.>.out.>` relay: subscribe, resolve the kind, dispatch to its `Sender` |
| [`watchdog/`](watchdog/) | The pure decision core of the per-session "is the agent silently stuck?" watchdog |
| [`historyresp/`](historyresp/) | Serves the `read_thread_history` runner tool over a NATS request/reply subject |
| [`explainer/`](explainer/) | A second, isolated LLM call producing a `{what, why}` pair for a credential prompt |
| `e2e/` | End-to-end tests only — no non-test Go files |

## Non-obvious constraints

- **The outbound relay routes off the NATS subject, not the envelope body.**
  The session it looks up comes from the subject a publisher's per-session JWT
  authorized. See `outbound/relay.go`'s `handle`.
- **`watchdog` holds no I/O and no Kubernetes types.** `Decide()` is a pure
  function of `(state, now, config)`, so every behavioural rule is
  table-testable in one place instead of smeared across fire-time re-checks.
  The binary owns the map and the timer and feeds it signals.
- **`historyresp` exists because the runner has no channel credentials.** The
  tool round-trips through channelsd, which resolves the bound kind and calls
  its `ConversationReader`.
- **`pipelinehost` fail-closes approvals.** channelsd fires the `InboundTurn`
  point with the cheap SpiceDB `#interact` gate, which only ever returns
  Allow/Deny and never produces an `ApprovalAsk` — so its approval methods
  return `ErrApprovalUnsupported` and the executor turns that publish error into
  a Halt. The deny side effects live in the pipeline, not here.
