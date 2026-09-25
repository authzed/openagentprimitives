# `pkg/authz/guardian/schema`

Composes the live SpiceDB schema. Onto the code-owned scaffold in
[`../../spicedb/schema`](../../spicedb/schema/) it merges:

1. **`AgentSessionGrants` CRs** contribute `(resourceType, permission)` pairs.
   Each becomes a `grant_<perm>_<resType>` relation on the `agentsession`
   definition (declared `with check_hash and expiration`) plus a matching
   `check_<perm>_<resType>` permission arrowing into the resource's permission
   of the same name.
2. **Four CR-sourced fragment kinds** — `MCPServer`, `SidecarToolbox`,
   `SpiceboxToolkit` and `SpiceDBBootstrap` — each contribute
   `spec.spiceDBSchema` resource definitions. All four are tenant/operator
   input gathered cluster-wide at reconcile time, and all four go through the
   SAME two-stage isolation before reaching `RunAll`: `ValidateFragment` (bad
   on its own) then `PartitionCompatibleFragments` (valid alone, conflicts
   with another accepted fragment). `SpiceDBBootstrap` is the one
   operator-authored kind (`FragmentTier`); the other three are
   tenant-authored.
3. **The compile-time set** — the scaffold plus registered channel-kind
   fragments plus the `//go:embed`'ed built-in toolkits (`toolkits/*.yaml`) —
   is a *different* axis from (2): it ships inside the binary, is composed by
   `ComposeBase` (`base.go`), and is deliberately NOT partitioned. There is no
   CR to carry a rejection, so a compile-time fragment that fails to compose
   is a build bug, not a cluster problem, and errors loudly instead.

| File | Owns |
| ---- | ---- |
| [`types.go`](types.go) | `GrantPair` and its `RelationName()` / `PermissionName()` derivations — the single place the naming convention is defined. |
| [`composer.go`](composer.go) | `Compose` / `ComposeWithSkipped` / `ComposeAll`, and `Run` / `RunAll` over the `SchemaIO` seam. |
| [`base.go`](base.go) | `ComposeBase` — the compile-time half (scaffold + channel-kind fragments + embedded built-in toolkits), deliberately NOT partitioned; see (3) above. |
| [`spicedb_schema.go`](spicedb_schema.go) | `EmitSpicedbSchema` — concatenates schema fragments from all four CR-sourced kinds into one block of definitions. |
| [`validate_fragment.go`](validate_fragment.go) | `ValidateFragment` and `ReservedDefinitionNames`. |
| [`partition.go`](partition.go) | `PartitionCompatibleFragments` — the greedy maximal conflict-free subset, ordered by trust tier then key. |
| [`references.go`](references.go) | `UnresolvedReferences` — reports (does not gate) a permission expression naming a relation/permission its definition does not declare; the one in-process check beyond `compiler.Compile`'s parse. |

## Invariants

- **`Compose` is pure — no I/O.** The read-from-SpiceDB → compose →
  write-to-SpiceDB orchestration lives in the guardian controller. Keep it that
  way; it is what makes the composition testable without a backend.
- **Existing relations and permissions on `agentsession` are preserved.** A pair
  whose `ResourceType` or `Permission` is not present in the live schema is
  *skipped*, not invented — use `ComposeWithSkipped` to see the skip list.
- **Reserved names are derived, never transcribed.** `ReservedDefinitionNames`
  parses `schema.zed` on first call rather than hardcoding the list, so a
  definition added to the scaffold is automatically reserved and cannot drift.
  Slots use the same derived list (`ReservedDefinitionNames`, not a
  hand-maintained set), so a slot naming a reserved scaffold type is skipped
  and reported the same way an undeclared type is.
- **`EmitSpicedbSchema` must not emit `definition user {}`.** That lives in the
  scaffold, which the composer concatenates *before* this output; emitting it
  here would double-declare it. A fragment is rejected if it declares `user`
  itself, regardless of which of the four CR kinds it came from.
- **A conflicting fragment is isolated, not fatal.** `PartitionCompatibleFragments`
  assumes each fragment already passed `ValidateFragment` alone, and exists only
  to find fragments that conflict with *each other*. Rejections carry the
  contributor key, the compose error, and (`RejectedFragment.DisplacedBy`) the
  key of the already-accepted fragment that won the conflict, so the operator
  can report **which** contributor was excluded, **why**, and **by what** —
  never drop one silently.
- **The partition orders by trust tier first, key second.** Candidates sort by
  `FragmentTier` (higher trust first — `TierOperator` ahead of `TierTenant`),
  and only *within* one tier does the existing lexicographic key order decide
  a tie. This is the one place a cold reader needs to learn the tier rule
  without reading `partition.go`'s own comment: an earlier-sorting
  tenant-authored fragment never displaces a later-sorting operator-authored
  one on key alone.
