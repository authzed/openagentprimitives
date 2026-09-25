# `pkg/authz/validator`

The shared outcome surface that the repo's two tool-argument validators return.
This directory holds no code of its own.

| Package | What it does |
| ------- | ------------ |
| [`core`](core/) | The `Decision` / `Trace` / `CheckRef` types and their helpers, generic over the per-validator "parsed invocation" type. |

## Why it exists

Two validators evaluate per-phase rules against a tool invocation:

- `pkg/tools/toolspec/validator` — CLI argv, parsed to `*ParsedCall`.
- `pkg/tools/mcp/validator` — MCP JSON arguments, parsed to `*ParsedArgs`.

They agreed on the same outcome surface — `Allow` + `Reason` + `FailedOn` +
`Trace` + `Warnings` + `Redactions` — but each declared its own copies of the
types and helpers, so drift was inevitable. Consolidating here lets each
validator focus on its own domain.

`Decision[T]`'s type parameter is that parsed-invocation type, carried on
`Decision.Parsed`, so callers see exactly one parsed-invocation field with a
typed shape rather than an `any`.

**Both validators must keep returning this type.** A validator that adds a
result field locally is the drift this package was created to prevent — add it
here instead.
