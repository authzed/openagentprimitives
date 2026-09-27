# `pkg/apis/v1alpha1`

API schema for group `agentprimitives.authzed.com`, version `v1alpha1` — the
only version. 27 kinds register into `SchemeBuilder` from their own `init()`
(see [`register.go`](register.go) and [`doc.go`](doc.go)); everything else in
this package is either a shared sub-type or generated.

## The kinds

| Area          | Kinds                                                                                                        |
| ------------- | ------------------------------------------------------------------------------------------------------------ |
| Agent         | `AgentClass`, `AgentSession`, `AgentUI`, `AgentSettings`, `ClusterAgentSettings`                             |
| Identity      | `AgentIdentity`, `UserIdentity`, `SessionUserIdentity`, `ClusterIdentityProvider`, `CredentialUpdateRequest` |
| Authorization | `AgentSessionGrants`, `SpiceDBBootstrap`                                                                     |
| Tools         | `MCPServer`, `SidecarToolbox`, `SpiceboxToolspec`, `SpiceboxToolkit`, `SpiceboxToolchain`, `ToolCall`        |
| Tool sandbox  | `SpiceboxClass` (template), `SpiceboxSession` (instance)                                                     |
| Skills        | `Skill`, `SkillSource`, `ClusterSkill`, `ClusterSkillSource`                                                 |
| Surfaces      | `Channel`, `ArtifactRender`                                                                                  |
| Workspace     | `WorkspaceSource`                                                                                            |

Each kind lives in `<lowercased-kind>_types.go`. The doc comment above the root
struct carries the `+kubebuilder:` markers that define scope, short name,
printer columns, and the `status` subresource.

## Not a kind — shared and generated files

| File                                                                                                                                                                                                                                                     | What it is                                                                                                                                                                                                                                                |
| -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`zz_generated.deepcopy.go`](zz_generated.deepcopy.go)                                                                                                                                                                                                   | **Generated** by `mage gen:api`. Never hand-edit.                                                                                                                                                                                                         |
| [`conditions.go`](conditions.go)                                                                                                                                                                                                                         | Condition-type and reason string constants, shared by controllers and CLI.                                                                                                                                                                                |
| [`settings_common_types.go`](settings_common_types.go), [`effective_settings_types.go`](effective_settings_types.go)                                                                                                                                     | The tiered-settings shapes. `EffectiveSettings` is the resolved snapshot stamped onto status; it mirrors `pkg/platform/settings.EffectiveSettings` in a CRD-serializable form (`metav1.Duration`, not `time.Duration`) — **the two must stay in sync**.   |
| [`credential_descriptor.go`](credential_descriptor.go), [`persession_secrets.go`](persession_secrets.go), [`configmap_ref.go`](configmap_ref.go)                                                                                                         | Reference shapes reused across identity and tool kinds.                                                                                                                                                                                                   |
| [`pinning_types.go`](pinning_types.go), [`toolguard_types.go`](toolguard_types.go), [`openrouter_routing_types.go`](openrouter_routing_types.go), [`inbound_attachments.go`](inbound_attachments.go)                                                     | Sub-types embedded by several kinds.                                                                                                                                                                                                                      |
| [`agentsession_ownership.go`](agentsession_ownership.go), [`archive.go`](archive.go), [`wake.go`](wake.go)                                                                                                                                               | Small derivation helpers on `AgentSession` — read a fact off the object rather than re-deriving it per caller.                                                                                                                                            |
| [`toolcall_validation.go`](toolcall_validation.go), [`spiceboxclass_validation.go`](spiceboxclass_validation.go), [`spiceboxtoolchain_validation.go`](spiceboxtoolchain_validation.go), [`workspacesource_validation.go`](workspacesource_validation.go) | **Hand-written** validation that CEL markers cannot express (e.g. refusing a `ToolCall` credential source that names a Secret outside its own namespace — a confused-deputy secret-theft vector). Called by controllers/admission, not by controller-gen. |

## Constraints

- **This package imports nothing else from `pkg/`.** Helpers stay leaf-shaped
  (`json.go` wraps `encoding/json`) precisely so no group can be pulled in.
- **Doc comments are user-facing.** They become the `description` in
  `config/crds/*.yaml` and the text `kubectl explain` prints.
- **Any CRD-shaping edit needs `mage gen:api` then `mage manifests`.** See the
  chain in [`../README.md`](../README.md).
- **`agentclass_types.go` is intentionally not gofmt-clean.** gofmt rewrites the
  paired single quotes in its CEL `XValidation` rules into smart quotes and the
  regenerated CRD stops installing. The full explanation is in
  [`../README.md`](../README.md) — read it before reformatting the file.
