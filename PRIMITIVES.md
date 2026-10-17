# AgentPrimitives

This file is the canonical index of the six **AgentPrimitives** — the conceptual
building blocks this project is organized around — and a map of where each one's
code lives in the tree.

The primitives, in order:

1. [Agent definition](#agent-definition)
2. [Safe tools](#safe-tools)
3. [Identity & credentials](#identity--credentials)
4. [Authorization](#authorization)
5. [Channels & continuity](#channels--continuity)
6. [Memory & knowledge](#memory--knowledge)

---

## Agent definition

An agent is a reviewable template (**AgentClass**) and a running instance
(**AgentSession**).

- **CRDs:** `pkg/apis/v1alpha1/agentclass_types.go`,
  `pkg/apis/v1alpha1/agentsession_types.go`
- **Controllers:** `pkg/controllers/agentclass/`,
  `pkg/controllers/agentsession/`
- **Agent runtime:** `pkg/agent/` — `runner/` (turn loop, budget, approvals,
  descriptors, dispatch pipeline, `approval/`, `identityadvisor/`), `session/`
  (`lifecycle/`, `state/` + its registry), `llm/` (the `Provider` interface +
  `providers/` registry: anthropic, openai, openrouter), `harness/` (`Harness`
  interface + `registry`, with `apnative/` and `fake/`), `tool/` (`sandbox/`,
  `mcp/`, `sidecartoolbox/`, `synthesize/`, `operations/`, `meta/`,
  `authfail/`), `modality/`, `preview/`, `secretout/`
- **Skills (reusable capability bundles):** CRDs `skill_types.go`,
  `skillsource_types.go`, `clusterskill_types.go`,
  `clusterskillsource_types.go`; controllers
  `pkg/controllers/{skill,skillsource,clusterskill,clusterskillsource}/`;
  packages `pkg/tools/skills/` (`canonical/`, `skillfetch/`, `skillmd/`,
  `validate/`, `materialize/`) and `pkg/tools/skillbundle/`
- **Governance settings & pinning:** CRDs `agentsettings_types.go`,
  `clusteragentsettings_types.go`, `effective_settings_types.go`,
  `settings_common_types.go`, `pinning_types.go`, `openrouter_routing_types.go`;
  controller `pkg/controllers/settings/`; packages `pkg/platform/settings/`
  (tiered resolve), `pkg/platform/settingswiring/`, `pkg/authz/pinning/`
  (version pinning + `kinds/` registry, incl. `oap/`)
- **Agent container format:** `pkg/platform/oap/` (the `.oap` single-file,
  OCI-native bundle), `test/oaptest/` (shared test fixtures),
  `pkg/platform/apimage/`
- **Workspaces (per-session working trees):** CRD `workspacesource_types.go`;
  controller `pkg/controllers/workspacesource/`; packages
  `pkg/platform/workspacekinds/` (`Kind` + `registry`, backend `git/`) and
  `pkg/platform/workspace/` (snapshotter, overlays, reconcile job)
- **Pod shape & capacity:** `pkg/platform/podspec/`, `pkg/agent/agentcaps/`,
  `pkg/controllers/agentstatus/`, `pkg/platform/capacityfit/`,
  `pkg/platform/schedfit/`, `pkg/agent/postsession/`
- **Binary:** `internal/cmd/runner/` (the per-session runner process); leakage,
  history- and profile-gate wiring in `pkg/agent/runner/` (`leakagewiring/`,
  `channelhistorygate/`, `userprofilegate/`)

## Safe tools

An agent can only do what it was given permission to do: sandboxed CLI tools and
MCP servers, validated against a toolspec.

- **CRDs:** `pkg/apis/v1alpha1/spiceboxtoolspec_types.go`, `mcpserver_types.go`,
  `sidecartoolbox_types.go`, `spiceboxtoolkit_types.go`,
  `spiceboxtoolchain_types.go`, `toolcall_types.go`, `toolguard_types.go`; the
  sandbox substrate itself is `spiceboxclass_types.go` (pod template: image,
  resources, network, mounts, tools/toolspecs/toolchains) and
  `spiceboxsession_types.go`
- **Controllers:**
  `pkg/controllers/{spiceboxtoolspec,spiceboxtoolkit,spiceboxtoolchain,mcpserver,sidecartoolbox,toolcall}/`,
  plus `pkg/controllers/{spiceboxclass,spiceboxsession}/` for the sandbox pods
- **Tool kinds (CLI presentation):** `pkg/tools/` — `contract/` (the `Kind`
  interface) + `kinds/{sandbox,mcp,sidecartoolbox}/` registered via
  `kinds/registry/`; plus `catalog/`, `cel/`, `redact/`, `websearch/` (the
  `Provider` seam: anthropic, fake), `sandboxkinds/` (the `Kind` interface +
  `registry/` for sandbox backends: pod, agent-sandbox)
- **Toolspec validation/parsing:** `pkg/tools/toolspec/` — `parser/` +
  `registry/`, `spec/`, `validator/`, `effect/`, `toolkit/`, `render/`, `llm/`
- **Tool guard (per-tool policy tiers):** `pkg/authz/toolguard/`
- **Toolchains & streaming:** `pkg/tools/toolchain/` (the resolved-toolchain
  seam), `pkg/tools/toolkitstream/` (per-toolkit stream-parser plug-ins),
  `pkg/web/gateway/` (streaming gRPC service for ToolCall exec)
- **Execution & MCP plumbing:** `pkg/tools/exec/` (sandboxed CLI execution, with
  `remote/` and `fake/`), `pkg/tools/mcp/` (`spec/`, `oauth/`, `probe/`,
  `render/`, `trust/`, `validator/`)
- **Browser toolbox:** `pkg/apis/v1alpha1/browsertoolbox_types.go` (a
  per-session browser: site allowlist/blocklist, act/readonly interaction,
  ephemeral state); `pkg/agent/tool/browser/` synthesizes the observation tools
  (navigate/read/find/follow/scroll/back/wait_for, +screenshot) over one
  chromedp session; `pkg/tools/browsergate/` is the per-session gate process
  (authenticated DevTools + policy-enforced egress) the sandbox pod runs;
  `pkg/tools/browserpolicy/` is the shared site-matching decision both the gate
  and the tool's own pre-check consult. No dedicated controller yet (Phase A) —
  identity-scoped sites arrived in Phase B, acting tools in Phase C (below).
  Phase B adds identity-bound login catalogs: the `directCredentials` credential
  type (`pkg/platform/identity/directcred/`, sources `onepassword` and `secret`)
  lets a BrowserToolbox bind an AgentIdentity and list its logins as
  `{id, label, hosts}` onto `AgentSession.status` — the username/password/OTP
  itself never reaches the model, the broker (`Resolve` errors unconditionally
  for this type), or status. Phase C adds credentialed, acting browsing. Acting
  tools (`web_click`/`web_type`/`web_press`/`web_select`/`web_submit`) are
  synthesized only for an `interaction: act` toolbox
  (`pkg/agent/tool/browser/synthesize.go`), each gated External on a per-session
  `web_host` SpiceDB slot's `act` permission (`pkg/tools/browserschema/`) — so
  every acting call on a host that lacks a grant routes to a human plan-gate
  approval card, and one approval covers the whole acting phase for that host.
  The page host reaches the authz check as live page state via `NamedArgs`, not
  as an LLM-supplied arg; `web_host` object ids are `spicedb_escape`-encoded
  because a dotted hostname is not a legal SpiceDB object id. The no-secret
  filler (`pkg/agent/tool/browser/filler.go`): the runner Fetches the bound
  login's credential and types it directly into the page (a short-lived
  in-memory hold keyed by host), so the username/password/OTP never reaches the
  model — the transcript's `type` args carry a placeholder/login ref, not the
  value — and observation (`pkg/agent/tool/browser/snapshot.go`) is blinded to
  filled fields so a subsequent read cannot echo a typed secret back.
  `request_human_step` (the `login_human_step` interaction category,
  `pkg/channels/channelinteractions/categories/`) lets the agent ask a person to
  complete a step (a confirm, or an emailed/SMS code typed into a named page
  element) — the code rides a free-text carrier to the runner and is typed into
  the page but is never placed in the model's context, transcript, logs, or tool
  result (`pkg/agent/tool/browser/humanstep.go`). An operator can mark an
  allowlist+identity site `unattended` (`BrowserSite.Unattended`) so its
  `web_host#act` grant is written at session start with no human card. A
  credentialed browser (default `persistent` when an identity is bound) captures
  cookies + localStorage after each acting call and on graceful stop into a
  per-session Secret — the one Secret the runner is RBAC-allowed to write, never
  model-visible — and re-injects it when the pod wakes after a reap, so a login
  survives pod churn (`pkg/agent/tool/browser/capture.go`).
- **Binaries:** `internal/cmd/streamclient/` (claims a streaming ToolCall via
  the gateway); tool execution is otherwise driven from `internal/cmd/runner/`

## Identity & credentials

An agent acts as itself or on a user's behalf, with named credentials resolved
by need

- **CRDs:** `pkg/apis/v1alpha1/agentidentity_types.go`, `useridentity_types.go`,
  `sessionuseridentity_types.go`, `clusteridentityprovider_types.go`,
  `credentialupdaterequest_types.go`
- **Controllers:**
  `pkg/controllers/{agentidentity,useridentity,clusteridentityprovider,credentialupdaterequest}/`
- **Identity packages:** `pkg/platform/identity/` — `setup/` (`engine`, `store`,
  `llmagent/`, and `builtins/` flows `anthropic_oauth`, `github_pat`,
  `kubectl_kubeconfig`, `oauth_mcp`, `onepassword_scim`, `slack_bot_token`,
  `tailscale_authkey` plus `loader/` and `registry.go`), `authkind/` (`cli`,
  `mcp`, `toolspec`, `sidecartoolbox` + `loader/` + `registry/`), `broker/`
  (token broker, `inproc/` backend), `credresolve/` (named-credential
  resolution), `credupdate/`, `externaltoken/`, `federation/` (the EMA / ID-JAG
  `Minter` seam, with `idjag/` and a fake), `idp/` (`googlekind`, `oidckind`,
  `passwordkind`), `refresh/`, `passthrough{,catalog,link}/`, `provider/`,
  `agentidentity/`, `useridentity/`, `sensitive/`, plus `canonical.go` /
  `principal.go` / `subject.go`
- **Credential masking:** `pkg/x/credmask/`
- **Binaries/services:** `pkg/platform/identityd/` (CLI / OAuth / OIDC /
  password / portal / link HTTP handlers, `ui/`, `icons/`); those routes are
  hosted in-process by `internal/cmd/webd/`, which registers identityd as a
  `pkg/web/webui` plugin and blank-imports the setup-flow and IdP registries.
  The `federation.Minter` is DI-wired in `internal/cmd/runner/` and
  `internal/cmd/operator/`, consumed by `pkg/platform/identity/broker/inproc/`
  and the `pkg/controllers/agentsession/` reconciler

## Authorization

Every action is checked against a SpiceDB graph: can this subject do this, to
this resource, right now?

- **CRDs:** `pkg/apis/v1alpha1/agentsessiongrants_types.go`,
  `spicedbbootstrap_types.go`
- **Controller:** `pkg/controllers/guardian/` (AgentSessionGrants +
  SpiceDBBootstrap sync)
- **Authz packages:** `pkg/authz/` — subject/scope/check logic, CEL funcs, grant
  evaluation, templates and transforms, plus `engine/`, `scope/`, `hooks/`,
  `relwrites/`, `extract/`, `coldstart/`
- **SpiceDB plumbing:** `pkg/authz/spicedb/` (client, schema, bootstrap, grant
  writer, object IDs), `test/testspicedb/` (containerized SpiceDB for the
  integration/e2e suites)
- **Guardian:** `pkg/authz/guardian/` — `approval/`, `grants/`, `leakage/`,
  `schema/`
- **Daemon:** `pkg/authz/authzd/` (`pipelinehost/`), binary
  `internal/cmd/authzd/`
- **Adjacent enforcement seams:** `pkg/memory/spicedbauthorizer/` (SpiceDB-
  backed memory authorization), `pkg/authz/revocation/` (in-flight revocation),
  `pkg/authz/contentguard/` (content inspection), `pkg/authz/untrusted/`
  (nonce-delimited untrusted-content tags), `pkg/controllers/webhooks/`,
  `pkg/authz/validator/`

## Channels & continuity

Agents are reached where teams already work; threads continue and approvals live
in-channel.

- **CRD:** `pkg/apis/v1alpha1/channel_types.go`
- **Controllers:** `pkg/controllers/channel/`, `pkg/controllers/monitoring/`
  (watches framework CRs for status transitions and publishes
  `channelevents.MonitoringEvent`)
- **Channel transports:** `pkg/channels/channelkinds/` — `Kind` interface +
  `registry/`, with backends `slack/`, `bento/`, `browser/`, `local/`, `agent/`,
  `github/`, `fake/`; plus `resolve/` (audience resolution), `outputbind/`,
  `kindtest/`, `attachments.go`, `webauth.go`
- **Daemon & supporting packages:** `pkg/channels/channelsd/` (`outbound/`
  relay, `pipeline/` + `pipelinehost/`, `watchdog/`, `explainer/`,
  `historyresp/`, `e2e/`), `pkg/channels/channelevents/` (the envelope + event
  model), `pkg/channels/channelinteractions/` (semantic interaction categories),
  `pkg/channels/channelkey/`, `pkg/channels/interact/`, `pkg/channels/notice/`
- **Asset renderers:** `pkg/channels/channelassets/` — `Renderer` interface +
  `registry/`, with renderers `html/`, `css/`, `svg/`, `image/`, and the
  operator-only `mcpui/`
- **Attachment ingestion:** `pkg/channels/channelsd/pipeline/attachments.go`
  calls the deliberately powerless extractor service `internal/cmd/extractord/`
  over `pkg/platform/extract/` (`text/`, `tabula/`)
- **Browser surfaces:** `pkg/web/webui/` (the webd UI plugin framework, incl.
  `interact/`), `pkg/x/externalurl/`, `pkg/web/viewurn/`; hosted by
  `internal/cmd/webd/`
- **Binary:** `internal/cmd/channelsd/`

## Memory & knowledge

An agent remembers within reason: an authorized, searchable store with a
knowledge-graph layer and durable artifacts.

- **CRD:** `pkg/apis/v1alpha1/artifactrender_types.go`
- **Controller:** `pkg/controllers/artifactrender/`
- **Memory facade & backends:** `pkg/memory/` (facade, the `Backend` /
  `SearchProvider` / `KGProvider` interfaces, `query`/`search`, `registry`,
  append-only `provenance/` + `publisherkeys/`, backends `inmem/`, `postgres/`,
  `sqlite/`, `shadow/`), with `httpsrv/` + `httpclient/` for the
  token-authenticated HTTP surface, `tokens/`, `assetlimits/`, `memcopy/`
- **Search:** `pkg/memory/search/` — `CompositeSearcher` + RRF ranker
  (`rrf.go`), with providers `inmem/`, `postgres/` (tsvector + pgvector),
  `sqlite/`, `graphiti/`, and `embedding/` (OpenAI-compatible embedder)
- **Knowledge graph:** `pkg/memory/kg/` (`KGProvider` interface) +
  `pkg/memory/kg/graphiti/`
- **Memory kinds (signal-driven hooks):** `pkg/memory/kinds/` — ~30 kinds
  registered via `kinds/all/`, including `turn/`, `lifecycle/`, `sessionscope/`,
  `artifact/`, `artifactrevision/`, `label/`, `lineage/`, `kgingestion/`,
  `toolsession/`, `tool_dispatch_snapshot/`, `approval/`, `parkedprompt/`,
  `coldstarttask/`, `extraction_state/`, `extracted_entity/`,
  `metaagentthread/`, `channel_msg_ref/`, and the append-only audit kinds
  `authzdecision/`, `scopeaudit/`, `relwritesaudit/`, `toolguardaudit/`,
  `toolchainaudit/`, `contentguardaudit/`, `metaagentaudit/`,
  `infoleakage{audit,decision,taint}/`
- **Authorization of recall:** `pkg/memory/spicedbauthorizer/`
- **Artifacts:** `pkg/platform/artifacts/` (versioning, labels, resolve),
  `pkg/platform/artifactstore/` (the `Store` interface + `blob/`, a gocloud.dev
  multi-backend impl selected by `ARTIFACT_STORE_URL`)
- **DI wiring:** `internal/cmd/operator/` (constructs the memory/search/KG stack
  from `POSTGRES_URI`, `EMBEDDING_ENDPOINT`, `GRAPHITI_ENDPOINT`,
  `KG_INGESTION_STRATEGY`, `MEMORY_READ_SOURCE`, …)
