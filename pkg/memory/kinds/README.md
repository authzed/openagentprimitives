# `kinds` — the registered memory Kinds

32 Kinds, one package each. A Kind declares its name, ID prefix, retention,
content schema, indexed fields, write authority, and optional `ScopeHooks`.
Registration is `init()`-time via `memory.RegisterKind`.

**Every kind package exports a `KindName` constant.** Use it — never spell a
kind name as a string literal at a call site.

| Package                                              | `KindName`               | Purpose                                                                                                  | Append-only |
| ---------------------------------------------------- | ------------------------ | -------------------------------------------------------------------------------------------------------- | ----------- |
| [`approval/`](approval/)                             | `approval`               | Approval-flow request + outcome events, linked to their tool call by `toolUseID`                         | **yes**     |
| [`artifact/`](artifact/)                             | `artifact`               | Mutable head per logical artifact: name, renderer kind, revision count, movable tag refs                 | no          |
| [`artifactrevision/`](artifactrevision/)             | `artifact_revision`      | One immutable rendered revision of an artifact                                                           | no          |
| [`authz_session_config/`](authz_session_config/)     | `authz_session_config`   | Per-session authz config snapshot; authzd's cold-start policy and extraction worker read it              | no          |
| [`authzdecision/`](authzdecision/)                   | `authz_decision`         | Every `authz.Check` outcome, linked to resource + subject                                                | **yes**     |
| [`channel_msg_ref/`](channel_msg_ref/)               | `channel_msg_ref`        | Sparse index: a kind-scoped message reference → the turn index it landed at                              | no          |
| [`coldstarttask/`](coldstarttask/)                   | `cold_start_task`        | The approver's decision on a new session's cold-start scope proposal                                     | no          |
| [`contentguardaudit/`](contentguardaudit/)           | `contentguard_audit`     | Per-call content-inspection findings (pass / block / approve)                                            | **yes**     |
| [`extracted_entity/`](extracted_entity/)             | `extracted_entity`       | Raw LLM-extracted entity candidates from authzd — unchecked                                              | no          |
| [`extraction_state/`](extraction_state/)             | `extraction_state`       | authzd's per-turn extraction lifecycle: pending → complete \| failed                                     | no          |
| [`infoleakageaudit/`](infoleakageaudit/)             | `infoleakage_audit`      | One record per info-leakage gate event                                                                   | **yes**     |
| [`infoleakagedecision/`](infoleakagedecision/)       | `infoleakage_decision`   | Per-session approve/deny of a `(resourceType, resourceID)` for this audience                             | **yes**     |
| [`infoleakagetaint/`](infoleakagetaint/)             | `infoleakage_taint`      | One record per resource read via a tool call — what flowed into this session's context                   | **yes**     |
| [`kgingestion/`](kgingestion/)                       | `kg_ingestion`           | Not a stored record: its `ScopeHooks` reacts to `lifecycle/turn.completed` and calls `KGProvider.Ingest` | no          |
| [`label/`](label/)                                   | `label`                  | `(resourceType, id)` → display label from MCP tool responses. Every entry is `trust:untrusted`           | no          |
| [`lifecycle/`](lifecycle/)                           | `lifecycle`              | The session/scope lifecycle timeline                                                                     | **yes**     |
| [`lineage/`](lineage/)                               | `lineage`                | Bidirectional fork edges — parent ↔ child, two entries per fork                                          | no          |
| [`metaagentaudit/`](metaagentaudit/)                 | `metaagent_audit`        | One entry per `@metaagent` invocation                                                                    | **yes**     |
| [`metaagentthread/`](metaagentthread/)               | `metaagent_thread`       | Messages in the metaagent's sub-thread                                                                   | no          |
| [`parkedprompt/`](parkedprompt/)                     | `parked_prompt`          | Durable record of a render-ready prompt a session is parked on                                           | no          |
| [`preferenceaccess/`](preferenceaccess/)             | `preference_access`      | One entry per `?user-ref=` subject-named preferences read, resolved or not                               | **yes**     |
| [`preferencewrite/`](preferencewrite/)               | `preference_write`       | One entry per first-party user preference write (e.g., Slack App Home edit) into the user's own scope    | **yes**     |
| [`relwritesaudit/`](relwritesaudit/)                 | `relwrites_audit`        | Tuples `pkg/authz/relwrites` emitted after a successful tool call                                        | **yes**     |
| [`scopeaudit/`](scopeaudit/)                         | `scope_audit`            | One entry per applied `ScopeDelta`                                                                       | **yes**     |
| [`sessionscope/`](sessionscope/)                     | `session_scope`          | The dynamic session scope (Layer 2). One entry per session, ID `scp-config`                              | no          |
| [`tool_dispatch_snapshot/`](tool_dispatch_snapshot/) | `tool_dispatch_snapshot` | A workspace snapshot committed before a `readwrite`/`external` tool dispatch                             | **yes**     |
| [`toolchainaudit/`](toolchainaudit/)                 | `toolchain_audit`        | Toolchain selection, carrying the frozen image digests of what actually ran                              | **yes**     |
| [`toolguardaudit/`](toolguardaudit/)                 | `toolguard_audit`        | Breaker trips/recoveries, rate-limit hits, denials, halts                                                | **yes**     |
| [`toolsession/`](toolsession/)                       | `tool_session`           | One entry per parsed event from an interactive tool's stream                                             | **yes**     |
| [`turn/`](turn/)                                     | `turn`                   | The LLM transcript. ID `turn-<index>-<role>` makes re-append idempotent                                  | **yes**     |
| [`uiaction/`](uiaction/)                             | `ui_action`              | One agent-UI action's lifecycle, rewritten in place as it progresses                                     | no          |
| [`uiviewmodel/`](uiviewmodel/)                       | `ui_view_model`          | Agent-UI Tier-1 view model per `(session, ui, slot)`, rewritten in place                                 | no          |
| [`uiviewparams/`](uiviewparams/)                     | `ui_view_params`         | The agent's binding-parameter choices per `(session, ui)`, rewritten in place                            | no          |

