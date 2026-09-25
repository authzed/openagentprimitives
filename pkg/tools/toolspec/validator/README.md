# validator

Runs one proposed CLI invocation through the toolspec pipeline and returns a
`Decision`. Shares its `Decision` / `Trace` / redaction machinery with
[`pkg/tools/mcp/validator`](../../mcp/validator/) through
[`pkg/authz/validator/core`](../../../authz/validator/core/); the two are
deliberately parallel.

| File                                               | Holds                                                                                                                                                                                          |
| -------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`validator.go`](validator.go)                     | `Check`, and `Invocation` (command, argv, env, cwd, binary version).                                                                                                                           |
| [`phases.go`](phases.go)                           | One helper per phase. Each returns pass / recorded-failure / internal-error.                                                                                                                   |
| [`rules.go`](rules.go)                             | Evaluation of the structured `deny.effects.*` and `allow.*` rules against the resolved effects, returning the rule paths that would deny — the caller decides whether an exception lifts them. |
| [`decision.go`](decision.go)                       | Aliases re-exporting the shared `core` types, plus the CLI-specific `ParsedCall`.                                                                                                              |
| [`trace.go`](trace.go), [`helpers.go`](helpers.go) | Trace append, finalize/redaction, and small lookups.                                                                                                                                           |

## Constraints

- **The `error` return is reserved for authoring bugs** — an unknown parser,
  malformed CEL, a malformed version range in the toolkit or spec. A policy
  denial is a `Decision` with `Allow=false` and a `FailedOn` path.
- **`checkRevision` refuses a spec authored against a different
  `toolkitRevision`** outright, rather than silently revalidating it against the
  current one.
- Every user-visible string on the returned Decision has been through the
  redactor before it leaves; `Decision.Redactions` maps each emitted token back
  to a descriptor.
- `Decision.Parsed` is populated whenever the parse step succeeded, even when a
  later phase denied — so a deny still shows what the pipeline acted on.

Part of [`pkg/tools/toolspec`](../README.md). See the root
[`README.md`](../../../../README.md) and [`AGENTS.md`](../../../../AGENTS.md).
