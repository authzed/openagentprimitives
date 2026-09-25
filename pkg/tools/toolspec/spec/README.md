# spec

The **spec** half of the toolspec pair: a capability contract authored against
one toolkit revision. (The other half is [`toolkit`](../toolkit/), the
closed-world description of the CLI itself.)

The stanzas, in evaluation order: `Require` preconditions; `AllowSubcommands`,
the closed set of permitted subcommand paths — **empty allows nothing**; `Deny`
rules, which run first and short-circuit; `Allow`, which bounds what an otherwise
permitted call may reach; `Exceptions`, which relax a *named* deny rule behind a
CEL guard; `Constraints`, CEL predicates every allowed call must satisfy; and
`Sensitive`, naming the inputs whose values are redacted from every user-visible
output.

| File | Holds |
| --- | --- |
| [`types.go`](types.go) | `Spec` and its stanzas. |
| [`load.go`](load.go) | `Load` / `LoadBytes` plus structural validation, and the two exception-path sets. |
| [`lint.go`](lint.go) | Non-fatal authoring warnings — today, a constraint that has CEL but an empty `message`. |
| [`paths.go`](paths.go) | `PathKey`, the dotted rule path shared by `Generation.Descriptions`, `exceptions[].overrides`, and `Decision.FailedOn.Path`. |

## Constraints

- **The two path sets in `load.go` are the load-bearing invariant.**
  `OverridableRulePaths` is the closed set an exception may name;
  `NonOverridablePaths` — `parse`, `revision`, `binaryVersion`,
  `allowSubcommands` — is what an exception may never target. Load enforces both,
  so an exception cannot widen the allowlist or make a spec revalidate against a
  different toolkit revision.
- A constraint's `message` is what `introspect_tool` surfaces to the agent as the
  justification of a CEL gate. `LintConstraintMessages` nudges authors to fill it;
  it is a lint, not a validation error.

Part of [`pkg/tools/toolspec`](../README.md). See the root
[`README.md`](../../../../README.md) and [`AGENTS.md`](../../../../AGENTS.md).