| Support package          | What it holds                                                                      |
| ------------------------ | ---------------------------------------------------------------------------------- |
| [`all/`](all/)           | Blank-imports every Kind above. One import wires the whole framework into a binary |
| [`internal/`](internal/) | Shared accessor boilerplate and the undecodable-entry report                       |

## Non-obvious constraints

- **A Kind omitted from [`all/`](all/) is unregistered in the operator**, which
  serves the memory API and decides whether a `Put` is storable — so every write
  of it is answered `400 unknown Kind`. `parked_prompt` shipped that way once;
  `all_test.go` now fails on any omission.
- **Append-only is not just "do not overwrite".** It means: content-changing
  `Put` returns `ErrAppendOnlyConflict`, per-entry `Delete` is refused, and
  every entry is Ed25519-signed into a per-`(scope, publisher)` hash chain. See
  the
  [group README](../README.md#append-only-kinds-are-ed25519-signed-into-a-hash-chain).
- **Mutable is sometimes the _point_.** `ui_action`, `ui_view_model` and
  `ui_view_params` are rewritten in place by design; append-only would reject
  the second write and let the agent compose a slot exactly once. That is not an
  audit gap — the underlying tool call is already in the tamper-evident
  transcript.
- **`parked_prompt` is deliberately not append-only** either: a resolved prompt
  must be overwritable with its tombstone. It is working state, not evidence.
- **`WriteAuthority()` is an interface method, not a table in a consumer**, so a
  new Kind cannot be registered without answering "which credential class may
  author this?"
- **`IndexedFields()` names JSON tags, not Go field names.** `FieldFilter.Path`
  draws from the same vocabulary; a Go name here is misinformation that
  reappears as a predicate matching nothing.
