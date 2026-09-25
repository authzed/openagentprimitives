# toolspec

The **CLI-invocation validator**: given a declarative description of a CLI (a
*toolkit*) and a capability contract authored against it (a *spec*), decide
whether a proposed argv may run, and say why. This is what backs the
`SpiceboxToolspec` CR.

| Package | Purpose |
| --- | --- |
| [`toolkit`](toolkit/) | The closed-world description of one CLI: subcommands, flags, positionals, env allowlist, declared effects, version probe. |
| [`spec`](spec/) | The capability contract authored against one toolkit revision: allow/deny/exceptions/constraints/sensitive. |
| [`parser`](parser/) | Argv → `Call`, the structured form rules and CEL read. Also the (empty) named-parser registry. |
| [`effect`](effect/) | A subcommand's declared side effects, with template strings resolved against the parsed `Call`. |
| [`validator`](validator/) | The pipeline: parse → structured rules → CEL → `Decision`. |
| [`render`](render/) | Plain-language description of a spec. Pure; no network, no LLM. |
| [`llm`](llm/) | The provider interface behind LLM-assisted spec authoring, and its implementations. |
| [`registry`](registry/) | Toolkit reference resolution for the operator: compile-time built-ins first, then `SpiceboxToolkit` CRs. |

The built-in toolkit YAMLs live at the repo root in
[`toolkits/`](../../../toolkits/) and are embedded into the binaries. A
`SpiceboxToolkit` CR extends the set with new `(name, revision)` pairs;
collisions with a built-in are rejected at admission.

## Library usage

```go
import (
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/validator"
)

tk, _ := toolkit.Load("toolkits/gh.yaml")
sp, _ := spec.Load("pkg/tools/toolspec/validator/testdata/gh-readonly-spec.yaml")

d, err := validator.Check(tk, sp, validator.Invocation{
	Command:       "gh",
	Argv:          []string{"pr", "view", "123", "-R", "authzed/openagentprimitives"},
	BinaryVersion: "2.53.1",
})
// d.Allow, d.Reason, d.FailedOn, d.Trace, d.Warnings, d.Redactions
```

## CLI usage

There is no standalone `toolspec` binary; the verbs are part of `oap`. The
kind-agnostic file verbs work on any tool kind, detecting it from the document:

```sh
oap tools validate -f specs/gh-readonly.yaml   # validate a spec file
oap tools lint     -f specs/gh-readonly.yaml   # offline lint
oap tools probe    -f servers/linear.yaml      # probe a live backend
```

The sandbox-toolspec-specific helpers stay under `oap tools toolspec`:

```sh
# human-readable trace of one proposed invocation
oap tools toolspec explain --toolkit toolkits/gh.yaml --spec specs/gh-readonly.yaml \
  --binary-version 2.53.1 -- gh pr view 123 -R authzed/openagentprimitives

# schema + CEL compile check, optionally cross-checked against its toolkit
oap tools toolspec check specs/gh-readonly.yaml --toolkit toolkits/gh.yaml

# render the spec in plain language (--format text|markdown|json)
oap tools toolspec describe specs/gh-readonly.yaml

# replay the spec's persisted regression corpus through the validator
oap tools toolspec test specs/gh-readonly.yaml
```

`--toolkits` (or `TOOLSPEC_TOOLKITS`) points the catalog at a directory of
toolkit YAMLs; it defaults to `./toolkits`.

## Trust model

- **Toolkit YAML is data, never code.** The only code path out of it is
  `parser: {kind: builtin, name: <registered>}`, and registration is in-tree only
  — see [`parser/builtin`](parser/builtin/), which today has **no parsers
  registered at all**. Everything in production parses declaratively.
- The parser is **closed-world**: unknown subcommands and unknown flags are parse
  errors, not pass-throughs.
- Sensitive flag, env and positional values (declared in the toolkit or the spec)
  are replaced with `<redacted id="N"/>` tokens in every user-visible string on a
  `Decision`; `Decision.Redactions` carries the descriptors.
- An exception may relax only a rule path in `spec.OverridableRulePaths`. It can
  never target `parse`, `revision`, `binaryVersion` or `allowSubcommands`.

## LLM-assisted authoring

`oap tools gen` opens the agent builder in your browser — an OAP agent (the
model itself, under `internal/cmd/workshop`) that walks you from an idea to a
working, tested spec, replacing the old local authoring loop that drove the
[`llm.Provider`](llm/) interface directly.

```sh
oap tools gen
```

Only `gen` needs network and credentials (to open the browser session).
`validate`, `lint`, `explain`, `check`, `describe` and `test` are offline.

Part of [`pkg/tools`](../README.md). See the root
[`README.md`](../../../README.md) and [`AGENTS.md`](../../../AGENTS.md).
