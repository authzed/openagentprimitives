# cel

Wraps `cel-go` with the **two fixed environments** this repo evaluates tool
policy in, plus a registry of the helper functions available to them.

| Environment | Variable                                                                     | Used by                        |
| ----------- | ---------------------------------------------------------------------------- | ------------------------------ |
| `Env()`     | `call` — a map shaped by [`toolspec/parser.Call`](../toolspec/parser/)       | `SpiceboxToolspec` constraints |
| `MCPEnv()`  | `args` — a `google.protobuf.Struct` matching the JSON handed to `tools/call` | `MCPServer` arg constraints    |

| File                                   | Holds                                                                                                                                          |
| -------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| [`registry.go`](registry.go)           | The `Helper` type and the process-level registry, keyed by a `Scope` bitmask (`ScopeToolspec`, `ScopeMCP`, or both).                           |
| [`env.go`](env.go), [`mcp.go`](mcp.go) | The two environment constructors.                                                                                                              |
| [`helpers.go`](helpers.go)             | The built-in helpers: `path.isUnder`, `host.of`, `host.matches`, `glob.match`, `semver.satisfies`, `call.flag`, `call.hasFlag`, `call.hasEnv`. |
| [`time.go`](time.go)                   | `now()`, registered in both scopes.                                                                                                            |
| [`compile.go`](compile.go)             | `Compile` and `EvalBool`. Compile once at spec load; reuse the `Program` per invocation.                                                       |

## Constraints

- **A new helper is a `Register(Helper{…})` call from an `init()`** — never an
  inline `cel.EnvOption` at a call site. Both environment constructors and the
  LLM authoring prompts read the same registry through `HelpersFor` /
  `HelperDocs`, so what the model is told exists and what actually type-checks
  cannot drift.
- The same registry serves compile-time type checking
  ([`mcp/spec.Compile`](../mcp/spec/),
  [`toolspec/validator`](../toolspec/validator/)) and runtime evaluation, so a
  constraint that compiled will evaluate.
- CEL's `duration()` does not accept `d`; day-scale intents are expressed in
  hours (`168h` = 7 days). The `now()` helper doc says so, and that doc is what
  reaches the authoring model.

Part of [`pkg/tools`](../README.md). See the root
[`README.md`](../../../README.md) and [`AGENTS.md`](../../../AGENTS.md).
