# `config/webd/` — the browser-UI host

`webd` serves the browser surfaces (the chat transcript data plane, the session
shell, the agent-UI plugin) and hosts the identityd credential-linking, OAuth,
and portal routes in-process — the standalone `identityd` binary is retired.
This directory ships its Deployment, ClusterIP Service, ServiceAccount, and its
RBAC.

## The RBAC split

There is deliberately **no global `namespace:`** in `kustomization.yaml`. Each
Role targets a different namespace and pins its own `metadata.namespace`:

| File                                             | Namespace                    | Grants                                                                                                                                                                                                                                                     |
| ------------------------------------------------ | ---------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `role.yaml` / `rolebinding.yaml`                 | `agentprimitives-identities` | Master-Secret CRUD for the credential-link flow.                                                                                                                                                                                                           |
| `role-system.yaml` / `rolebinding-system.yaml`   | `agentprimitives-system`     | `get` on a named allowlist of Secrets (passthrough link key, Slack OAuth creds, IdP client secrets).                                                                                                                                                       |
| `role-default.yaml` / `rolebinding-default.yaml` | `default`                    | Per-conversation CRUD: create/delete a Channel, creds Secret, and AgentSession for a browser session. Scoped to `default` only — these verbs include Secret create/delete, and a browser-facing pod holding them cluster-wide would be standing privilege. |
| `clusterrole.yaml` / `clusterrolebinding.yaml`   | cluster-wide                 | UserIdentity read/write for passthrough identity, plus `get` on AgentSessions and Channels anywhere — a viewer holding `agentsession#interact` may open a session in any namespace.                                                                        |

Reading a session is cluster-wide; **creating** one is not. Starting a session
in a namespace `role-default.yaml` does not cover fails loudly at the create.

Nothing in this directory is generated.

[← config/](../README.md)
