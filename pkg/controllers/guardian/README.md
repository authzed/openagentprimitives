# `guardian` — composing per-class authz into the SpiceDB schema

Two operator-side concerns live here: composing per-AgentClass authz
requirements (`AgentSessionGrants` CRs) into the SpiceDB `agentsession`
definition, and syncing `SpiceDBBootstrap` relationship tuples.

| File | Role |
| ---- | ---- |
| `agentsessiongrants_controller.go` | The `AgentSessionGrants` reconciler — the schema composition itself |
| `bootstrap_sync.go` | Wires `SpiceDBBootstrap` relationship sync into the reconciler |
| `bootstrap_canonicalize.go` | `ResolveTuple` — a bootstrap relationship → the `spicedb.Tuple` the writer accepts |
| `bootstrap_refcount.go` | `Owner` — which CRs claim a tuple, so a tuple survives until its last claimant is gone |
| `bootstrap_validation.go` | Bootstrap spec validation |
| `mcpserver_fragment_validation.go` | Validates an MCPServer's contributed schema fragment |

## Non-obvious constraints

- **Canonicalize before writing to SpiceDB.** A raw email or URL used directly
  as an object ID does not round-trip. `ResolveTuple` is the one place a
  bootstrap relationship becomes a tuple — go through it.
- **Tuples are reference-counted, not owned.** Two `SpiceDBBootstrap` CRs may
  claim the same tuple; deleting one must not delete the tuple while the other
  still claims it. That is what `bootstrap_refcount.go` is for.
- **Do not restamp observation timestamps unconditionally.** This controller
  shipped a `~5s-forever` reconcile storm by writing a fresh
  `ObservedSchemaWrittenAt` on every pass instead of only on the False→True
  transition. Use `reconcile.StampIfMoved`;
  [`../testenv/idempotency`](../testenv/idempotency/) is the CI gate.
