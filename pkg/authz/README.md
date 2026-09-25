# `pkg/authz`

Every runtime authorization decision in agentprimitives. Consumers — the runner,
channelsd, the operator, authzd — take the [`engine.Engine`](engine/) interface
rather than calling into this package directly.

## The root is backend-neutral

`pkg/authz` (the top-level files only) is the **vocabulary**: the types a
decision is expressed in and the pure logic over them. It knows nothing about
SpiceDB.

- `go list -deps ./pkg/authz | grep authz/spicedb` → no matches.
- `grep authzed-go pkg/authz/*.go` → no matches.

Every SpiceDB-shaped dependency arrives as a small interface the root declares
and the backend satisfies — `SessionChecker`, `ApproverChecker`, `ForkChecker`,
`ManageScopeChecker`, `Granter`, `Lookuper`, `RelWriter`. A second backend would
implement those interfaces; it would not touch the root.

| Root file                                                                                                                    | Owns                                                                                                                                                                                                       |
| ---------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`authz.go`](authz.go)                                                                                                       | The core vocabulary: `Outcome`, `StateImpact` (`stateless`/`passthrough`/`readonly`/`readwrite`/`external`), `EnforceMode`, `Permission`, `PermissionCheck`, `Inputs`, `Result`, `Relation`, `SessionRef`. |
| [`approve.go`](approve.go)                                                                                                   | The click-time approver gate — see below.                                                                                                                                                                  |
| [`check_session.go`](check_session.go)                                                                                       | `CheckSessionInteract` / `CheckSessionManageScope` / `CheckSessionFork` / `CheckSessionGrant`, each over its own checker interface.                                                                        |
| [`grant.go`](grant.go), [`lookup.go`](lookup.go)                                                                             | `Grant`/`Revoke`/`Touch*` writes and the `Lookup*` enumerations.                                                                                                                                           |
| [`subject.go`](subject.go)                                                                                                   | `ValidateSubject` / `ValidateSubjectSet` — the guard on any site that _assigns_ the identity a session acts as.                                                                                            |
| [`expr.go`](expr.go), [`cel_funcs.go`](cel_funcs.go)                                                                         | The CEL env for `permissionVariants[].when` and `check.resourceIDExpr`, plus the `spicedb_user_id(email)` function.                                                                                        |
| [`template.go`](template.go), [`transforms.go`](transforms.go), [`variants.go`](variants.go), [`prefilter.go`](prefilter.go) | Resource-ID templating, the named transform registry, permission variant selection, and the cheap pre-pass that skips entity extraction.                                                                   |
| [`bind_defaults.go`](bind_defaults.go), [`autofill.go`](autofill.go), [`binding_wait.go`](binding_wait.go)                   | Session entity binding: class defaults, tool-arg autofill, and waiting on extraction.                                                                                                                      |

## Fail-closed is the load-bearing property

Every gate denies rather than admits when it cannot decide. Concretely:

| Failure                                                                                   | Result   | Where                                                                                                                                                                                                                                                                                                           |
| ----------------------------------------------------------------------------------------- | -------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Dependency not wired (nil checker)                                                        | Denied   | [`approve.go`](approve.go) and [`check_session.go`](check_session.go) return `(false, nil)`; `CheckSessionGrant` in [`check_session.go`](check_session.go) returns `Denied` with `"no SessionGrantChecker wired"`. [`engine/engine.go`](engine/engine.go) re-checks `ApproverChecker == nil` before delegating. |
| Backend RPC error                                                                         | Denied   | `CheckSessionGrant` in [`check_session.go`](check_session.go) maps the error onto `Denied` with the error text as `Message` — **an error wins even when the backend also answered `Allowed`.** The `(bool, error)` gates return `(false, err)` and every caller treats non-nil as a denial.                     |
| No SpiceDB client, but the call needs one                                                 | Denied   | [`spicedb/toolcheck/check_tool_call.go`](spicedb/toolcheck/check_tool_call.go) — a `readonly`/`readwrite` `StateImpact` with `Cli == nil` returns an explicit fail-closed deny instead of panicking.                                                                                                            |
| Unrecognized `StateImpact`, or one that requires a `check` with none supplied             | Denied   | [`spicedb/toolcheck/check_tool_call.go`](spicedb/toolcheck/check_tool_call.go) `default:` branch.                                                                                                                                                                                                               |
| Subject type outside the caller's allow-list, or an empty allow-list                      | Rejected | [`subject.go`](subject.go) — an empty `allowed` set rejects everything.                                                                                                                                                                                                                                         |
| Not-yet-implemented gates (`CheckEntityCanBind`, `CheckMCPTrust`, `CheckInformationFlow`) | Denied   | [`engine/engine.go`](engine/engine.go) — they return `Denied` carrying `ErrNotYetMigrated` rather than panicking, so an accidental call fails closed.                                                                                                                                                           |

