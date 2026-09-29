# Repo conventions for agents

## Where code lives

`pkg/` is grouped into super-packages. A new package goes inside the group that
owns its concern — adding a new top-level `pkg/<thing>/` is the exception, and
needs a reason.

| Group             | Owns                                                                                                                                                                                                                                          |
| ----------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `pkg/apis`        | CRD types. Everything depends on it; it depends on nothing here.                                                                                                                                                                              |
| `pkg/agent`       | The runner: turn loop, harness, LLM providers, session state, tools-as-called.                                                                                                                                                                |
| `pkg/authz`       | Authorization. `spicedb/` holds the backend-specific half; the root is backend-neutral. Also guardian, revocation, pinning, the content/tool guards.                                                                                          |
| `pkg/channels`    | Transports and the conversation surface: channel kinds, channelsd, assets, events, interactions.                                                                                                                                              |
| `pkg/controllers` | Reconcilers, webhooks, status/condition helpers.                                                                                                                                                                                              |
| `pkg/memory`      | Storage, search, knowledge graph, the append-only audit kinds.                                                                                                                                                                                |
| `pkg/platform`    | Cluster and install substrate: cloud kinds, manifests, identity, workspaces, artifacts, NATS, pipeline.                                                                                                                                       |
| `pkg/tools`       | Tool definition and execution: toolspec, toolchains, MCP, sandbox kinds, skills.                                                                                                                                                              |
| `pkg/web`         | Everything served over HTTP: webui, admind, the UI packages, the sandbox gateway.                                                                                                                                                             |
| `pkg/cli`         | Shared CLI presentation. Two packages with disjoint consumers: `tui` (terminal rendering) is used by `cmd/oap`; `clikit` (env-bound flag binding) is used only by the server binaries under `internal/cmd/`.                                  |
| `pkg/gen`         | Code/doc generators driven by magefiles (auditgen, clidocs, crddocs, claudeexec).                                                                                                                                                             |
| `pkg/steelthread` | Captures a finished session's durable records into a replayable bundle for `test/e2e/steelthread`: fold, self-check, fixture rewrite, SpiceDB seed derivation.                                                                                |
| `pkg/x`           | Single-purpose utilities with no better home. Keep it small; a package that grows a domain belongs in a real group.                                                                                                                           |
| `test/`           | Suite harnesses — envtest reaping, the SpiceDB container fixture, the suite lock, e2e. **At the repo root, not under `pkg/`.**                                                                                                                |
| `internal/cmd/`   | Every binary a _user_ never invokes: the cluster components (`operator`, `runner`, `channelsd`, `webd`, `authzd`, `extractord`), the desktop VM's guest agent (`apguest`), and the two repo-local dev tools (`streamclient`, `leadflowfake`). |

`cmd/` holds exactly one binary — `cmd/oap`, the CLI a user installs and runs.
Everything else is under `internal/cmd/<name>/`. The split is about audience,
not about Go's import rule: these are all `package main` and so were never
importable anyway, but `internal/` says out loud that nothing outside this
module has a stake in them. Binary and image names are unaffected by where the
source sits — `go build -o /out/ ./internal/cmd/operator` still emits
`/out/operator` — so a running cluster never sees this layout.

**Both roots are in the ship gate.** `mage test:unit` runs `./pkg/...`,
`./internal/...`, `./cmd/...` and `./test/...`. Any tree-walking guard test must
name `internal/` too: a scan rooted only at `cmd/` now sees the CLI alone and
will keep passing while silently guarding nothing on the server side.

`cmd/oap` is the user-facing CLI and is package `main` in name only: the command
families live in `cmd/oap/internal/<family>cmd/`, with shared plumbing in
`cmd/oap/internal/apcmd` (globals + generic cobra factories) and test fixtures
in `cmd/oap/internal/aptest`. Only `main.go`, the root command wiring, and the
registry blank imports stay at the root. **Subpackages cannot import `main`** —
if a command needs a helper, the helper moves down into `internal/`, it does not
get exported from `main`.

## Pluggability is a primary design goal

Most moving parts in this project are designed to be swapped — channel
transports, artifact renderers, tool CRD kinds, identity setup flows, LLM
providers, memory stores, and so on. The ones already factored as
`interface + registry` (or `interface + DI`):

| Aspect                            | Interface                                   | Wiring                                                                                      | Backends today                                                                                                   |
| --------------------------------- | ------------------------------------------- | ------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------- |
| Channel transports                | `pkg/channels/channelkinds.Kind`            | `pkg/channels/channelkinds/registry`                                                        | agent, bento, browser, fake, github, local, slack                                                                |
| Artifact renderers                | `pkg/channels/channelassets.Renderer`       | `pkg/channels/channelassets/registry`                                                       | html, css, svg, image, mcpui (operator-only, not agent-selectable)                                               |
| Tool CRD kinds (CLI presentation) | `pkg/tools/contract.Kind`                   | `pkg/tools/kinds/registry`                                                                  | SpiceboxToolspec, MCPServer, SidecarToolbox                                                                      |
| Toolspec parsers                  | `pkg/tools/toolspec/parser.Parser`          | `pkg/tools/toolspec/parser/builtin` registry                                                | declarative (only kind in production use today); builtin registry defined, none registered                       |
| AgentIdentity setup flows         | `pkg/platform/identity/setup/builtins.Flow` | `builtins.Register` (`pkg/platform/identity/setup/builtins/registry.go`)                    | anthropic-oauth, github-pat, kubectl-kubeconfig, oauth-mcp, onepassword-scim, slack-bot-token, tailscale-authkey |
| AgentIdentity auth kinds          | `pkg/platform/identity/authkind.Kind`       | `authkind/registry`                                                                         | cli, mcp, toolbox, toolspec                                                                                      |
| Identity credential types         | `pkg/platform/identity/credkind.Kind`       | `credkind/registry` (+ the `credkind/imports` bundle)                                       | static, oauth, federated, githubapp                                                                              |
| Session state kinds               | `pkg/agent/session/state.Kind`              | `state.Register` (`pkg/agent/session/state/registry.go`)                                    | plans, deliveries, triggerstatus                                                                                 |
| Completion requirements           | `pkg/agent/completion.Requirement`          | `completion.Register` (`pkg/agent/completion/registry.go`)                                  | artifact-delivered, plan-steps-complete, trigger-status-concluded                                                |
| Agent harnesses                   | `pkg/agent/harness.Harness`                 | `pkg/agent/harness/registry`                                                                | ap-native                                                                                                        |
| Interaction categories            | `pkg/channels/channelinteractions.Category` | `pkg/channels/channelinteractions` registry                                                 | 44 registered (15 interactive + 29 notice) — see `pkg/channels/channelinteractions/categories`                   |
| Workspace sources                 | `pkg/platform/workspacekinds.Kind`          | `pkg/platform/workspacekinds/registry`                                                      | git                                                                                                              |
| Sandbox backends                  | `pkg/tools/sandboxkinds.Kind`               | `pkg/tools/sandboxkinds/registry`                                                           | pod, agent-sandbox                                                                                               |
| LLM providers                     | `pkg/agent/llm.Provider`                    | `pkg/agent/llm/providers` registry                                                          | anthropic, openai, openrouter                                                                                    |
| Web search providers              | `pkg/tools/websearch.Provider`              | overridable factory var in `cmd/oap/internal/toolscmd` (switch on the `--web-search` value) | anthropic, fake                                                                                                  |
| Toolspec LLM                      | `pkg/tools/toolspec/llm.Provider`           | none — unwired since the `oap tools gen` retirement                                         | none in production (interface + `promptlib` adapter retained for a future toolspec-authoring surface)            |
| Token broker                      | `pkg/platform/identity/broker.Broker`       | DI in `internal/cmd/runner` + `internal/cmd/operator`                                       | inproc                                                                                                           |
| Memory stores                     | `pkg/memory.Backend`                        | DI in `internal/cmd/operator`                                                               | inmem, sqlite, shadow (postgres dual-write)                                                                      |
| Search providers                  | `pkg/memory.SearchProvider`                 | DI in `internal/cmd/operator`                                                               | inmem, postgres, sqlite, graphiti                                                                                |
| KG providers                      | `pkg/memory.KGProvider`                     | DI in `internal/cmd/operator`                                                               | graphiti                                                                                                         |
| Embedding providers               | `pkg/memory.EmbeddingProvider`              | DI in `internal/cmd/operator`                                                               | openai-compatible                                                                                                |
| Artifact stores                   | `pkg/platform/artifactstore.Store`          | DI (`ARTIFACT_STORE_URL`)                                                                   | gocloud blob: gs/s3/azblob/file/mem                                                                              |
| Cluster kinds                     | `pkg/platform/cloud.Strategy`               | `pkg/platform/cloud` registry                                                               | local, desktop, default, gke, eks, aks                                                                           |
| Terminal drivers                  | `pkg/cli/tui.Driver`                        | `pkg/cli/tui.DriverFor` (from `Caps`)                                                       | tty, plain, noninteractive                                                                                       |

