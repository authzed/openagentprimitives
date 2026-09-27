# `bento` — the scheduled-trigger channel kind

Schedules inbound messages from an embedded
[Bento](https://warpstreamlabs.github.io/bento) `generate` input: cron-style
triggers with **no human user** behind them. Pairs with a sibling `role=output`
Channel that provides the response surface.

| File                | Role                                                                         |
| ------------------- | ---------------------------------------------------------------------------- |
| `kind.go`           | The `channelkinds.Kind` implementation                                       |
| `listener.go`       | Runs the embedded Bento pipeline and delivers each generated message inbound |
| `config.go`         | Spec parsing — the schedule and the Bloblang mapping                         |
| `forward_output.go` | Forwards the agent's reply to the bound output Channel                       |
| `wizard.go`         | `oap channel create --kind bento`                                            |
| `init.go`           | Registry registration                                                        |

## Non-obvious constraints

- **There is no user to attribute a turn to.** The bound Channel must carry an
  `AuthzSubject`, which the pipeline uses verbatim as the session's `started_by`
  subject and for subsequent permission checks. The AgentClass validator
  enforces its presence on a bento Channel.
- **The output binding is validated at apply time.** The Channel controller
  reports an unresolvable binding as `Valid=False` via
  [`../outputbind`](../outputbind/), so a misconfiguration surfaces on
  `kubectl apply` rather than when the cron first fires.
