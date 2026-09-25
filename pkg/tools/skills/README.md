# skills

Everything about a Skill's **identity and content**, for the agentskills.io
`SKILL.md` format. The reconcilers and the admission webhook live in
[`pkg/controllers`](../../controllers/); this is the library they call, so one
rule set serves both.

| Package                       | Purpose                                                                                                                                                                          |
| ----------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`canonical`](canonical/)     | Parses and manipulates canonical skill names — the stable, collision-free identity derived from git provenance, or from the reserved `local` authority for hand-authored skills. |
| [`skillfetch`](skillfetch/)   | Clones a repo at a ref and returns its files plus the resolved commit SHA. `GoGit` does it entirely in memory (no on-disk checkout); `Fake` keeps the reconciler unit-testable.  |
| [`skillmd`](skillmd/)         | Parses a `SKILL.md`: a YAML frontmatter block fenced by `---` lines, then the Markdown body.                                                                                     |
| [`validate`](validate/)       | The frontmatter and body rule set — agentskills.io constraints plus this project's forward-compat rules. Returns _every_ problem found, so the webhook can report them together. |
| [`materialize`](materialize/) | Answers whether a non-local canonical name is actually backed by the `SkillSource` that would produce it.                                                                        |

## Constraints

- **Canonical name shape** is `<repo-locator>//<subpath>[@<ref>]` for
  git-sourced skills, `local//<name>` or `local/<namespace>//<name>` for
  hand-authored ones. The `//` is a literal Terraform-style separator, and the
  `@<ref>` is _part of the identity_ — two different refs are two different
  skills.
- **`materialize` exists because `spec.Source` is user-writable.** A tenant with
  `skills:create` could otherwise set it to anything and claim a trusted git
  authority for attacker-controlled body text, which an org's name-pattern
  allowlist would then trust. The signal used instead cannot be forged by a
  same-namespace tenant: a controller owner-ref of the right Kind, to a source
  that actually exists, whose normalized `RepoURL` equals the canonical name's
  authority. **All three must hold.** Do not substitute a `Source != nil` check.
- `validate` rejects XML tags and the reserved provider words in frontmatter,
  and cross-checks the frontmatter `name` against the canonical directory.

Part of [`pkg/tools`](../README.md). See the root
[`README.md`](../../../README.md) and [`AGENTS.md`](../../../AGENTS.md).
