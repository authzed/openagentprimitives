# `skillsource` — the SkillSource reconciler

Resolves the (optional) clone credential, fetches the repo at the requested ref,
discovers `SKILL.md` files, caches each skill's bundle by content digest, and
materializes owned `Skill` CRs.

Fetch is abstracted behind `skillfetch.Fetcher` (go-git in production, a fake in
tests); the bundle cache behind `skillbundle.Store`.

| File                  | Role                                                                              |
| --------------------- | --------------------------------------------------------------------------------- |
| `controller.go`       | `Reconcile` + `SetupWithManager`                                                  |
| `discover.go`         | `Discover` — a pure function walking a fetched tree into `DiscoveredSkill` values |
| `sync.go`             | The **scope-agnostic** half of a sync pass, shared with the cluster-scoped mirror |
| `watch.go`            | Dependency watches — `SkillSource.spec.auth` → `AgentIdentity` → `Secret`         |
| `repoinstructions.go` | Caps and sanitizes the repo-instructions text injected into the agent prompt      |

## Non-obvious constraints

- **`sync.go` is called by [`../clusterskillsource`](../clusterskillsource/),
  not copied into it.** The two were byte-identical copies and had already
  drifted user-visibly: the cluster copy accepted an empty auth value the
  namespaced copy rejected, silently downgrading a credentialed clone to an
  anonymous one.
- **Failures surface on `Ready` and are logged — they are not returned.** A bad
  credential or ref requeues on the sync interval instead of crash-looping the
  reconcile.
- **Materialization is level-triggered.** Every pass re-caches the bundles and
  re-upserts the Skills, because CR status is not evidence that either still
  exists. Both writes are idempotent, and the status write is skipped entirely
  when nothing moved — that is what keeps the self-watch quiet.
- **Watching only `SkillSource` is not enough.** The clone credential is two
  hops away, and a change at either hop changes the bytes the next clone uses. A
  rotated PAT would otherwise be invisible until the sync interval elapsed.
- **`repoinstructions` is a prompt-injection surface.** The byte cap bounds
  prompt growth and blunts a hostile or oversized `AGENTS.md`/`CLAUDE.md`.
