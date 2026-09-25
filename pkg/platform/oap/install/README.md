# `pkg/platform/oap/install`

Everything between "I have a [`Bundle`](../)" and "the cluster has it": the
fail-closed checks that run **before any write**, the question/answer resolution
that produces the values to overlay, the server-side apply itself, and the
uninstall walk back out.

## Files

| File | What it does |
| ---- | ------------ |
| `preflight.go` | `Preflight` — structural validation, the `Compat.MinApVersion` floor against the cluster's version, and `Requires`/`Questions` coherence. Returns on the first failure. |
| `questions.go` | Answer resolution: prompts, `--set` values, CEL-evaluated conditions and defaults, and quantity coercion. |
| `conflict.go` | `Conflict` — reports every pre-existing object this install would seize, as **data**, so one run names all collisions instead of erroring on the first. |
| `clusterdeps.go` | Ensures shared cluster-scoped dependencies, adopting a pre-existing one through `pkg/tools/adoptkit` with an `OapInstall` ownership annotation. |
| `apply.go` | The SSA writes: bundled CRs, Secrets materialized from secret questions, and the adoption patches. |
| `uninstall.go` | Walks the managed kinds — cluster-scoped listed cluster-wide, namespaced scoped to a namespace — and removes what this install created. |

## Constraints

- **Nothing writes until preflight passes.** That ordering is the package's
  reason to exist; do not add a "convenient" early apply.
- **`FieldManager` is a single constant** (`ap-agent-install`) shared by every
  write this package makes. Server-side apply ownership depends on it staying
  stable.
- **Adopting a Secret is categorically different from adopting a CR.** A bundled
  CR's spec *converges* under re-apply; adopting a Secret **overwrites its
  data**. That is why `Conflict.Secret` exists and why a blanket adopt never
  covers one — only naming it explicitly does.
- **Applied fields must be a pure function of the bundle plus the answers.** No
  wall-clock, no randomness: a re-install of the same bundle must be a
  byte-identical SSA no-op. Observations belong in controller-owned `status`.
- **Uninstall selects by `instance.LabelInstall`**, not by name pattern — see
  [`../instance`](../instance/).
- Both `preflight.go` and `questions.go` carry a `// Package install …` comment;
  they disagree slightly in wording. Treat `preflight.go`'s as canonical.
