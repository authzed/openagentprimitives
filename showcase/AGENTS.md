# AGENTS.md — working in `showcase/`

Directions for an agent extending the OAP docs + demo system. Read
[README.md](./README.md) for what the pieces are; this is how to change them.

## Orientation

- This tree is its **own pnpm project**, outside the repo's `web/` workspace and
  outside Go's `./...` walk. Run `pnpm` commands from **`showcase/`**.
- The generators it depends on are Go, under `pkg/gen/` in the repo, driven by
  `mage` from the **repo root**.
- The docs + landing site that renders this media lives at
  [`site/`](../site/AGENTS.md) — a separate pnpm project. Doc-page authoring,
  the CLI/CRD reference recipe, and the docs validation gate live in
  `site/AGENTS.md`, not here.

## The gate — run before you call anything done

```bash
pnpm typecheck
pnpm test                                              # vitest
```

If you touched Go (a generator), also `go build ./...`, `go vet ./pkg/gen/...`,
and `go test ./pkg/gen/<pkg>/` from the repo root.

## Recipes

### Add a Slack demo

1. Write `demos/slacksim/src/scenarios/<name>.ts` (model it on an existing one):
   a `newScenario()` builder + ordered `beats` (each
   `{id, caption, hold, run}`). Reuse a captured fixture from
   `src/fixtures/blockkit/` where possible; author inline blocks only for a
   surface with no real fixture (note it).
2. Register it in `scenarios/index.ts`.
3. `pnpm slacksim:build && pnpm exec vite preview --config vite.slacksim.config.ts --port 5178 --strictPort &`
4. Render:
   `SIM_PORT=5178 node engine/capture/clip.mjs <name> out/clips/<name> dark 1600x900`
   (and/or `still.mjs <name> <beat> …` for a caption-free still). For spoken
   narration, put `ELEVENLABS_API_KEY=...` in ignored `.env.local` and run
   `node --env-file=.env.local engine/capture/clip.mjs ...`.
5. Copy to `site/public/media/`, add a `site/content/_manifest.json` entry,
   reference it from a guide (see [`site/AGENTS.md`](../site/AGENTS.md)).

### Add a console demo

Same shape in `demos/consolesim/src/scenarios/`, but a beat's `run` is **async**
and drives a `TermControl` (`type`, `line`, `prompt`, …) — animated typing +
ANSI color via `term/ansi.ts`. Serve on `:5180`, capture with `SIM_PORT=5180`.

### Regenerate Block Kit fixtures

Never hand-author a card that OAP really emits. Add a specimen in
`pkg/gen/blockcapture/blockcapture.go`, then `mage blocks:capture`. A specimen
is an envelope run through the real `slack` kind.

### Capture the real product UI

Log in over the trusted origin with a cookie jar and the **user-supplied**
password (see `engine/capture/webshot.mjs`). Rules: never read the signing key
or mint a cookie; stay **read-only**; and only capture clean, no-real-names
state — if the instance has real data, crop it out or ask for a clean instance.
The web UI is **dark-only** (ignores OS light mode).

### Record a real-product clip

Write a story in `engine/capture/stories/<name>.mjs` (model it on
`builder-first-agent.mjs`): `intro` plus ordered `beats`, each
`{ id, caption, hold, shot?, run(ctx) }`. `run` drives the real page through
`ctx` (`click`, `pointerTo`, `say`, `decide`, `waitFor`, `steps`, `composed`,
`still`, `kubectl`) and returns once the page shows what the caption says; the
engine keeps only each beat's window, so waiting on a model costs nothing on
screen. Run `engine/capture/webclip.mjs <story> out/clips/<name>` with
`OAP_ADMIN_PASSWORD` and `BASE` set. A story is NOT read-only (it starts
sessions, approves stage cards, may request an install) — say in its header what
it leaves behind, and delete that session when the capture is done. Use the
product's own visible labels as selectors; fall back to the HTTP call the
control makes, never to a fabricated state.

## Rules (non-negotiable)

- **No real names.** Fabricated everywhere — workspaces, people, repos,
  companies.
- **Deterministic scenarios.** Frozen clock, author-assigned ids, no
  `Date.now()` / `Math.random()` in scenario data — captures must be
  byte-stable.
- **Real-not-drift.** Prefer captured fixtures and generated reference over
  hand-authored approximations. When you must author (App Home, an artifact
  card), say so in the file header and keep it faithful.
- **A blocking assertion.** New generator logic gets a unit test; a demo's claim
  should be visible in the still/clip you commit.

## Gotchas (learned the hard way)

- **`SIM_PORT`** selects the sim server (default 5178 slacksim; 5180
  consolesim). `clip.mjs`/`still.mjs` build the URL from it.
- **consolesim beats are async** — `window.__showcaseStory.run(i)` returns a
  promise so `page.evaluate` awaits the whole typing animation. Keep them async.
- **Empty-state captures.** A minimal cluster yields empty admin sections;
  that's honest but thin. Re-capture on a populated cluster when one's available
  — the scripts are deterministic, so stills swap in with no doc changes.

## Commit & merge discipline

- Commit in focused chunks; keep the gate green per commit.
- The showcase branch merges to **local main** as clean `--no-ff` merges run in
  the main checkout (main diverges, so it's a merge, not a fast-forward). Local
  only — do **not** push to origin unless asked.
- Never `git stash` (repo-wide rule); never touch the main checkout's
  uncommitted work — the merges are on disjoint paths and must leave it
  untouched.