**Rules of thumb:**

- New variants of an aspect get _registered_, not branched on. If you're writing
  `if kind == "slack"` (or equivalent) outside the kind's own package, the
  abstraction is missing a method — fix the interface, don't add a switch in the
  consumer.
- Single-implementation interfaces (`memory.Store`) are kept as interfaces
  _anticipating_ swap-out (Postgres memory, …). Don't collapse them into
  concrete types just because today there's only one backend.
  (`artifactstore.Store` used to be such a case; it now has a real multi-backend
  impl — `pkg/platform/artifactstore/blob` over gocloud.dev, selected by
  `ARTIFACT_STORE_URL`.)
- New aspects that _should_ be pluggable but aren't yet (prompt builder,
  watchdog policy, channel sub-channels like `permission_request` for non-Slack
  kinds) belong in a tracked backlog item with the shape of the abstraction
  noted, not coded as one-offs.
- **Registry vs. DI:** registry when the consumer dispatches by a string name
  read from a CRD spec (channel.kind, mcpserver.kind, agentidentity binding
  prefix). DI when there's exactly one concrete chosen by the binary at startup
  (web search, toolspec LLM).
- **Two substrates are deliberately not pluggable.** The NATS bus
  (`pkg/platform/nats`, imported concretely by six binaries) and secret storage
  (`corev1.Secret`, referenced by 60 non-test files) are substrate, like the API
  server and SpiceDB — not gaps. Upstream secret managers integrate one layer
  down via External Secrets Operator or the Secrets Store CSI driver, without AP
  changing a line. Revisit the bus only if a concrete adopter requirement
  appears; do not add a `pkg/bus` abstraction speculatively.

**Adding a new backend** (worked example for a new channel kind):

1. New package `pkg/channels/channelkinds/<name>/` with `kind.go`,
   `listener.go`, `sender.go`, `wizard.go`.
2. `func init() { registry.Register(&Kind{}) }` in `kind.go`.
3. The blank import (`_ "…/<name>"`) goes in the binaries that need it:
   `internal/cmd/channelsd`, `cmd/oap`. Consumers (relay, wizard dispatcher,
   status reconcilers) are untouched.
4. If the new transport needs a new sub-channel (permission_request for Slack
   today), add the sub-channel name to
   `pkg/channels/channelkinds.SubChannelSenderFor`'s contract — same registry
   stays.

The same shape applies to artifact renderers, tool kinds, identity flows, etc.
When the diff for a new backend touches anything outside its own package + a
single blank import, that's a sign the seam isn't in the right place.

**Identity credential types are a deliberate exception to "the blank import goes
in the binaries."** `pkg/platform/identity/broker/inproc/broker.go`
blank-imports `credkind/imports` at LIBRARY level, not from a binary's `main`.
The broker cannot function with an empty registry — every resolve dispatches
through it — and the credential-type set is closed by the CRD's
`+kubebuilder:validation:Enum` marker, unlike a channel transport, where an
operator genuinely chooses which ones to install. A binary linking the broker
gets every credkind for free; there is no "install only the oauth kind" case to
support.

## Cluster kinds

`pkg/platform/cloud` replaced a single boolean — born as `localMode` in
`oap init`, renamed `allowSharedOrigin` inside `runInstall`, hardcoded `true` by
`ap desktop` — that was branched on in thirteen places across `cmd/oap`,
`internal/cmd/operator`, and `internal/cmd/webd` to decide install shape. One of
those sites never received the bool at all: the operator re-derived local-ness
by sniffing its own memory backend (`backendKind != memoryBackendPostgres`), so
an install running Postgres locally or sqlite remotely silently flipped an
authorization gate. Six kinds are registered now: `local`, `desktop`, `default`
(`pkg/platform/cloud/unmanaged`), `gke`, `eks`, `aks`.

- **`local` and `desktop` are opt-in only** — `--local` /
  `--cluster-kind=local`, and `cloud.MustFor(cloud.KeyDesktop)` passed directly
  by `oap desktop`. Neither reports a `ProviderIDPrefix()`, so `cloud.Detect`
  can never return either: auto-selecting one on a real cluster would install
  sqlite and an in-memory SpiceDB datastore onto durable infrastructure.
- **`desktop` differs from `local` in one `InstallProfile` answer, and that
  answer currently gates nothing.** `pkg/platform/cloud/desktop` embeds
  `local.Strategy` and overrides only `Key`, `DisplayName`, and `InstallProfile`
  — do not clone a kind package to make a variant. The one differing answer is
  `ServesLocalWebChat()`: true for `desktop`, false for `local`. No binary reads
  it. The browser surfaces it once named — the transcript data plane
  (`pkg/web/webui/chat`), the agent-UI plugin (`pkg/web/webui/agentui`) and the
  session shell (`pkg/web/webui/sessions`) — each authorize every request on
  `agentsession#interact`, fully consistently, and each mounts wherever its own
  collaborators are configured, on every cluster kind. It never gated a channel
  kind either (the `browser` kind registers everywhere, `desktop` included). The
  method is retained because it is the only thing distinguishing two registered
  kinds; what SHOULD distinguish them is an open question.
- **The first-boot invariant is a property, not a key list.**
  `pkg/platform/cloud/invariants_test.go` asserts that any kind whose
  `SpiceDBDatastore()` is `DatastoreMemory` must be opt-in only — no
  `ProviderIDPrefix()`, and not `KeyDefault`. SpiceDB's
  `--datastore-bootstrap-files` is fatal against a non-empty datastore, so an
  ephemeral one is safe only for a kind that can never land on shared,
  persistent, multi-replica infrastructure. Asserting the property rather than
  naming `local` is what let `desktop` be added without weakening the rule.
- **Lookup is fail-closed.** `cloud.For(key)` errors on both an unknown and an
  empty key — no silent downgrade to the fallback kind. A typo'd
  `--cluster-kind` (or a stale `AP_CLUSTER_KIND`) on GKE would otherwise install
  with no Hyperdisk pinning, no GCS artifact bucket, and no managed Gateway, all
  without a word. `cloud.Default()` is the only route to the `default` fallback,
  so every fallback is spelled out at its call site; `MustFor` panics and is for
  `init()`/tests only.
