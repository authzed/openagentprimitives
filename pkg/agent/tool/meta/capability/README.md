# `pkg/agent/tool/meta/capability`

The meta-tool capabilities an AgentClass can grant. A capability answers two
questions: *was I granted?* (class grant, or default-on) and *can I be
satisfied?* (bound channel, backends present in `RunnerEnv`). If both hold, it
contributes its meta tools.

`Assemble` is the single seam `internal/cmd/runner` and the e2e in-process
factory both drive, so the tool list a test sees is the one production builds.

| File | Holds |
| ---- | ----- |
| [`capability.go`](./capability.go) | The `Capability` interface, `Config`, `OfferContext`, `RunnerEnv`, `SkipReason`. |
| [`registry.go`](./registry.go) | `Register`, `Lookup`, `Ordered`. |
| [`assemble.go`](./assemble.go) | `Assemble` — walk the ordered registry, offer each, collect tools and skip reasons. |
| [`active.go`](./active.go) | `ActiveWithConfig` — grant resolution plus per-capability config parse. |
| [`validate.go`](./validate.go) | `ValidateGrant` for admission-time checking of a class's grants. |

One file per capability otherwise. Registered names:

`agent_builder` · `agent_ui_handoff` · `artifacts` · `attachments` ·
`channel_history` · `channel_interaction` · `core` · `credential_update` ·
`external_data` · `fine_grained_info_leakage` · `introspection` ·
`knowledge` · `memory` · `mention_lookup` · `planning` · `read_view` ·
`session_views` · `set_view_params` · `skills` · `subagent_conversation` ·
`subagents` · `thread_history` · `trigger_status` · `update_view` ·
`user_profile` · `workspace`

[`modality_tools.go`](./modality_tools.go) is not a capability but a shared
helper: it collects what every registered [modality](../../../modality/) offers
for the turn. Both `artifacts` and `attachments` use it, and both may be active
at once — `Assemble` dedups by tool name.

## Constraints

- **Grants are opt-in unless `DefaultOn()`.** `core` is the infrastructural
  always-on one (`agent_work_complete`, `new_operation`).
- **Register, don't branch.** A new capability is a new file with an `init()`;
  `Assemble` needs no change.
- **A granted-but-unsatisfiable capability returns a `SkipReason`, never
  silence** — the caller logs it, so "I granted it and got no tool" is
  diagnosable. Tools and a skip are independent: `Assemble` appends whatever
  came back either way, so a capability that withholds ONE of several tools
  returns the rest alongside the reason for the one it held back
  (`channel_interaction` does this with `respond_to_user`).
- **Grant resolution lives in [`pkg/agent/agentcaps`](../../../agentcaps/)**,
  not here: it must stay importable by components that cannot pull in the tool
  graph. This package owns the *contract* (`Name`/`DefaultOn`/`ParseConfig`/
  `Offer`); `agentcaps` owns *is-it-granted*.

## See also

- [`pkg/agent/tool/meta`](../) — the tools themselves.
