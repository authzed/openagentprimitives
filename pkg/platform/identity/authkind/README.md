# `pkg/platform/identity/authkind`

The per-prefix plug-in surface for AgentIdentity **setup** routing. An
AgentIdentity binding match is `<prefix>:<name>`; each prefix is handled by
exactly one `Kind` registered at `init()`. A `Kind` resolves the suffix to a
`Target` and reports the `CredentialRequirement`s that target needs — nothing
more.

This package covers setup only. Resolving a credential at runtime is the token
broker's job ([`../broker`](../broker/)).

## Subpackages

| Package                             | Prefix     | What it resolves                                                                                            |
| ----------------------------------- | ---------- | ----------------------------------------------------------------------------------------------------------- |
| [`cli`](cli/)                       | `cli`      | A CLI toolkit.                                                                                              |
| [`mcp`](mcp/)                       | `mcp`      | An `MCPServer` CR.                                                                                          |
| [`toolspec`](toolspec/)             | `toolspec` | A `SpiceboxToolspec` CR.                                                                                    |
| [`sidecartoolbox`](sidecartoolbox/) | `toolbox`  | A `SidecarToolbox` CR. Note the package name and the prefix differ.                                         |
| [`registry`](registry/)             | —          | The process-wide registry plus `ParseBindingMatch`. Storage/mutex/dup-panic come from `pkg/x/kindregistry`. |
| [`loader`](loader/)                 | —          | Blank-imports every kind so a binary registers all of them with one import. **New kinds are added here.**   |

## Constraints

- **Register from `init()`, resolve by prefix.** `registry.Register` panics on a
  duplicate prefix. Never branch on the prefix outside a `Kind`.
- **`ResolveTarget` must return `ErrTargetNotFound`** when nothing by that
  suffix exists; callers translate it into a setup error or a `Failed=True`
  condition. Other errors mean transport/lookup failure and are distinguished.
- **`SetupRequirements` is a pure function of the target** — no I/O — so setup
  can be idempotent per requirement.
- `Target.BindingMatchString()` is the inverse of `registry.ParseBindingMatch`;
  keep the pair in sync rather than formatting the string at a call site.
