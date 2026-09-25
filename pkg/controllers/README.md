# `pkg/controllers` — reconcilers, webhooks, status helpers

Every controller-runtime reconciler the operator registers, the admission
webhooks it serves, and the shared helpers they all use for status and
conditions.

## `+kubebuilder:rbac` markers here generate the ClusterRole

The markers in these packages' `controller.go` files are the **source** of
`config/manager/role.yaml`. The chain is:

```
+kubebuilder:rbac markers here  ──mage gen:api──▶  config/manager/role.yaml
                                ──mage manifests─▶  pkg/platform/manifests/install.yaml
```

**Adding a watched resource without regenerating produces an operator that
crash-loops on `failed to wait for caches to sync`** — controller-runtime starts
an informer for the resource, the list/watch is Forbidden, the cache never
syncs, and `mgr.Start` returns. The failure is silent at build time and fatal at
deploy time. Run `mage gen:api` then `mage manifests` after touching a marker;
`TestInstallYAMLMatchesKustomize` catches the drift in CI.

Note that a `Get` through the operator's client reads _through the cache_ and
therefore starts an informer — so `get` alone is not enough, you need
`list;watch` too. See
[`spiceboxsession/controller.go`](spiceboxsession/controller.go) for a worked
example of exactly this trap.

## Reconcilers

| Package                                                | Primary CRD                                                              |
| ------------------------------------------------------ | ------------------------------------------------------------------------ |
| [`agentclass/`](agentclass/)                           | `AgentClass`                                                             |
| [`agentidentity/`](agentidentity/)                     | `AgentIdentity` (a validity reconciler + a refresh reconciler)           |
| [`agentsession/`](agentsession/)                       | `AgentSession`                                                           |
| [`agentui/`](agentui/)                                 | `AgentUI`                                                                |
| [`artifactrender/`](artifactrender/)                   | `ArtifactRender`                                                         |
| [`channel/`](channel/)                                 | `Channel` (the `Connected` condition is channelsd's, not the operator's) |
| [`clusteridentityprovider/`](clusteridentityprovider/) | `ClusterIdentityProvider` (singleton)                                    |
| [`clusterskill/`](clusterskill/)                       | `ClusterSkill`                                                           |
| [`clusterskillsource/`](clusterskillsource/)           | `ClusterSkillSource`                                                     |
| [`credentialupdaterequest/`](credentialupdaterequest/) | `CredentialUpdateRequest`                                                |
| [`guardian/`](guardian/)                               | `AgentSessionGrants` → the SpiceDB `agentsession` definition             |
| [`mcpserver/`](mcpserver/)                             | `MCPServer`                                                              |
| [`monitoring/`](monitoring/)                           | _Many_ — a declarative table of (CR type, condition) rows                |
| [`settings/`](settings/)                               | `ClusterAgentSettings` and `AgentSettings`                               |
| [`sidecartoolbox/`](sidecartoolbox/)                   | `SidecarToolbox`                                                         |
| [`skill/`](skill/)                                     | `Skill`                                                                  |
| [`skillsource/`](skillsource/)                         | `SkillSource`                                                            |
| [`spiceboxclass/`](spiceboxclass/)                     | `SpiceboxClass`                                                          |
| [`spiceboxsession/`](spiceboxsession/)                 | `SpiceboxSession`                                                        |
| [`spiceboxtoolchain/`](spiceboxtoolchain/)             | `SpiceboxToolchain`                                                      |
| [`spiceboxtoolkit/`](spiceboxtoolkit/)                 | `SpiceboxToolkit`                                                        |
| [`spiceboxtoolspec/`](spiceboxtoolspec/)               | `SpiceboxToolspec`                                                       |
| [`toolcall/`](toolcall/)                               | `ToolCall`                                                               |
| [`useridentity/`](useridentity/)                       | `UserIdentity` (a validity reconciler + a refresh reconciler)            |
| [`workspacesource/`](workspacesource/)                 | `WorkspaceSource`                                                        |

The cluster-scoped skill pair mirrors the namespaced pair; the shared halves
live in [`internal/skillspec/`](internal/skillspec/) and
[`internal/skillpin/`](internal/skillpin/) rather than being copied.

## Shared machinery

| Package                          | What it holds                                                                                                                 |
| -------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| [`agentstatus/`](agentstatus/)   | Multi-writer AgentSession status writes — each writer touches only the fields it changed                                      |
| [`conditions/`](conditions/)     | Condition helpers that always stamp `ObservedGeneration`. Do not call `meta.SetStatusCondition` directly outside this package |
| [`internal/`](internal/)         | The phase-based reconciler skeleton, backoff, and the helper bodies mirror controllers share                                  |
| [`webhooks/`](webhooks/)         | Admission webhooks — the author checks the API server cannot express                                                          |
| [`testenv/`](testenv/)           | Shared envtest harness + the SSA-idempotency CI gates                                                                         |
| [`testfixtures/`](testfixtures/) | `mustCreate` / `eventually` / `hasTrueCondition` helpers shared across controller tests                                       |

## Status writes

Two rules the reconcilers here follow, both from `AGENTS.md`:

- **Observations go in controller-owned `status`, never in a client-applied
  field.** A byte-identical server-side re-apply must be a no-op;
  [`testenv/idempotency/`](testenv/idempotency/) is the CI gate for that.
- **Write only what changed.** On `AgentSession` three actors write status
  concurrently, so use [`agentstatus.WriteOwned`](agentstatus/statuswrite.go)
  rather than a whole-object `Status().Update`.
