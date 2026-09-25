# `pkg/web/admind/config/projectors`

Every concrete config projector for the admin console's Config tab. A
**projector** maps one URL slug to a flattening of one or more CRDs into either
a list of rows or a tabbed detail page.

Both interfaces live in the parent, [`config`](..). Nothing here defines an
interface; every concrete type is unexported and registers itself from `init()`.
The parent is blank-imported by `admind.go`, which is what runs those `init()`s.

## The eight slugs

Eight slugs cover **thirteen CRD types** — several slugs fold more than one.

| Slug | CRDs | Primary condition → status word |
| ---- | ---- | ------------------------------- |
| `agents` | AgentClass | Valid |
| `tools` | MCPServer, SidecarToolbox, SpiceboxToolkit, SpiceboxToolspec | Reachable / folded Valid+Reachable / Valid / Valid |
| `skills` | Skill, ClusterSkill | Valid |
| `sources` | SkillSource, ClusterSkillSource | Ready |
| `channels` | Channel | Connected |
| `identities` | AgentIdentity | Valid |
| `users` | UserIdentity | Valid |
| `providers` | ClusterIdentityProvider | Valid |

The same eight slugs have detail projectors: `agentdetail.go` and
`tooldetail.go` register one each, and `configdetail.go` registers the remaining
six in a single `init()`.

`tooldetail.go` dispatches by which of the four CRDs exists at the id — a
namespaced id probes MCPServer and SidecarToolbox only, a bare id probes Toolkit
and Toolspec only.

## Support files

`status.go` (`projectStatus`, `reasonOrMessage`, `editCmd`, `shortSHA`) and
`detail.go` (`getObj`, the `fld*` field builders, the `*Section` builders)
register nothing.

**An unknown status condition projects to `"Unknown"`, never to the ok word.**

## Invariants that must stay in sync

1. **Slug ↔ the UI's column schema.** `pkg/web/adminui/ui/config/columns.ts`
   maps each slug to columns that reference **badge keys and count labels by
   string**, and names this package as its source of truth. **Renaming a badge
   key silently blanks a column — nothing in Go or TypeScript enforces it.**
2. **`config.Link.Entity` ↔ the UI's `EntityKind` union** (ten singular slugs in
   `adminui/ui/lib/router.ts`). Drift here is silent but safe: the renderer
   degrades an unrecognized slug to plain text rather than a dead route.
3. **`settings` is not a projector.** `GET /admin/v1/config/settings` is a
   bespoke handler in the parent, relying on ServeMux dispatching the exact path
   ahead of the `/config/{resource}` wildcard. Registering a `"settings"`
   projector would be dead code.
4. **A duplicate slug panics at process start**, and a test asserts it.
5. **`rows == nil` is coerced to `[]` by the handler**, because several
   projectors build with `var rows []ResourceRow` and a nil slice would marshal
   to `null` and throw client-side.

## RBAC coupling

The operator's ClusterRole carries `create` and `patch` on the agent-facing CRDs
beyond what any reconciler needs, **because of `admind/oapinstall.go`** — the
RBAC sufficiency test names that file as the reason. Narrowing those verbs
breaks agent install from the console, not any controller.

## A stale doc comment

`status.go`'s package doc says the package holds "the per-CRD `config.Projector`
implementations" and that "each file registers one projector via `init()`".
Neither half is true now: `configdetail.go` registers six in one `init()`,
`detail.go` and `status.go` register none, and the doc never mentions
`DetailProjector` at all — which is roughly half the package. Prefer this README
over that comment.
