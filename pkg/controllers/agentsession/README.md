# `agentsession` — the AgentSession reconciler

The largest reconciler here, and the one that owns a session's whole Kubernetes
footprint. It resolves the `AgentClass` and gates on `Valid=True`; owns the
per-session ServiceAccount, Role, RoleBinding, memory-token Secret and bundle
`SpiceboxSession`s; creates the runner Pod; and tracks runner restarts through
`RunnerReady` / `Failed=RunnerCrashed`. On deletion its finalizer revokes the
memory token and deletes the memory entry.

| Concern                    | Files                                                                                                                                                   |
| -------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Core**                   | `controller.go`, `phase.go`, `sequencer.go`, `statusapply.go`                                                                                           |
| **Pod construction**       | `podspec.go`, `pod_runner_factory.go`, `runner_factory.go`, `rbac.go`, `bundles.go`                                                                     |
| **Workspace**              | `workspace.go`, `workspaceoverlay.go`, `snapshotstore.go`                                                                                               |
| **Sidecars**               | `sidecars.go`, `sidecarpod.go`, [`cosidecar/`](cosidecar/)                                                                                              |
| **Network**                | `netpol.go`, [`netpolrule/`](netpolrule/)                                                                                                               |
| **Identity + credentials** | `owner_resolve.go`, `identitychoice.go`, `effectivemode.go`, `passthrough.go`, `passthrough_credhash.go`, `credential_grants.go`, `credentialupdate.go` |
| **Lifecycle**              | `sleep.go`, `wake.go`, `archive.go`, `expiration.go`, `retry_ttl.go`                                                                                    |
| **Restart / fork**         | `restart.go`, `restart_decide.go`, `restart_pvc.go`, `fork_host.go`                                                                                     |
| **Other**                  | `authzsessionconfig.go`, `skills.go`, `metaagent_membership.go`, `explanation.go`                                                                       |

| Package                      | What it holds                                                                              |
| ---------------------------- | ------------------------------------------------------------------------------------------ |
| [`cosidecar/`](cosidecar/)   | Builds the hardened co-located sidecar container, its startup probe, and its NetworkPolicy |
| [`netpolrule/`](netpolrule/) | The cloud-agnostic cluster-DNS egress rule fragment                                        |

## Non-obvious constraints

- **Status has three concurrent writers.** Use
  [`agentstatus.WriteOwned`](../agentstatus/statuswrite.go) — touch only the
  fields you changed. A whole-object `Status().Update` reverts a field a
  concurrent writer owns.
- **This reconciler mints the audit trust root.** It creates the per-session
  Ed25519 keypair in the session Secret (`audit-signing-key`) and anchors the
  public half on `status.auditPublicKey` / `auditKeyID`. Those status fields are
  pinned against the runner by the
  [admission webhook](../webhooks/agentsession/) — the runner holds `patch` on
  its own AgentSession and Kubernetes RBAC has no field-level granularity.
- **A forked session's memory must be seeded before its runner can write.**
  Append-only kinds reject a content-changing `Put`; `Put` is not
  last-writer-wins. `restart.go` copies the prefix first for that reason.
- **`cosidecar`'s builders contain no per-consumer branches.** Both
  SidecarToolbox (tools, allowlist egress) and the content-guard detector (no
  egress) consume them; the egress policy is a caller argument.
- **`effectiveAllowedHosts` is recorded, not enforced.** `effectiveNetworkMode`
  _is_: `none` → deny-all, `allowlist` → DNS plus coarse TCP 443/80.
  Hostname-level egress needs a DNS-aware policy controller consuming the status
  snapshot — that half is the cluster operator's.
- **32 `+kubebuilder:rbac` markers live in `controller.go`.** Regenerate after
  touching one.
