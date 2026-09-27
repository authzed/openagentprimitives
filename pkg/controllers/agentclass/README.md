# `agentclass` — the AgentClass reconciler

Reconciles `AgentClass` CRs. Validates the spec shape, confirms every referenced
Secret and ConfigMap exists **without reading their bytes**, and surfaces
`Valid=True/False` with a reason.

| File                       | Role                                                                                                      |
| -------------------------- | --------------------------------------------------------------------------------------------------------- |
| `controller.go`            | `Reconcile` + `SetupWithManager`, and the `+kubebuilder:rbac` markers                                     |
| `grants.go`                | Composes the class's authz requirements into an `AgentSessionGrants` CR for [`../guardian`](../guardian/) |
| `permission_validation.go` | Per-tool permission declarations                                                                          |
| `schema_validation.go`     | Contributed SpiceDB schema fragments                                                                      |
| `skills_validation.go`     | Referenced Skill / ClusterSkill resolution                                                                |

## Non-obvious constraints

- **Existence is checked, contents are not.** A referenced Secret is confirmed
  to exist and to carry the named key; the value bytes are never read here. That
  keeps the operator out of the credential path.
- **An opted-in dependency that is invalid makes the class invalid.** A class
  that references a broken toolspec, MCPServer or skill reports `Valid=False`
  rather than silently dropping the dependency — a session must fail closed, not
  start missing a tool it declared.
- **This package holds 18 of the repo's `+kubebuilder:rbac` markers** — more
  than any other except `agentsession`. Regenerate after touching one; see the
  [group README](../README.md#kubebuilderrbac-markers-here-generate-the-clusterrole).
