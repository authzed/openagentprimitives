# `pkg/web/webui/webassets`

The single embed point for every built browser bundle in the repo. One Go file
(`embed.go`) `//go:embed`s `dist/` and exposes two things: `FS()`, the asset
filesystem, and `Manifest()`, which parses `dist/manifest.json` into
`appKey -> Entry{Scripts, CSS}`. Page handlers look up their app by key and emit
the script/stylesheet URLs the manifest names, in load order.

## `dist/` is generated build output — never hand-edit it

`dist/` is committed, but it is **not source**. It is produced by Vite from the
TypeScript workspace at the repo root (`web/`), which writes its `outDir`
directly here so this package can embed it.

```
web/  (pnpm + Vite workspace)  ──  mage web:build  ──▶  pkg/web/webui/webassets/dist/
```

| Command | What it does |
| ------- | ------------ |
| `mage web:build` | `pnpm install --frozen-lockfile` then `pnpm build`, in `web/`. Regenerates `dist/`. |
| `mage web:check` | Runs `web:build`, then `git diff --exit-code -- pkg/web/webui/webassets/dist`. **Fails if the committed `dist/` drifted from source** — the drift guard. |
| `mage web:dev` | Long-running Vite dev server (HMR) for `webd --web-dev`. Does not touch `dist/`. |

Editing a file under `dist/` by hand is always wrong: the next `web:build`
overwrites it, and `web:check` fails in the meantime. Change the TypeScript,
rebuild, and commit `dist/` in the same change.

Bundle filenames are content-hashed (`admin.Cqu30MJ5.js`), so a rebuild renames
files rather than editing them in place. Never hand-maintain `manifest.json` —
it is the map from stable app key to those hashed names. The URL prefix in the
manifest is a fiction: files live directly under `dist/`, and only the URL
carries `/assets/`; there is no `dist/assets/` directory.

Vite builds with `emptyOutDir: true`, so a rebuild replaces the tree wholesale.
The drift guard is `git diff` over tracked paths, which means it catches a
changed or deleted bundle but would not by itself flag a stray *untracked* file
left under `dist/`.

## The entries are not all `pkg/web`'s

Vite discovers entry points by globbing **`pkg/**/ui/**/app.json`** — repo-wide,
not just under `pkg/web`. Each `app.json` declares a `"key"`, and the entry
module is the `index.tsx` beside it. So `dist/` also carries bundles owned by
other groups, and a change there lands here.

| App key source | Entries |
| -------------- | ------- |
| `pkg/web/adminui/ui` | `admin` |
| `pkg/web/webui/artifactview/ui`, `…/ui/host` | `artifact-view`, `artifact-host` |
| `pkg/web/webui/health/ui` | `health` |
| `pkg/web/webui/sessions/ui` | `sessions` |
| `pkg/web/webui/sessionview/ui`, `…/ui/mcpuihost` | `session-view`, `mcpui-host` |
| [`pkg/platform/identityd/ui/*`](../../../platform/identityd/ui) | `identity-link`, `identity-link-form`, `identity-portal`, `identity-verify-warn` |
| *(built in, not globbed)* | `system` — hardcoded in `web/vite.config.ts` to `web/packages/runtime/src/system/index.tsx`, the shared design-system response page |

A `ui/` directory with **no** `app.json` is a component library, not an entry —
it is imported by an entry rather than bundled on its own. `chat/ui` and
`agentui/ui` are both in that category.

`discoverEntries()` throws on an `app.json` with no `"key"`, no sibling
`index.tsx`, or a duplicate key — a malformed entry fails the build rather than
silently vanishing from `dist/`.

`dist/` also holds shared chunks Vite split out (`vendor.*`, `markdown.*`,
`label.*`) and the self-hosted Inter / JetBrains Mono `.woff2` faces.

## Related

- [`pkg/web/webui`](..) — the server that mounts these bundles into pages.
- [`pkg/web`](../../) — the group README, including the `web/` vs `pkg/web/` naming trap.
