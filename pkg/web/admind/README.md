# `pkg/web/admind`

The platform admin console's **backend API**. `Handler()` returns a plain
`*http.ServeMux` of `/admin/v1/*` routes — this is a mux, not a server: no port,
no `ListenAndServe`, no lifecycle of its own.

`internal/cmd/operator` is the only binary that mounts it, onto the operator's
HTTP mux at `/admin/` (`--debug-bind-address`, default `:8082`). The browser
never reaches it directly: [`pkg/web/adminui`](../adminui), running in `webd`,
reverse-proxies `/admin/api/<rest>` → `<admind>/admin/v1/<rest>` and injects the
bearer token and `X-Admin-Subject` server-side.

## The authorization gate

Every route is wrapped in `require(permission, handler)` (`admind.go`), which
checks **two** factors in order:

1. **Service token** — constant-time compare against the `spicebox-admind-token`
   Secret value. This proves the caller is `adminui`, not that a human may act.
2. **SpiceDB platform permission** — the subject comes from the
   `X-Admin-Subject` header (which must be exactly `user:<canonical>`), and the
   check is `CheckPlatformPermission(permission, subject)` against the singleton
   object `platform:platform`.

| Permission | Guards |
| ---------- | ------ |
| `view_sessions` | session list / stream / detail / logs |
| `kill_session` | `DELETE /admin/v1/sessions/{ns}/{name}` |
| `view_live` | toolcalls, approvals |
| `view_audit` | audit query/facets/entities, artifacts, memory, kg |
| `view_overview` | overview, budget, health, cluster |
| `view_config` | access, config settings/resource/detail |
| `install_agent` | `POST /admin/v1/agents/oap-install` |

**Check the per-area permission, never `can_admin` directly** — the schema
(`pkg/authz/spicedb/schema/schema.zed`) states this contract, and admind honors
it even though all seven currently alias to `admin`. That aliasing is what makes
splitting the roles later a schema change rather than a code change.

**One route is per-resource, not platform-wide.** `POST
/admin/v1/credentials/agent-update` uses `requireProvenSubject`, which checks
the token only and passes the canonical subject as an **explicit argument** —
deliberately, so a handler cannot silently forget to establish who is asking.
Its real gate is `CheckAgentIdentityUpdateCredential`, i.e. `update_credential`
on `agentidentity:<ns>/<name>`, resolved **FullyConsistent** and with the target
read from the request's own status rather than from the request body.

## Fail-closed invariants

- **Construction fails closed.** `New` errors unless `Mem`, `K8s`, `Checker`,
  and `Token` are all set — the reason `PlatformChecker` is a single interface
  is that every route's authorization arrives together or admind does not serve
  at all. No token file → admind is never constructed, and webd's admin UI fails
  closed behind it.
- **A SpiceDB error is a 500 — never an allow, never a deny.** A fault is not a
  denial.
- **Optional dependencies degrade, never fail:** no KG → `available:false`; no
  metrics API → `"n/a"`; no clientset → capacity preflight skipped with a
  notice. The operator declares that clientset as the *interface* so a typed-nil
  cannot defeat the `== nil` check.
- **Unpriced models are the deliberate exception to graceful degradation:** they
  yield `NaN`, which marshals as JSON `null`, so the UI shows nothing rather
  than implying `$0`.

## Files

| File | What it holds |
| ---- | ------------- |
| `admind.go` | `Config`/`Admind`/`New`, the `PlatformChecker` seam, the route table, and the `require` middleware |
| `aggregator.go` | In-memory live-session state machine — CRD snapshots overlaid with sub-turn NATS envelopes — plus SSE fan-out |
| `handlers.go` | JSON write helpers and most handlers: sessions, toolcalls, approvals, audit, overview, health, config |
| `access.go` | The Access panel: platform admins, plus a SpiceDB stats block that is currently always `Available:false` |
| `artifacts.go` | ArtifactRender list/detail, grouped into revision "tracks" by the `artifact-id` label |
| `budget.go` | Token and estimated-cost rollups by model, class, session, and user |
| `cluster.go` | Best-effort cluster identity from node `providerID`, memoized for 5 minutes |
| `credentials.go` | `requireProvenSubject` and the agent-credential-update route |
| `kg.go` | Read-only proxy to `memory.KGProvider`, degrading to `available:false` |
| `memorybrowser.go` | Per-kind rollups, `?kind=` drill-in, and `?q=` ranked search over a bounded cross-scope scan |
| `oapinstall.go` | `POST /admin/v1/agents/oap-install` — load an uploaded `.oap` or pulled OCI ref, preflight, resolve questions non-interactively, apply |
| `oapinstall_capacity.go` | Capacity-clamp questions for the install form |
| `settings.go` | Projects the singleton `ClusterAgentSettings` into labeled rows — **bespoke, not a projector** |
| `starters.go` | Indexes each session's `startedBy` subject and `startedAt`, shared by the budget and audit rollups |

## Subpackages

| Package | What it holds |
| ------- | ------------- |
| [`agentcred`](agentcred) | The wire contract (`Path`, `SubjectHeader`, `Request`/`Response`) **and** the HTTP client for the agent-credential-update route, so webd and identityd can call the operator without either side owning both halves. |
| [`audit`](audit) | Normalizes memory entries into one cross-session `Event` shape through a per-memory-kind `Mapper` registry (8 registered), and answers admin queries, facets, and entity rollups. |
| [`config`](config) | The config-projector contract and registries. See its README. |
| [`cost`](cost) | Three-layer per-model price map (`llmpricing` defaults → `ADMIND_PRICE_MAP_PATH` → `ClusterAgentSettings.spec.modelCatalog`) and `Estimate`. |
| [`health`](health) | The Overview cluster-health snapshot: seven first-party workloads, a Graphiti liveness ping, and a best-effort CPU/memory rollup. |
| [`overview`](overview) | The dashboard payload — by-model and by-class token rollups, a 24-bucket hourly series, and denial/tool-call KPIs. TTL-cached. |

**`audit`, `overview`, and `health` deliberately do not import their parent.**
The parent adapts into narrow seams (`classResolver`, `liveSessionSnapshot`)
instead. Keep it that way — it is what stops the rollup packages from acquiring
the whole handler surface as a dependency.

## Memory access is capability-gated

Two handlers wrap the request context in
`memory.WithSystemApproval(ctx, "operator:admind")`. That mints a wildcard
approval satisfying any **non-internal** memory permission, and it can never
satisfy an internal-tier one. It is sound only because those routes are already
gated on `view_audit` / `view_sessions`: `memory.Local.QueryAllScopes` is
admin-only and applies **no** per-entry authorizer post-filter, so its callers
must gate on the SpiceDB platform permission first.
