# parser

Turns an invocation's argv into a `Call` — the structured form the validator's
structured rules read, and the top-level CEL variable (`call.subcommand`,
`call.flags`, `call.positional`, `call.tail`).

| File                               | Holds                                                                                                                                                             |
| ---------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`parser.go`](parser.go)           | The `Parser` interface and the `Call` type.                                                                                                                       |
| [`declarative.go`](declarative.go) | `Declarative` — the single generic argv walker, driven entirely by the toolkit schema.                                                                            |
| [`flags.go`](flags.go)             | Flag indexing, value assignment, type coercion.                                                                                                                   |
| [`errors.go`](errors.go)           | `ErrParse` plus the typed `ParseError` kinds: `UnknownSubcommand`, `UnknownFlag`, `ArgCountMismatch`, `FlagTypeMismatch`, `MissingFlagValue`, `EnumValueInvalid`. |
| [`builtin/`](builtin/)             | The named-parser registry — see below.                                                                                                                            |

## Constraints

- **Closed world.** An unknown subcommand or an unknown flag is a parse error,
  never a pass-through.
- `Declarative` consumes a leading prefix of recognized _global_ flags before
  matching the subcommand, because tools like `git` require them there.
- **A global flag must not clobber a same-named subcommand flag.**
  `git -c key=value` and `git switch -c branch` mean different things; letting
  the global win made a plain branch creation parse as a config override, which
  a spec scoping `-c` to committer identity then rejected. `flags.go` keeps the
  subcommand's meaning in the post-subcommand region.

## `builtin/` is a declared seam with nothing registered

`toolkit.parser.kind` may be `"declarative"` or `"builtin"`. The `builtin`
registry exists and works — `Register` panics on a duplicate, `Get` returns
`(nil, false)` on a miss, and [`validator.Check`](../validator/) resolves
through it — but **nothing registers a parser into it**, anywhere in this tree
or in any binary. Every toolkit in production use is `declarative`; a toolkit
naming `kind: builtin` is refused with `builtin parser %q not registered`.

That is the intended state, not an oversight. Registration is deliberately
in-tree only: it is the single code-execution escape hatch from toolkit YAML,
which is otherwise pure data, so it stays empty until a CLI genuinely cannot be
described declaratively.

Part of [`pkg/tools/toolspec`](../README.md). See the root
[`README.md`](../../../../README.md) and [`AGENTS.md`](../../../../AGENTS.md).