- **`Strategy.Validate(ctx, ValidateParams)` runs before any mutation**, on
  every install — explicitly chosen or detected. `local` refuses a managed-cloud
  `providerID` unconditionally; `--allow-non-local-cluster` relaxes only the
  kubeconfig-host heuristic (for a VPN'd corporate dev cluster), never a
  managed-cloud refusal, on any kind.
- **`AP_CLUSTER_KIND` carries the resolved kind to the operator and webd.**
  `oap install` stamps it onto both Deployments; each resolves the same registry
  at its own startup, fail-closed — unset or unrecognized crash-loops rather
  than silently defaulting. It deliberately gets no default in `config/`: a base
  default would make "unset" mean `default` instead of fatal. Accepted
  consequence: upgrading a pre-cluster-kind install requires re-running
  `oap install`.

## Memory, Search, and Knowledge Graph

The memory framework has three layers: storage (Backend), ranked retrieval
(SearchProvider), and knowledge graph intelligence (KGProvider).

### Memory Backend stack

```
Local facade (pkg/memory/facade.go)
  ├── Backend: ShadowBackend (inmem + postgres, switchable reads)
  ├── SearchProviders: [inmem, postgres, graphiti]
  ├── Searcher: CompositeSearcher (RRF ranking + SpiceDB authz)
  └── Logger: logr for best-effort index error logging
```

The operator constructs this in `internal/cmd/operator/main.go`. Environment
variables control what's available:

| Env var                 | Effect                                                         |
| ----------------------- | -------------------------------------------------------------- |
| `POSTGRES_URI`          | Enables postgres backend + shadow dual-write + postgres search |
| `EMBEDDING_ENDPOINT`    | Enables vector search in postgres (OpenAI-compatible)          |
| `GRAPHITI_ENDPOINT`     | Enables graphiti search provider + KG provider + ingestion     |
| `KG_INGESTION_STRATEGY` | `every_turn` (default), `content_gated`, or `batch`            |
| `MEMORY_READ_SOURCE`    | `primary` (inmem) or `secondary` (postgres) for shadow reads   |

### Search architecture

`Memory.Search` delegates to `CompositeSearcher`, which fans out to all
registered `SearchProvider`s concurrently, merges results via RRF (Reciprocal
Rank Fusion), and post-filters through SpiceDB. Each provider owns its own
capabilities:

- **inmem**: structured filters only (tags, time range), score 1.0
- **postgres**: tsvector FTS + pgvector cosine similarity + structured
- **graphiti**: graph-aware search via Graphiti REST API

`Memory.Query` (structured recall) and `Memory.Search` (ranked retrieval)
coexist. Query is boolean filtering; Search is scoring.

### Knowledge graph

Graphiti runs as a sidecar (`zepai/graphiti:latest`) with Neo4j for graph
storage. The `KGProvider` interface provides graph-native queries (entity
lookup, fact traversal, related entities, communities).

Ingestion is signal-driven: the `kg_ingestion` Kind's ScopeHooks reacts to
`lifecycle/turn.completed` signals, reads the turn content, and calls
`KGProvider.Ingest`. Graphiti handles entity extraction, dedup, and
contradiction detection asynchronously.

Key packages:

- `pkg/memory/search/` -- CompositeSearcher, RRF ranker
- `pkg/memory/search/postgres/` -- tsvector + pgvector provider
- `pkg/memory/search/graphiti/` -- Graphiti search + ingest client
- `pkg/memory/search/embedding/` -- OpenAI-compatible embedder
- `pkg/memory/search/inmem/` -- structured-only fallback
- `pkg/memory/kg.go` -- the KGProvider interface (package `memory`, not `kg`)
- `pkg/memory/kg/graphiti/` -- GraphitiKGProvider (`pkg/memory/kg/` has no root
  Go files)
- `pkg/memory/kinds/kgingestion/` -- ingestion hooks

### CLI commands

```
oap memory list <session>                        # list entries
oap memory get <session> <id>                    # get entry by ID
oap memory query <session> [filters]             # structured query
oap memory search <session> --text "query"       # ranked search
oap memory reindex <session>                     # rebuild search indexes
oap memory put/delete/share ...                  # write operations

oap kg search <session> --text "query"           # search facts
oap kg entity <session> <uuid>                   # get entity
oap kg facts <session> --entity <uuid>           # entity's facts
oap kg related <session> --entity <uuid>         # related entities
oap kg communities <session>                     # entity clusters
```

### Agent tools

Three memory/KG tools are registered for agents:

- `query_memory` -- structured recall (tags, fields, time ranges)
- `search_memory` -- ranked text/vector/structured search
- `query_knowledge` -- KG graph queries (facts, entities, communities)

### Tamper-evident audit log

The transcript, authz-decision, tool-session, tool-catalog, trigger-delivery,
and audit kinds are **append-only**: declared via `Retention.AppendOnly` and
enforced at the `memory.Local` facade — a `Put` that changes existing content
returns `ErrAppendOnlyConflict`, per-entry `Delete` is refused, byte-identical
re-put is idempotent. Scope-level deletion (retention / session GC) is still
allowed.

Each append-only Entry carries a `Provenance` envelope: publisher, keyID, a
per-`(scope, publisher)` monotonic seq, prevHash, and an Ed25519 sig over a
canonical entry digest. The seq + prevHash form a hash chain per
`(scope, publisher)`, so modification, fabrication, gaps, and reordering are
detectable — and, with the tail anchor, truncation too.

Key custody: the AgentSession reconciler mints a per-session Ed25519 keypair
into the per-session Secret (`audit-signing-key`) and anchors the public half on
`AgentSession.status.auditPublicKey` / `auditKeyID` (the K8s-witnessed trust
root); chain heads land on `status.auditChainHeads` at completion. Component
publishers (channelsd / authzd / operator) mint at startup and register their
pubkeys via `POST /memory/_publisher_key` into the `publisher-keys` ConfigMap.
The operator's facade **verifies on write**: unsigned or forged append-only
writes are rejected from both token callers and in-process writers.
`oap audit verify <session>` walks every `(scope, publisher)` chain offline
against those keys and exits non-zero on hard findings.

Operational consequence: `oap memory put` / `oap memory delete` of an
append-only kind is now rejected — only the runner / components / operator may
write those kinds, and only through the signing path. See
`pkg/memory/provenance/` and `pkg/memory/publisherkeys/`.

## Manifest regeneration

`pkg/platform/manifests/install.yaml` is the canonical install bundle embedded
into `oap` and applied by `oap install` / `oap init`. It is **generated** from
the kustomize tree under `config/` — never edit it by hand.

The pipeline is two-stage:

```
+kubebuilder:rbac markers in pkg/controllers/*/controller.go
+ pkg/apis/v1alpha1/*_types.go
                  │
                  │  mage gen:api  (controller-gen: CRDs + role.yaml + deepcopy)
                  ▼
config/**  (CRDs, role.yaml, Deployments, Services, etc.)
                  │
                  │  mage manifests  (kubectl kustomize config/)
                  ▼
pkg/platform/manifests/install.yaml  ← embedded into ap, applied by oap install
```

Run **both** after any change to:

- A `+kubebuilder:rbac:` marker on a controller (added a new resource to watch,
  changed verbs, etc.) — `mage gen:api` re-emits `config/manager/role.yaml`,
  then `mage manifests` re-bundles.
- A CRD-shaping `*_types.go` field — same chain.
- Anything under `config/**` directly (a new resource added to a kustomization,
  a Deployment edit, etc.) — `mage manifests` alone is enough.

```bash
mage gen:api    # only when controller markers or types changed
mage manifests  # always after gen:api, OR after any config/ edit
```

## Documentation regeneration

`mage docs:cli` and `mage docs:crd` regenerate the docs site's reference pages —
one from the live cobra tree, one from the CRD schemas under `config/crds` —
writing into `site/content/docs`. Both are plain deterministic generators;
neither invokes an LLM.

`PRIMITIVES.md` (repo root) and `docs/owasp-agentic-top10-coverage.html` are
both hand-maintained: update them by hand when a primitive's code moves or the
OWASP coverage picture changes. Neither has a regeneration command — the
Claude-driven generator that used to write them (`docsgen`) was removed as
internal-only tooling, not something the published project needs.

## SidecarToolbox conventions

`SidecarToolbox` is a CRD that describes a user-supplied MCP server running as a
sidecar container alongside the agent runner pod. Key design constraints:

**Credential resolution is controller-side, not broker-side.** The AgentSession
reconciler resolves upstream credentials via `credresolve.Resolve()` in
`materializeSidecarSecret` and writes a per-session Secret that the sidecar
`envFrom`s. The value is frozen at pod-create time — there is NO OAuth
JIT-refresh for sidecar creds. A future init-container/broker approach is
deferred; do not implement it here.

**`effectiveAllowedHosts` is recorded, not enforced; `effectiveNetworkMode` IS
enforced.** The merged egress allowlist lands on
`AgentSession.status.resolvedSidecarToolboxes[].effectiveAllowedHosts` and
`effectiveNetworkMode`. The operator **does** emit per-session NetworkPolicies
(`pkg/controllers/agentsession/netpol.go` — runner, sandbox, and separate-pod
sidecar builders; default-on via `--session-network-policies`), but enforcement
is L3/L4 only:

- `effectiveNetworkMode` is enforced: `none` → deny-all, `allowlist` → DNS plus
  coarse TCP 443/80 to `0.0.0.0/0`.
- `effectiveAllowedHosts` is **not** enforced. Hostname-level egress needs a
  DNS-aware network-policy controller (Cilium, Calico) consuming the status
  snapshot. That half is permanently the cluster operator's.

Do not read `cosidecar.EgressPolicy.AllowedHosts` as an enforcement input: it is
populated but never consumed. Per-MCPServer egress ports remain open work.

**Two different `sidecartoolbox` packages; do not conflate them.**
`pkg/tools/kinds/sidecartoolbox/` is the CLI-**presentation** Kind (`Row`,
`Detail`, `ValidateFile`), following the same registry pattern as `sandbox` and
`mcp`; it is blank-imported by `cmd/oap` (`cmd/oap/internal/toolscmd/root.go`)
and touches no credential. `pkg/agent/tool/sidecartoolbox/` is the **execution**
synthesizer (`Synthesize`), imported directly by `internal/cmd/runner` to build
sidecar tools from session status. A new CLI Kind gets the blank import; the
synthesizer does not.

## Nil interfaces: never assign a typed-nil pointer directly

Go's interface representation is a tuple `{type, value}`. Assigning a typed-nil
pointer to an interface field produces a **non-nil interface** whose `value`
happens to be nil. The interface's `== nil` comparison returns `false`, but any
method call on it dereferences the nil pointer and panics.

We hit this in production: `internal/cmd/operator/main.go` declared
`var spiceDBClient *spicedb.Client`, left it nil when `SPICEDB_ENDPOINT` was
unset, then assigned it into `agentclassctrl.Reconciler.SpiceDBSchema` (an
interface field). The guard `r.SpiceDBSchema != nil` returned `true`, the
AgentClass controller panicked on every reconcile with mcpServers, and
controller-runtime's silent panic recovery hid the failure for hours.

**Never do this:**

```go
var x *foo.Client     // declared, possibly never assigned
return Reconciler{
    SpiceDBSchema: x,  // typed-nil pointer → non-nil interface!
}
```

**Always do this:**

```go
var x *foo.Client
var iface foo.Schema   // declared as the interface; default zero value is a true nil interface
if shouldConstruct {
    x = foo.NewClient(...)
    iface = x
}
return Reconciler{
    SpiceDBSchema: iface,  // genuine nil when shouldConstruct was false
}
```

The same rule applies at every interface-field-from-pointer boundary. When in
doubt, declare the variable as the interface type, not the pointer type, and
only assign once you have a real value.

## Never silently drop errors

Every error path in this codebase MUST do at least one of:

1. **Return the error to the caller** so they can decide how to handle it.
2. **Log it** at INFO or higher with enough structured context (session, kind,
   subject, etc.) that an operator reading logs can locate the failing component
   without source-diving.
3. **Surface a user-visible message** when the failure affects something the
   user is waiting for (a channel reply, an `oap` CLI command, an API response).
   A generic "something went wrong; check logs" is far better than silence — the
   user at least knows the system noticed.

In practice, this rules out patterns like:

```go
_, _ = sender.Send(ctx, sess, env)        // discards both return + error
if err != nil { return }                  // returns without logging
return                                    // bare return on an error path
```

…and prefers:

```go
if _, err := sender.Send(ctx, sess, env); err != nil {
    logger.Info("sender.Send errored", "session", sess.Name, "err", err.Error())
    // optional: surface to user via a fallback channel write
}
```

This rule exists because we hit a real production failure where the runner
published a `respond_to_user` envelope, the channelsd outbound relay consumed
it, `sender.Send` returned an error — and every layer swallowed the error
silently. The user saw "delivered" from the agent and no message in Slack.
Diagnosing it took longer than it should have because there was no log to grep
for. Don't repeat this.

The runtime watchdog (channelsd's silence watchdog, the runner's per-turn timer)
is **not** a substitute for explicit error handling — it only catches _complete_
silence, not partial failures where some sub-step worked. Always log + surface;
let the watchdog be the last line of defense, not the first.

Forgetting this is silent at build time but breaks at deploy time — usually as
the operator crash-looping with "failed to wait for caches to sync" because its
ClusterRole is missing a watch verb for a resource a controller registers. The
`TestInstallYAMLMatchesKustomize` test in `pkg/platform/manifests/` catches this
drift in CI.

## Prefer a library over a hand-rolled utility

When a well-maintained library — the standard library first, then something
already in `go.mod` — solves the problem, use it. Do not hand-roll a
near-equivalent. Hand-rolled utilities look small at the point they are written
and then quietly accumulate the edge cases the library already handles, in a
place nobody thinks to test.

**Check `go.mod` before writing a helper.** Most of what gets hand-rolled here
is already a dependency. `github.com/dustin/go-humanize` is the standing
example: byte sizes (`humanize.Bytes`), thousands separators (`humanize.Comma`),
SI units (`humanize.SIWithDigits`), relative times (`humanize.Time`). A
hand-written `formatBytes` that switches on KB/MB/GB is the exact shape to
reject — see `pkg/agent/postsession/cost/format.go` and
`pkg/channels/channelsd/pipeline/attachments.go` for how it should look instead.

The same reflex applies to parsing, time and duration formatting, sorting and
comparison helpers, retry/backoff, set and slice operations reachable from
`slices`/`maps`, and anything involving Unicode, escaping, or encoding — the
categories where "it works on my inputs" and "it is correct" diverge quietly.

**When the library's output is close but not exactly what you want, post-process
it — do not reimplement it.** The hard part is the bucketing, rounding and
pluralization, not the punctuation.
`pkg/channels/channelkinds/slack/ sender_turn_progress.go:86` is the pattern: it
keeps `humanize.SIWithDigits`'s `"1.2 k"` and normalizes only the presentation
to `"1.2K"` with `strings.ToUpper(strings.ReplaceAll(s, " ", ""))`.
Reimplementing to change a space is how you inherit every rounding edge case the
library had already solved. Note too that stdlib often already fits —
`time.Duration.String()` gives `"34s"` / `"1m20s"` with no dependency at all.

**Where the rule stops.** This is a judgement call, not a licence to add
dependencies:

- **Do not add a new module for a genuine one-liner.** A three-line helper you
  fully control beats a new supply-chain edge — be deliberate about adding one.
  Prefer stdlib → existing `go.mod` entry → new dependency, in that order, and
  justify the third.
- **Do not wrap a library in a helper that only renames it.** That is a second
  thing to learn, not an abstraction.
- **A domain-specific thing that merely resembles a general one is not a
  duplicate.** Our subject canonicalization, our scope keys, and our toolspec
  argument binding look like general utilities and are not — they encode repo
  semantics, and forcing them onto a library is a bug, not DRY.
- **Do not rewrite a working hand-rolled helper just to satisfy this rule.** Fix
  it when you are already touching it, or when it has a defect. A no-value
  refactor costs review, merge conflicts, and git blame (see the gating function
  in §Test conventions — the same judgement applies).

## Server-side apply: keep applied fields idempotent; put observations in status

A client that server-side-applies (SSA) a resource owns the fields it sets,
under its field manager. **A byte-identical re-apply MUST be a no-op.** The
moment a volatile value — `time.Now()`, a random ID, a monotonic counter, a
freshly-recomputed hash — lands in an SSA-applied field (spec, a label, an
annotation), every re-apply rewrites the object: field ownership churns, the
apply stops being idempotent, and any controller watching that field
re-reconciles on every apply. That is a merge/apply-semantics violation, not a
cosmetic one.

We hit this: `oap agent install` SSA-applied an `oap-source` provenance
annotation that embedded `installedAt: time.Now()`. Two installs of the exact
same bundle produced two different annotations, so re-install stopped being an
SSA no-op and the AgentClass controller re-mirrored the annotation into `status`
on every apply. The fix moved the timestamp out of the applied annotation (which
became a stable `{ref,digest,version,sourceKind}`) and into controller-owned
`status`, set once per digest.

**Rules:**

- **Volatile/observed values belong in controller-owned `status`, never in an
  applied field.** Install/observation timestamps, `lastSeen`, computed digests,
  generation counters — the owning controller writes them to `status` (via
  `Status().Update` / the owned-status write) with set-once /
  set-on-meaningful-change semantics: preserve the existing value when the
  inputs that define it are unchanged, update only when they actually change.
  The client SSA-applies only the stable desired state.
- **Keep the SSA payload a pure function of its inputs.** The fields a client
  applies must derive solely from its inputs (the bundle, the flags) — no
  wall-clock, no randomness — so identical inputs always yield byte-identical
  applied fields.
- **Respect subresource + field-manager separation.** A client applies
  spec+metadata under its field manager (with `ForceOwnership` only when it must
  own them); the controller owns `status` via the status subresource. A
  controller must NOT do a plain `Client.Update` that rewrites client-owned
  metadata/annotations with a stale value, and a client must NOT apply a
  `status` stanza (sanitize it out before applying).
- **Check this at PLANNING time, not just review.** When a plan adds a field a
  client writes and can re-write, ask before coding: _is a byte-identical
  re-apply a no-op? does any volatile value leak into an applied field? is this
  an observation that belongs in `status` instead?_ Note the answer in the
  plan's Global Constraints.

## First-boot-only initialization never belongs in a serving container

A step that is only valid **once**, against **empty** state — seeding a schema,
creating a database, claiming an ID — must run exactly once: in a Job, or in an
operator/controller reconcile. Never in the startup path of a container that is
replicated or restartable.

The failure mode is not a flake; it is a permanent, self-sustaining deadlock.
Put a once-only step in a serving container and:

- **At N replicas**, exactly one wins the race and writes the state. The losers
  now find non-empty state and die. Their killer was created by their own
  sibling and never goes away, so they crashloop _forever_ — deleting the pod,
  restarting it, or waiting longer can never help.
- **At 1 replica**, install _succeeds_ — and the pod dies on its **next**
  restart (a drain, an eviction, an image bump, an OOM kill), long after the
  install that planted the mine. This is the more dangerous variant, because
  nothing fails at the time the mistake is made.

We shipped exactly this: `oap install` set SpiceDB's `datastoreBootstrapFiles`
on a 2-replica Deployment sharing one Postgres datastore. SpiceDB's bootstrap
applies only to an empty datastore and is **fatal** against a non-empty one, so
one replica served and the other crashlooped permanently, wedging `oap install`
at "SpiceDB (authorization)". It survived review because the local-dev path uses
an in-memory datastore that genuinely _is_ empty on every boot — **the bug was
invisible in the mode everyone tests.** See
`cmd/oap/internal/installcmd/spicedb_cluster.go` (`bootstrapsSchemaAtStartup`).

**Rules:**

- **Ask "is every boot a first boot?"** A once-only step is safe in a container
  only when its state is ephemeral and process-local _and_ the replica count is
  1. Encode that as a predicate the config derives from, not as a comment.
- **Beware the mode asymmetry.** When one backend is ephemeral (memory, a temp
  dir) and another is shared/persistent (Postgres, a PVC, an object store), a
  once-only step is safe in the first and fatal in the second. Local dev will
  not catch it. Reason about the persistent mode explicitly.
- **Make the unsafe combination unrepresentable.** Refuse to emit it — return an
  error at construction — rather than trusting a branch to stay correct through
  a future edit. Assert the invariant in a table-driven test over every mode.
- **A crash that only a restart reveals still needs a test.** The `replicas: 1`
  variant passes install and fails weeks later; cover it by asserting the
  config-level invariant, and, where practical, by restarting the workload in
  e2e and requiring it to return to Ready.

## Ship gate: all three test suites must pass before anything merges

**Nothing ships — no merge to `master`, no push, no "done" — until all three
suites are green:**

```bash
mage test:unit         # go test -race ./pkg/...           (no build tags)
mage test:integration  # -tags=integration + envtest        (apiserver/etcd)
mage test:e2e          # -tags=e2e + envtest + SpiceDB       (in-process harness)
```

**`test:e2e` includes the bronzethread bundles** (`test/e2e/bronzethread`) — the
scripted whole-session scenarios. They are not a separate target and need no
separate command, which is exactly why they are easy to forget: a feature can
ship with unit, integration and e2e all green and no bundle covering it, because
nothing in the gate output says "you added behavior and no scenario exercises
it."

**A new user-visible behavior wants a bundle**, and the bundles are where this
repo's most expensive misses were caught. Each is a JSON transcript under
`test/e2e/bronzethread/testdata/<name>/`, picked up by discovery on the presence
of `bundle.json`.

**A bundle starts one of two ways.** `userTurns` drives a conversational session
through the `fake` kind — the default, and for a long time the only one, which
is why two separate changes were written up as "a bundle cannot exercise this"
before the alternative existed. A `trigger` block instead starts the run from a
SIGNED WEBHOOK posted to the production `channelwebhook` route: the driver reads
the named input Channel's own kind and credentials, signs with them, and lets
HMAC verification, event filtering and channel-key derivation all run for real.
The two are mutually exclusive and the loader says so. Use `trigger` for
anything a person did not type — a pull request, a cron, any inbound the agent
did not get from a human.

Three rules, each learned the hard way:

- **Assert on the TOOL RESULT, not the reply.** A scripted transcript says the
  same words whether the call returned rows or a permission error, so
  `agentReplyContains` alone proves nothing about authorization. Use
  `lastToolResultContains` / `NotContains` / `IsError`.
- **Exercise the behavior more than once when the claim is about durability.**
  The phase-approval bundle made a single permissioned call and passed whether
  the approval persisted or was re-granted per call — which is how a missing
  decision-writer sat behind a green scenario. Counting cards
  (`approvalPrompts`) is what distinguished them.
- **MCP tool args are NESTED** — `{"operation_id":…, "_reason":…, "args":{…}}`.
  Flat args fail with "mcp: operation_id and _reason are required", and every
  bundle was silently doing this because none asserted on result content.

A bundle may also carry a **golden authorization trace** at
`testdata/<name>/trace.golden` — the sorted, deduplicated set of gate events and
authz decisions the run produced. Property assertions say what the author
thought to claim; a golden says everything that happened, so a change nobody
named still shows up in a diff.

```bash
BRONZE_UPDATE_GOLDEN=1 go test -tags=e2e ./test/e2e/bronzethread/   # regenerate
```

Two rules, and both are about keeping the golden worth having:

- **Promote, don't start with one.** A golden is frozen only _after_ the
  property assertions establish the scenario is right. Freeze first and it pins
  whatever the code did that day, bug included. A bundle with no `trace.golden`
  is simply not promoted yet, and runs exactly as before.
- **Read the diff when you regenerate.** A golden regenerated without being read
  is a test that asserts nothing, and that is the standard way this kind of
  harness rots. If the trace changed and you cannot say why, that is the
  regression it exists to catch.

The trace is sorted rather than chronological on purpose: record order across
two independently-written logs is not reproducible, and a golden that moves on
scheduling noise gets regenerated blindly. Where order carries meaning,
`assert.authz.decisions` already compares the recorded sequence per key.

### Steel threads: a bundle captured from a real session

**A steel thread is a bundle whose transcript was CAPTURED from a real session,
not authored.**
`oap session capture <session> --output test/e2e/steelthread/testdata/<name>`
reads a finished session's durable records — the transcript, the offered tool
catalogs, the system prompt, the authz and plan-gate logs, the relationship
writes, the signed delivery that opened it — and emits a bundle, its fixture
manifests, the SpiceDB seed its authorization depended on, and a golden trace.
Nothing in the path invokes a model: every emitted field is derived from a
record or left empty. `--overwrite` replaces a previous capture in `--output` in
place — it empties the directory first — but only after confirming the directory
already holds a `bundle.json`; without that marker it refuses rather than
deleting whatever is actually there, which is how a mistyped `--output` gets
caught instead of swept.

Same driver (`test/e2e/threadrun`), same divergence contract, own suite
(`mage test:steel`, folded into `mage test:e2e` through its `./test/e2e/...`
glob). The difference is evidentiary, and it is the whole reason the suites are
separate rather than a comment on a shared one: a green bronze bundle proves the
SYSTEM handles an interaction; a green steel bundle also shows a real model
produced it, at least once. Neither pins model behavior going forward —
replaying a recording of a model is not a claim about what it will do next.

The capture **refuses rather than emitting a bundle that would replay
differently than it recorded.** Eighteen self-check finding codes gate the write
— some hard (nothing is written), some warnings (the bundle is written, but the
finding says what was synthesized or dropped to make that possible). The
finding-code block in `pkg/steelthread/selfcheck.go` is self-documenting,
including a section on what it deliberately does NOT catch.

Two memory Kinds exist because a capture needs them, and they are consumed
differently. `trigger_delivery` records the signed webhook body that opened a
triggered session — for a session nobody typed into, that payload IS the entire
input, `channel_msg_ref`'s opaque reference cannot reconstruct it, and the
capture re-emits it as the bundle's `trigger` block. `tool_catalog` records
which tools the runner OFFERED, not which ones the model called — a tool
withheld by a capability gate or a plan-gate phase is otherwise invisible in a
transcript, because not calling a tool you were never offered looks exactly like
not calling a tool you declined — and the capture carries every recorded change
into `bundle.toolCatalogs`, where **the replay offers exactly the recorded set
at each turn**. A recorded tool missing at replay fails; so does an extra,
because a gate that stops withholding is the regression the record exists to
catch. The one exemption is `expectedExtraTools`, DERIVED from what
`RewriteFixture` itself ungated (dropping a SidecarToolbox's `secretInputs`, a
gate no fixture can satisfy) and never hand-written, so it cannot outlive the
rewrite that earned it. Nothing still derives `expect.toolOffered`: what the run
was offered is a fact, but which tool MATTERED to a scenario is a claim, and a
capture that picked one would be inventing a claim nobody made — the same
judgement `systemPromptContains` declines to guess.

**Capture has real limits, and a reader deciding whether to reach for it needs
them, not a sales pitch — including the limits of the measurement itself.** Of
**eleven** bronze bundles probed — a hand-picked sample of the **37**, ten of
them plan-gate or slot scenarios, which is the shape the capture was tuned
against — **ten emit without a hard finding**. Whole families were never probed
at all (`credential-halt-*`, `sandbox-*`, `git-repo-url-*`,
`completion-requirement-*`), so the ratio is not a rate over the suite.

**The survey measured EMISSION, not replay.** Of those ten, **two have been
captured and replayed end to end**: the round trip in `test/e2e/steelthread`,
over `plangate-external-covered-by-approved-slot`, and a refusal scenario
(`slot-permission-isolation`) replayed by a throwaway probe rather than a
committed test — the latter blocked from being one only by the fixture artifact
below. Seven more emit clean and are **untested at replay**; the tenth emits
with a warning and would then HANG, because its `tool_approval` has an
interaction category recorded nowhere, so the replay sits on the approval prompt
until the class budget expires rather than failing outright.

Nine of the ten are clean only after discounting a test-only fixture artifact,
and that artifact is a property of the **fixture tree**, not of any bundle
count: 56 files under `test/` set dummy Secret values to the literal strings the
capture substitutes as placeholders, which the leak scan correctly flags. A real
cluster credential cannot collide with a placeholder, so it does not happen
against a live session.

The one refusal left is a transcript turn `Fold` has no rule for
(`unmapped-turn`).

**A refused tool call is now a capturable session, and that reframing is what
moved the number.** `pkg/agent/runner/loop_dispatch.go` states it plainly: a
denied tool never reaches the sandbox or MCP server. So an error tool result is
one of two things. A GATE refusal — an authz denial, a plan-gate refusal, a
human's "no" — never ran, and the replay's own gate regenerates it from the
fixture's derived seed and gate configuration, so the bundle cans nothing and
the capture says nothing. An UPSTREAM failure DID reach the server, and
`bt.Bundle.ToolErrors` expresses it (registered through `MCPStub.OnToolError`).
`steelthread.gateRefused` tells them apart, from two per-call records that PROVE
a pre-dispatch refusal — a denied approval outcome, or a denied authz decision
whose recorded message is exactly the bytes the model was handed — and **fails
closed** for anything else — and failing closed means emitting NOTHING for that
call, not canning it as an upstream error. A canned entry there would carry the
gate's own refusal text, which is the same text the step's
`lastToolResultContains` was derived from, so a gate that STOPPED REFUSING would
reach the stub, be served that text back, and satisfy the assertion meant to
catch it — the bundle would pass with the permission boundary broken. With
nothing registered, that regression fails the step by name instead.
`unproven-error-origin` warns so the human knows an error result went
unrepresented.

What still cannot be expressed is one tool that did not fail the same way on
every call (`tool-error-not-uniform`) — `OnToolError` is keyed by tool NAME,
wins over every canned result, and is not counted.

A green `mage test:unit` (or `go test ./...`) is **NOT** sufficient and gives
false confidence. The integration and e2e tests are gated behind
`//go:build integration` and `//go:build e2e` — `go test ./...` and
`mage test:unit` **silently skip them** (they don't compile those files at all).
A change can leave the entire e2e suite red — or not even compiling — and the
default `go test` run stays green.

This is not hypothetical: the owner-derived approver refactor merged to `master`
with the whole e2e suite broken (stale `started_by` model, a `pipeline.Authz`
stub missing new methods, approval scenarios that no longer had a valid
approver) because only the unit suite was run. Fixing it after the fact cost far
more than running `mage test:e2e` once would have.

**Rules:**

- Before merging a branch, run **all three** targets and confirm each exits 0.
  Quote the result; don't assume.
- When you add a method to an interface that test fakes implement (e.g.
  `pipeline.Authz`, `authz.ApproverChecker`), the build-tagged fakes that
  implement it are in `*_test.go` files `go test ./...` never compiles —
  `mage test:integration` / `mage test:e2e` are the only thing that catches the
  missing method. Run them.
- When you change authorization/schema semantics (the SpiceDB schema, the
  approver model, an `interact`/`owner`/`view` chain), the e2e and integration
  suites are where the behavioral fallout surfaces. A unit-green refactor that
  touches authz is not done until `mage test:integration` and `mage test:e2e`
  are also green.
- Integration/e2e are heavier (envtest, a containerized SpiceDB via
  `test/testspicedb`, the in-process harness) and need Docker. They flake under
  contention far more readily than unit tests — treat a flake as a signal to fix
  the test-infra timeout (see `test/testspicedb`), not to skip the suite.
- **`test:integration` and `test:e2e` serialize automatically** via an advisory
  file lock in the clone's common git dir (`test/suitelock`, wired through
  `withSuiteLock`). Both stand up envtest control planes and SpiceDB containers,
  which are machine-wide, so concurrent runs from different worktrees do not
  take turns — they starve each other. A second run prints who holds the lock
  and waits. `AP_TEST_NO_LOCK=1` opts out for CI, where a run already owns its
  machine.
- **`AP_TEST_TIMEOUT_SCALE` widens the browser tests' websocket read deadlines
  on a box the built-in factors do not cover** — set it to a positive multiplier
  (`AP_TEST_TIMEOUT_SCALE=4 mage test:unit`) and `pkg/web/webui/internal/wstest`
  lengthens every positive-path read by that much, on top of the 3x it already
  applies under `-race`.
- **A moving set of failures is contention, not a regression.** When a suite
  fails, note _which_ tests failed. A real break fails the same tests every
  time; contention fails a different set each run, always timeout-shaped
  (`no X within 30s`, `presence != false within 45s`). Three consecutive e2e
  runs of one green tree once failed on three disjoint sets, every one passing
  alone in a fraction of the runtime. **Re-run a suspected flake in isolation**
  — `go test -tags=e2e -run '^TestName$' ./its/package/` — and say you did.
  Never assert "probably flaky" without that, and never chase a moving set as if
  it were real.
- **Then check the BOX, because neither mechanism defends against it.**
  `pkg/testparallel` bounds the suite's own concurrency and `suitelock` bounds
  other worktrees; **unrelated processes on the same machine are outside both**,
  and they reproduce the moving-set symptom exactly. One tree went 46/0 clean,
  then produced two more runs failing on two _disjoint_ pairs — every failure
  passing alone at 3-5x the speed it failed at — with `uptime` showing load
  13-19 and four other agent sessions plus a browser and a VM on the box.
  Ordering made it look like a regression (the clean run predated the change),
  and it was not. `uptime` and `ps aux | sort -k3 -rn | head` take five seconds
  and are the cheapest way to tell "my change broke this" from "this machine is
  full". Sequence: suite parallelism → other worktrees → machine load. **A clean
  full run is not obtainable on a loaded box** — say that plainly rather than
  reporting a green you did not get.

## Test conventions: testify + table-driven, where it makes sense

We use `github.com/stretchr/testify` (already in `go.mod`) with a **`require` +
`assert` mix** and prefer **table-driven tests** when multiple cases share setup
and differ only in input/expected. The goal is fewer characters per assertion,
clearer intent, and one failure message per fact — not maximal terseness.

### Tests MUST NOT reference `examples/` — not files, not names

**No test may depend on anything under `examples/`.** `examples/` is user-facing
demo material: bundles, manifests, and agent names that exist to be read,
copied, and edited by users. A test that reads an example file
(`oap.FromFolder("../../examples/pm-agent")`) or hardcodes an example's name
(`"reviewbot"`, `"pm-agent"`) couples the two — editing a demo to improve it
then breaks unrelated tests, and the demo can no longer evolve freely. This
includes test files placed _inside_ `examples/`; do not put `*_test.go` there at
all.

Tests own their fixtures:

- Need a bundle/manifest on disk? Use the shared `test/oaptest` fixture
  (`oaptest.WriteBundle(t)` materializes a valid `.oap` folder into a temp dir)
  or a package-local `testdata/`.
- Need an agent/class/channel name? Use a made-up fixture name (`"demo-agent"`,
  `"test-agent"`) — never an example's name.

The grep `grep -rn "examples/" --include="*_test.go"` must return nothing.

### When to use `require` vs `assert`

- **`require`** — fatal precondition for the rest of the test. If this fails,
  the test cannot meaningfully continue: scheme builds, fixture creation, the
  `Get` that loads the object you're about to inspect, the `Reconcile` call that
  produces the state under test.
- **`assert`** — claims about the state under test that don't gate later
  assertions. Each `assert.*` is a single fact about the result; multiple
  `assert.*` in the same test mean "I want all these failures reported together
  so I can see the full picture, not just the first."

```go
// good — preconditions abort early, then collect facts about the result
res, err := r.Reconcile(ctx, req)
require.NoError(t, err, "Reconcile must succeed for this case")

var got AgentIdentity
require.NoError(t, c.Get(ctx, key, &got), "Get after Reconcile")
assert.Equal(t, metav1.ConditionTrue, conditionStatus(&got))
assert.Equal(t, expectedReason, conditionReason(&got))
assert.Equal(t, expectedRequeue, res.RequeueAfter)

// bad — uses require where assert is right; first wrong field aborts
// and you never see the others
require.Equal(t, metav1.ConditionTrue, conditionStatus(&got))
require.Equal(t, expectedReason, conditionReason(&got))
```

### When to make a test table-driven

Convert sibling tests to a single table-driven test when:

1. They share **setup shape** (same scheme, same client builder, same
   reconciler/struct under test).
2. They differ only in **input** (an HTTP handler, an object's spec field, a
   credential type) and **expected outcome** (an error, a status condition, a
   return value).
3. The variation is naturally **enumerable** — "all the ways this can fail"
   rather than "every possible interaction."

```go
// Each case sets a different server handler; the rest of the test is
// the same fixture build + reconcile + check.
cases := []struct {
    name    string
    handler http.HandlerFunc
    check   func(t *testing.T, c client.Client, res reconcile.Result)
}{
    {name: "happy refresh", handler: happyHandler, check: ...},
    {name: "5xx failure",   handler: serverErr,    check: ...},
}
for _, tc := range cases {
    t.Run(tc.name, func(t *testing.T) {
        srv := httptest.NewServer(tc.handler); t.Cleanup(srv.Close)
        installRefreshClient(t, srv)
        // ... shared setup ...
        tc.check(t, c, reconcileOnce(t, r, "ai"))
    })
}
```

**Don't force it.** Some tests aren't a fit and become harder to read as a
table:

- A single end-to-end integration test (envtest, smoke) with one scenario. Keep
  it linear.
- Tests whose setup is genuinely different per case — different resource shapes,
  different mock servers per case in a way that doesn't reduce to one parameter.
  A 6-field struct with most fields unused per row is a smell.
- Tests where the assertions are wildly different per case (`check func` taking
  many params is a sign you've lost shared shape).

When in doubt, write the cases as separate `t.Run` subtests sharing local
helpers; if a clean common shape emerges, then collapse to a table. **Two tests
is not a table.** Three is sometimes a table. Four or more with shared shape
almost always is.

### Subtest naming

Subtest names should describe the **condition under test plus the expected
outcome**, not the input alone. Read as one line of documentation when scanning
test output.

```go
// good — reads as "5xx failure: Refresh=False/TokenEndpointError, RequeueAfter > 0"
{name: "5xx failure: Refresh=False/TokenEndpointError, RequeueAfter > 0"}

// bad — no signal about what's expected
{name: "server returns 500"}
```

### Helpers belong above the tests, not duplicated inline

Common factories (`buildOAuthAI`, `newReconciler`, `installRefreshClient`) live
near the top of the file. Each takes `t *testing.T` and calls `t.Helper()` so
failure lines point to the call site, not the helper body. Prefer **small,
well-named factory functions over many-field config structs** when there are
only 2–3 variation points.

### What stays the same

- `t.Helper()` on test helpers — keeps failure messages pointing to the caller.
- `t.Cleanup(...)` for tear-down — runs even on `t.Fatal`.
- `t.Parallel()` — still optional per test; testify works fine alongside it, but
  be careful with package-global state (e.g., `refresh.SetHTTPClient` mutates a
  package-level client; tests using it must NOT run parallel with each other).
- `eventually(t, timeout, fn)` for envtest async polling — stdlib-style poll
  loops are clearer than testify's `Eventually` for the controller-cache
  convergence pattern this project uses.

### "Where it makes sense" — the gating function

Not every test should be table-driven. Not every `if err != nil` needs testify.
The conversion is a judgment call, not a sweep-rule. The question to ask per
file: **does this make the test easier to read, easier to extend with a new
case, and easier to diagnose when it fails?** If yes, convert. If the answer is
"marginally" — leave it alone. The cost of a no-value refactor is real (review
burden, merge conflict risk, lost git blame context). Pick the high-value cases
first.

## Auditing: the periodic multi-lens code-review pass

An **audit pass** is a re-runnable, subagent-driven code review that sweeps a
target through ten independent lenses, verifies each finding adversarially, and
writes a dated, severity-ranked report. It is a health check you invoke on
demand — not part of the ship gate.

This is distinct from the tamper-evident **audit _log_** (§Tamper-evident audit
log): here "audit" means _review the code_, not _the append-only ledger_.

### What a pass does

1. **Scope the target.** Default is the **whole repo** (`pkg/` + `cmd/`). Narrow
   it when asked: "recent changes" → `git diff master...HEAD`; one or more named
   packages → just those. The whole-repo pass is expensive by design — it
   partitions the 330+ leaf packages into subsystem groups and fans out — so use
   the narrowed forms for routine work and reserve the full sweep for a periodic
   health check.
2. **Fan out the ten lenses.** Each lens is one subagent persona, dispatched in
   parallel (per subsystem group, for a whole-repo pass). Each returns
   _structured_ findings only — severity (blocker/major/minor/nit), a
   `file:line`, a one-line defect, and a concrete fix.
3. **Verify before reporting.** Every raw finding is a _candidate_, not a
   conclusion. Hand each to a fresh skeptic subagent charged to **refute** it
   against the current code (default to "refuted" when it can't be reconfirmed).
   Drop refuted findings; keep survivors with a confidence note. This mirrors
   the OWASP generator's "treat claims as untrusted, attempt to refute"
   discipline, and it is what keeps the report free of plausible-but-wrong
   noise.
4. **Synthesize.** Dedupe overlapping findings across lenses (real bugs surface
   under several lenses at once — that convergence is signal), rank blocker→nit
   within each lens, and write the report.

### The ten lenses

The canonical list lives in code — `pkg/gen/auditgen/lenses.go`
(`auditgen.Lenses`) — the single source of truth the prompt and this section
both render. A **new lens is added there as a row**, never bolted onto a
consumer, same registry-over-branching ethos as everything else.

| Lens                              | Charge (persona)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| --------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Security**                      | _senior security engineer._ Authz gaps and fail-open paths, secret/token leakage, injection/SSRF, capability leaks, typed-nil security gates, priv-esc, browser XSS. Anchored to repo patterns: capability-in-context, fail-closed choke points, live token verification.                                                                                                                                                                                                                                                               |
| **DRY**                           | _engineer allergic to copy-paste._ Logic duplicated across packages that should be factored onto a shared abstraction/registry; `if kind == "x"` that belongs as an interface method. Force variants onto the abstraction, not a new helper.                                                                                                                                                                                                                                                                                            |
| **Readability**                   | _reviewer optimizing for the next reader._ Opaque control flow, unclear names, missing _why_-comments, code in the wrong file/package, files grown too large. Offer a concrete relocation/rename/comment.                                                                                                                                                                                                                                                                                                                               |
| **Performance**                   | _performance engineer._ N+1 / O(n²), avoidable allocations in hot paths, locks held across I/O, redundant per-turn/per-reconcile work, unbounded growth, frontend re-render storms. Argue the hot path.                                                                                                                                                                                                                                                                                                                                 |
| **Correctness**                   | _reviewer hunting logic bugs._ Logic that contradicts intent, silently-dropped errors, nil-interface/nil-map panics, SSA idempotency violations, off-by-one, stale-closure updates. Each finding needs a concrete failure scenario.                                                                                                                                                                                                                                                                                                     |
| **Testing / coverage**            | _test engineer who distrusts untested code._ Untested error paths, assertions that assert nothing, table-driven opportunities, logic only reachable through the build-tagged integration/e2e suites that a refactor could silently break. Grep the `*_test.go` before claiming untested.                                                                                                                                                                                                                                                |
| **Conventions / repo-idioms**     | _the maintainer enforcing AGENTS.md._ Violations of the rulebook: no-silent-errors, SSA idempotency (observations in status), typed-nil guards, registry-not-`if kind==`, single-impl interfaces kept, no real names, test style. Name the rule each finding breaks.                                                                                                                                                                                                                                                                    |
| **Concurrency / races**           | _concurrency reviewer._ Data races on shared maps/slices/fields, goroutine leaks, deadlocks and lock-ordering, locks held across a channel send/I/O, missing context cancellation, close races, frontend set-state-after-unmount. Each finding needs a concrete interleaving.                                                                                                                                                                                                                                                           |
| **Durability / restart survival** | _reviewer who assumes every process is about to be killed._ In-memory state whose loss on restart is **silent and unrecoverable** — a cache that is the only record of something a user is waiting on, a process-scoped table never rehydrated from the durable source it came from, an in-memory-only dedup marker. The bar is the failure, not the storage: state that degrades gracefully or is ephemeral by design is correct and is not a finding. Measure against the repo's own pattern — cache in front, durable record behind. |
| **State management**              | _reviewer who treats mutable state as guilty until proven safe._ Multiple sources of truth for one fact, state stored that could be derived (and drifts), mirrored state across layers that desyncs, diffuse mutation ownership, ambiguous state machines. Weight findings toward simplifications that remove whole bug classes — lift state up, collapse two stores into one, replace stored-and-synced with derived.                                                                                                                  |

### Output

A severity-ranked report at `docs/audits/YYYY-MM-DD-<target>-audit.md`
(`<target>` = `all`, `recent`, or the slugified package path, e.g.
`pkg-web-webui-chat`), plus an inline summary. One section per lens in the order
above, ranked blocker→nit, each finding citing `file:line` and its fix, closing
with a "Themes" paragraph naming the cross-cutting patterns worth fixing first.
The report is **advisory and generated** — like the OWASP map it is not a
certification — and it is **not auto-committed**; leave it for a human to review
and commit or discard.

### Invocation

Same shell-to-`claude` shape as `mage docs:*` — a `pkg/gen/auditgen` `Generator`
with injected Run/Diff funcs over the shared `pkg/gen/claudeexec` plumbing (the
headless-claude invocation contract both generators share; do not re-hand-roll
it):

```bash
mage audit:all              # whole repo (pkg/ + internal/ + cmd/)
mage audit:recent           # git diff master...HEAD (no-op if empty)
mage audit:pkg pkg/memory   # a named package/dir
```

Env overrides: `AUDIT_DRY_RUN=1` prints the prompt/command without invoking
Claude; `AUDIT_CLAUDE_MODEL` (default `opus`) and `AUDIT_DIFF_BASE` (default
`master`) override the model and the "recent" diff base. Or trigger a pass in
plain language — "run an audit on `pkg/memory`" — following the steps above.

### Rules of thumb

- **Grounded, not speculative.** Every finding cites a `file:line` verified
  against the _current_ tree; drop any you cannot reconfirm. The verify pass
  exists to enforce this — prefer few high-confidence findings over an
  exhaustive list of maybes.
- **Review-only.** The pass makes _no_ code edits and _no_ git writes; the only
  file it writes is the report (mirrors `mage docs:*`, which regenerate files
  but make no git writes).
- **Convergence is signal.** When several lenses independently land on the same
  code, that is the highest-confidence class of finding — call it out in the
  report's Themes and fix it first.
- **A new lens is a row in `auditgen.Lenses`**, plus its rendering in the table
  above — not a special case in any consumer.
