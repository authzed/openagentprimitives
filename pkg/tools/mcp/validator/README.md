# validator

Per-tool-call validation of an MCP invocation against an `MCPServer` spec.
Deliberately parallel to
[`pkg/tools/toolspec/validator`](../../toolspec/validator/); the two share their
`Decision` / `Trace` / redaction machinery through
[`pkg/authz/validator/core`](../../../authz/validator/core/).

The pipeline, each phase short-circuiting on the first deny:

```
tool → allowedFields → deny.effects → deny.trust → constraints → allow
```

| File                                       | Holds                                                                                                         |
| ------------------------------------------ | ------------------------------------------------------------------------------------------------------------- |
| [`validator.go`](validator.go)             | `Check` — the pipeline — and `Invocation` (tool name plus JSON args).                                         |
| [`phases.go`](phases.go)                   | One helper per phase. Each returns pass / recorded-failure / internal-error.                                  |
| [`decision.go`](decision.go)               | Aliases re-exporting the shared `core` types, plus the MCP-specific `ParsedArgs`.                             |
| [`trace.go`](trace.go)                     | Trace append, and `finalize`, which redacts every user-visible string before the Decision leaves the package. |
| [`redact.go`](redact.go)                   | Walks `sensitiveFields` and replaces the whole value at each path with a single token.                        |
| [`constrainterror.go`](constrainterror.go) | `ConstraintError` — the typed error for CEL that could not be _evaluated_.                                    |
| [`helpers.go`](helpers.go)                 | Tool lookup and SEP-1913 enum containment.                                                                    |

## Fail-closed rules

- **An empty `allowedFields` is not "allow everything".** It denies any argument
  the caller passes, so a forgotten allowlist cannot silently open the surface.
  An arg-less call has nothing to reject and passes. The only way to permit
  arbitrary keys is the explicit `unconstrainedArgs` opt-out.
- **A declared-sensitive value is replaced outright**, not merely scrubbed as a
  string leaf — otherwise a number, bool or composite would serialize verbatim
  into `Parsed.Args`. A `sensitiveFields` path that fails to parse still fails
  closed, and records a `SensitiveFieldPathMalformed` warning so the cause is
  visible.
- **`Check`'s `error` return is reserved for authoring defects in the spec** —
  CEL that does not compile, does not build, errors, or yields a non-bool. A
  policy denial is a `Decision` with `Allow=false` and a `FailedOn` path;
  `ConstraintError` means the policy could not be consulted at all.

Part of [`pkg/tools/mcp`](../README.md). See the root
[`README.md`](../../../../README.md) and [`AGENTS.md`](../../../../AGENTS.md).
