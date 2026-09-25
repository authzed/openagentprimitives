# `pkg/authz/guardian`

The SpiceDB-facing machinery behind tool approval: composing the schema an
AgentClass's declared permissions require, writing the grant tuples an approval
produces, and orchestrating the pause while a human decides.

This directory holds no code of its own — everything is in a subpackage.

| Package | What it does |
| ------- | ------------ |
| [`schema`](schema/) | Composes the `agentsession` definition's grant-relation / check-permission block from the union of per-AgentClass `(resourceType, permission)` requirements, and validates/partitions the per-MCPServer schema fragments. |
| [`grants`](grants/) | Writes, deletes, and checks the per-`(session, tool, args)` grant tuples. |
| [`approval`](approval/) | The per-runner-process orchestrator for approval pauses, plus the info-leakage grant writer. |
| [`leakage`](leakage/) | A leaf package holding only the types the runner's info-leakage gate and the `respond_to_user` meta tool both need — kept separate purely to break an import cycle between `pkg/agent/runner` and `pkg/agent/tool/meta`. |

## The grant tuple shape

[`grants`](grants/) writes:

```
agentsession:<ns/name>#grant_<perm>_<resType>@<resType>:<resID>
  caveat: check_hash, ctx: {allowed_arguments_hash: <keyed hmac-sha256>}
  expires_at: now + ttl
```

Three things are load-bearing:

- **The args hash is keyed, not a bare digest.** `ArgsHash` /
  `ArgsHashFiltered` are HMAC-SHA256 under a per-session key, so an approval
  granted for one argument set cannot be replayed against another, and the hash
  cannot be precomputed off-session.
- **Every grant carries an expiration.** The schema declares the relation `with
  check_hash and expiration`, so an indefinite tuple is not expressible.
  `TTL == 0` means "session-wide" and gets the `DefaultSessionGrantTTL` backstop
  (7 days) — long enough to outlast any reasonable session; `TTL > 0` (used for
  external-effect tools) stamps the shorter caller-supplied window. A grant
  re-issued after expiry simply goes through the approval flow again.
- **`GrantKey` deliberately excludes the args hash.** A delete removes the grant
  for that `(session, permission, resourceType, resourceID)` regardless of which
  arguments it was approved for.

## Approval blocks a goroutine, not a process

`approval.Orchestrator.Await` is called by the dispatch goroutine that detected
"needs approval": it optionally publishes the approval envelope and blocks on a
per-request channel. The runner's NATS subscriber calls `DeliverDecision` when
the matching applied-envelope arrives, and `Await` returns. The orchestrator is
per-runner-process state — it is the rendezvous, not the record.

Writes here have no fail-open/fail-closed axis: an error from
`DeleteRelationships` means the grant **may still be live**, so a revoke must be
surfaced rather than reported as done.
