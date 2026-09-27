# pkg/gen

Code and documentation generators driven by magefiles. `auditgen` assembles a
prompt and hands it to the headless `claude` CLI through the shared `claudeexec`
plumbing to write its artifact; the side-effecting dependencies — running
`claude`, running `git diff` — are injected, so the deterministic half (the lens
list, prompt assembly, run orchestration) is unit-tested without a `claude`
process. `clidocs` and `crddocs` are plain deterministic generators — no LLM
involved — that walk the live cobra tree and the CRD schemas.

| Package                     | Purpose                                                                                                                              |
| --------------------------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| [`auditgen`](auditgen/)     | The audit pass behind `mage audit:*`: the canonical lens list, the prompt, and the run orchestration.                                |
| [`claudeexec`](claudeexec/) | The shared headless-`claude` invocation contract — argv, exec, and the `RunFunc`/`DiffFunc` seams a claude-driven generator injects. |
| [`clidocs`](clidocs/)       | `mage docs:cli`: regenerates the showcase docs' CLI reference from the live cobra tree.                                              |
| [`crddocs`](crddocs/)       | `mage docs:crd`: regenerates the showcase docs' CRD reference from `config/crds`.                                                    |

## Boundary

- A generator that shells out to `claude` to (re)write a repo artifact belongs
  here; its mage target belongs in `magefiles/`.
- A generator only writes files — none makes a git write, and none is part of
  the ship gate.
- `auditgen.Lenses` is a single source of truth, rendered into the audit prompt
  _and_ into the corresponding prose in [`AGENTS.md`](../../AGENTS.md). A new
  lens is a row in the slice, never a transcription.
- `PRIMITIVES.md` (repo root) and `docs/owasp-agentic-top10-coverage.html` are
  both hand-maintained now; the claude-driven generator that used to write them
  (`docsgen`) was removed as internal-only tooling not needed by the published
  project. Update them by hand when the code they describe moves.
- Not to be confused with the agent builder (`oap tools gen`,
  `internal/cmd/workshop`), which is LLM-assisted authoring of _tool specs_, or
  with the `render` packages under `pkg/tools`, which are pure formatters.

See the root [`README.md`](../../README.md) and [`AGENTS.md`](../../AGENTS.md).
