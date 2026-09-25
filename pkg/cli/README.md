# pkg/cli

Shared **command-line plumbing and presentation**. Two packages with two
different audiences: `tui` is the terminal design system every `oap` wizard,
list and detail view renders through; `clikit` is the typed flag↔environment
binding the long-running server binaries use at startup.

| Package | Purpose | Consumed by |
| --- | --- | --- |
| [`clikit`](clikit/) | A cobra `PreRunE` that fills flags from the environment through pflag's typed parsers, so a malformed env value fails closed at startup. Plus the canonical names of env vars read by more than one binary. | the six binaries under `internal/cmd/` |
| [`tui`](tui/) | The terminal design system: screens, drivers, theme, chrome, tables, summaries. | [`cmd/oap`](../../cmd/oap/) and the packages that supply wizard screens |

## Boundary

- Presentation shared by more than one command belongs here. A single command's
  flags and `RunE` stay in `cmd/oap/internal/<family>cmd/`.
- Wizard **screens** for a pluggable backend live with that backend — the Slack
  channel kind, the identity setup flows, the IdP kinds each own their own
  questions. `tui` supplies the `Screen` interface, the sequencer and the
  renderers; it never supplies a specific question.
- `clikit` is deliberately not viper: AP's env vars are shared and unprefixed
  across binaries (`NATS_URL` is read by five of them), so each binary declares
  an explicit flag→env-var binding rather than getting an auto-generated
  per-binary prefix.

See the root [`README.md`](../../README.md) and [`AGENTS.md`](../../AGENTS.md).
