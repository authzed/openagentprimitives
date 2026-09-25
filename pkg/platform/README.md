# `pkg/platform`

The cluster and install substrate: everything that decides **what gets stood up,
where, and under whose identity** — cloud kinds and install strategies, the
embedded manifest bundle, human and agent identity, credential acquisition, the
`.oap` agent-container format, workspace sources, artifact storage, the NATS
bus, and the generic session-lifecycle pipeline.

Nothing here reconciles. Packages in this group are libraries the reconcilers in
[`pkg/controllers`](../controllers/) and the binaries in
[`internal/cmd`](../../internal/cmd/) call into.

## What's here

| Package                             | Purpose                                                                                                                                           |
| ----------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`apimage`](apimage/)               | Single source of truth for the project's first-party container images: build-target names, image names, `:dev` tags, Dockerfiles, contexts.       |
| [`artifacts`](artifacts/)           | Artifact versioning shared by the `artifact_*` meta tools, `respond_to_user` attachment resolution, and the `oap artifact` CLI.                   |
| [`artifactstore`](artifactstore/)   | The durable artifact `Store` interface plus its gocloud-backed multi-scheme implementation.                                                       |
| [`capacityfit`](capacityfit/)       | Turns "this class asks for more than any node has" into install-time questions that clamp it.                                                     |
| [`cloud`](cloud/)                   | The cluster-kind registry (`local`, `desktop`, `default`, `gke`, `eks`, `aks`) and the per-cloud install strategies behind it.                    |
| [`deplogs`](deplogs/)               | Silences the process-global loggers our dependencies write to, so library chatter never reaches a terminal or container log.                      |
| [`extract`](extract/)               | Turns an uploaded file into agent-readable text, MIME-registry dispatched.                                                                        |
| [`identity`](identity/)             | Canonical user identity, plus the whole credential subtree: auth kinds, the token broker, IdP login, setup flows, passthrough links.              |
| [`identityd`](identityd/)           | The browser-facing identity server: OIDC login, credential link + portal pages, the CLI login exchange.                                           |
| [`kube`](kube/)                     | Small Kubernetes client-config helpers shared by the server binaries.                                                                             |
| [`kubeyaml`](kubeyaml/)             | Splits a multi-document Kubernetes YAML/JSON stream into unstructured objects. A leaf, so consumers get the splitter without the manifest bundle. |
| [`manifests`](manifests/)           | The embedded install bundle and the component manifests. **Generated** — see its README.                                                          |
| [`nats`](nats/)                     | NATS decentralized-JWT identity, per-client user-JWT minting, TLS material, the connection helper, and the subject grammar.                       |
| [`oap`](oap/)                       | The `.oap` agent-container format: pack/unpack, questions and binding overlays, install gating, registry push/pull.                               |
| [`pipeline`](pipeline/)             | The generic session-lifecycle interceptor framework (points, hooks, decisions, executor). Imports no domain packages.                             |
| [`podspec`](podspec/)               | Builds the sandboxed `corev1.Pod` from a session plus its resolved class spec.                                                                    |
| [`podstatus`](podstatus/)           | Controller-agnostic readers over a Pod's runtime status, so every controller classifies "stuck" the same way.                                     |
| [`schedfit`](schedfit/)             | Answers one question about a cluster: can it _ever_ schedule a pod that requests this much?                                                       |
| [`settings`](settings/)             | Folds the four-tier settings chain (cluster → namespace → class → session) into one `EffectiveSettings` plus `Violations`. Pure.                  |
| [`settingswiring`](settingswiring/) | The cluster-side half of `settings`: loads the tiers, resolves class refs, stamps the `SettingsAccepted` condition.                               |
| [`startup`](startup/)               | Bounded-retry primitives for startup steps that wait on another component becoming reachable.                                                     |
| [`workspace`](workspace/)           | Snapshot/restore of AgentSession workspace PVC contents, and the overlay-cut/reconcile Jobs.                                                      |
| [`workspacekinds`](workspacekinds/) | The pluggable workspace-source driver surface and its registry (`git` today).                                                                     |

## Boundary

- **A reconciler does not live here.** Watches, status writes, and condition
  helpers are [`pkg/controllers`](../controllers/). Platform packages are pure
  libraries or client-side helpers those reconcilers call.
- **Tool definition and execution do not live here.** Toolspec, toolchains, MCP,
  skills, and sandbox backends are [`pkg/tools`](../tools/) — including
  `sandboxkinds`, which is _not_ a sibling of `workspacekinds`.
- **HTTP surfaces mostly do not live here.** [`pkg/web`](../web/) owns what is
  served. `identityd` is the exception: it belongs to the identity concern, and
  registers itself as a `webui.WebUI` that `webd` mounts.
- **`cloud` is install-shape, not runtime config.** A cluster kind decides what
  `oap install` stands up; per-agent runtime policy is `settings`.

## See also

- Repo [`README.md`](../../README.md)
- [`AGENTS.md` → Where code lives](../../AGENTS.md#where-code-lives)