`Granter` writes have no open/closed axis: an error means the tuple may not
exist and the caller must not proceed as though it does. `Lookuper` methods
enumerate rather than decide, but every caller in this repo treats a failed
enumeration as "nobody qualifies" and stops — acting on a partial member list
would silently widen or narrow who is asked.

## The approver model

Two relations carry standing on a session: `#owner` (conferred by `TouchOwner`)
and `agentsession#approve`. `started_by` does **not** confer approve or fork
standing.

The click-time gate is [`CheckApproverAuthorized`](approve.go), and its rule is
not the obvious one:

- **The request names source resources** → the clicker must be `#owner` on **at
  least one** of them. Any one owner vouches; quorum is 1.
  **`agentsession#approve` is not consulted at all.**
- **The request names no source resources** → `agentsession#approve` alone
  decides.

Do **not** "harden" the first branch into `approve AND owner`. A resource-scoped
approval solicits the _resource owner's_ consent, and that owner usually has no
relationship to the session — the owner resolver writes only the requester as
session owner. Demanding session standing on top of ownership rejects the one
person the prompt was routed to; that has shipped as a production outage,
surfacing as "no one has standing to approve". Session standing alone is not
sufficient either: a session owner must never self-approve access to someone
else's resource. The rationale is written out at the top of
[`approve.go`](approve.go) — read it before changing the function.

Raise-time eligibility (`ResolveApprovers` in [`lookup.go`](lookup.go)) mirrors
this rule. **Change both or neither.**

Across the resource branch, ownership is checked one resource at a time; a check
error is remembered but the loop keeps going, and the first error is surfaced
only if _no_ resource answered yes — so a backend outage can never manufacture
an approval.

## Subpackages

| Package                         | What it does                                                                                                                                                                                                                                                                    |
| ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`engine`](engine/)             | The single `Engine` interface every consumer depends on: `Checker` + `Granter` + `Lookuper` + `BindingLifecycle`, composed over injected dependencies.                                                                                                                          |
| [`spicedb`](spicedb/)           | The SpiceDB backend: wire client, the embedded `.zed` schema, object-id composition, and the tool-call check.                                                                                                                                                                   |
| [`hooks`](hooks/)               | The concrete authz lifecycle hooks the runner registers into its per-turn pipeline, and the code-owned ordering between them.                                                                                                                                                   |
| [`scope`](scope/)               | The structured per-session permission policy document (dynamic session scope) — pure data types, no external deps.                                                                                                                                                              |
| [`guardian`](guardian/)         | Operator-side SpiceDB machinery: schema composition, tool-approval grant tuples, the approval orchestrator, info-leakage types.                                                                                                                                                 |
| [`toolguard`](toolguard/)       | Per-tool circuit breakers and rate limits, enforced as pipeline hooks.                                                                                                                                                                                                          |
| [`contentguard`](contentguard/) | The pluggable content-inspection seam over tool I/O (prompt-injection detection, URL allowlisting).                                                                                                                                                                             |
| [`pinning`](pinning/)           | The dependency-pinning abstraction: pin strength, frozen identity, drift detection, per-kind.                                                                                                                                                                                   |
| [`revocation`](revocation/)     | In-flight revocation — the operator publishes, each runner subscribes and invalidates.                                                                                                                                                                                          |
| [`validator`](validator/)       | Shared decision/trace types the CLI-argv and MCP-args validators both return.                                                                                                                                                                                                   |
| [`extract`](extract/)           | The entity-extraction `Provider` seam used by session binding.                                                                                                                                                                                                                  |
| [`authzd`](authzd/)             | The authzd daemon's own pipeline host — the approval round-trip for cold-start and mid-session `@metaagent` gates.                                                                                                                                                              |
| [`adoptguard`](adoptguard/)     | Gates operator reads of `Secret`/`ConfigMap` to objects it legitimately manages — adopted via a CR reference, or on a fixed-infra allowlist. Any other read is a programming error and **panics by default** (`Panic` mode; a `Warn` mode exists) so overreach is never silent. |
| [`coldstart`](coldstart/)       | Cold-start approval action constants and the `approval.Decision` → action mapping, in an importable package so both authzd and the hook can reach them.                                                                                                                         |
| [`untrusted`](untrusted/)       | The shared tag names for nonce-delimited untrusted-content envelopes, so producer and consumers cannot drift.                                                                                                                                                                   |
| [`relwrites`](relwrites/)       | **Non-production only.** A dev/test `Writer` decorator that rewrites every `user:` subject to one fixed canonical id so one person can play requester and approver. While enabled, who-owns-what is silently false.                                                             |
