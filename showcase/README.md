# showcase — OAP docs + narrated-demo system

A self-contained Node/TS project that produces the OAP documentation site and its
demo media. Two scripted **simulators** (a fake Slack and a fake terminal) and two
kinds of **real capture** (the web chat, the admin console) feed a Playwright
engine that renders stills and narrated webm/mp4 clips; those, plus generated CLI
and CRD reference, assemble into an MDX docs site.

It is deliberately **outside** the `web/` pnpm workspace and outside Go's `./...`
walk — the heavy capture toolchain (Playwright, ffmpeg, xterm.js) lives here. The
only Go touchpoints are read-only generators under `pkg/gen/` (see below).

> Working in here as an agent? Read **[AGENTS.md](./AGENTS.md)** first — it has the
> task recipes, the validation gate, and the gotchas.

## Layout

```
showcase/
├── demos/
│   ├── slacksim/     fake Slack (React): chrome + Block Kit renderer + scenario store   :5178
│   └── consolesim/   fake terminal (xterm.js): themed window + scripted typing/ANSI      :5180
├── docs/
│   ├── guides/       every doc page (*.mdx) — frontmatter meta drives the two-level nav
│   ├── _manifest.json   media map: name → clip {webm,mp4,poster} | screenshot {src}
│   ├── app/          the MDX docs app (nav, components, Clip/Screenshot/Callout, lightbox)
│   ├── public/media/ committed stills + clips (NOT gitignored)
│   ├── check.ts      validator: every referenced media/ref resolves + exists
│   ├── DOCS-PLAN.md  the information architecture
│   └── DOCS-GAPS.md  the codebase→docs gap analysis (what's covered / missing)
├── engine/
│   ├── capture/      Playwright drivers: shot.mjs (still), still.mjs (beat still),
│   │                 clip.mjs (narrated webm+mp4+poster), webshot.mjs (real product login)
│   ├── mocks/        authored HTML mocks for surfaces we can't capture (artifact viewer)
│   └── tts/          pluggable TTS seam (stubbed; clips are caption-driven for now)
└── out/              gitignored render scratch (raw videos, throwaway shots)
```

Real Block Kit fixtures live at `demos/slacksim/src/fixtures/blockkit/*.json` and are
generated (not hand-authored) — see below. The CLI/CRD reference pages under
`docs/guides/` (`oap-*.mdx`, `crd-*.mdx`) are generated too.

## Quick start

```bash
pnpm install
pnpm docs:dev            # docs site  → http://localhost:5179
pnpm slacksim:dev        # fake Slack → http://localhost:5178/?scenario=<name>&theme=dark
pnpm consolesim:dev      # fake term  → http://localhost:5180/?scenario=<name>&theme=dark
```

Validate (the gate every change must pass):

```bash
pnpm typecheck                              # tsc over docs app + both sims
pnpm exec vite build --config vite.docs.config.ts   # all MDX must parse
pnpm docs:check                             # media/refs resolve + exist
pnpm test                                   # vitest (parsers, renderers, generators-as-libs)
```

## The simulators

Both are standalone apps driven by the capture engine through the same contract:
`?scenario=<name>&theme=<light|dark>`, `window.__showcaseStory` (ordered beats +
captions), and `window.__showcase` (caption + pointer overlay).

- **slacksim** — reproduces the Slack desktop client (rail, sidebar, threaded pane,
  thread panel, composer, App Home) in light (Aubergine) + dark. Fidelity is
  anchored to **real Block Kit**: message bodies are the exact JSON OAP's `slack`
  kind emits, captured by `mage blocks:capture`. Scenarios in
  `demos/slacksim/src/scenarios/`.
- **consolesim** — a themed terminal window (xterm.js) scripted with animated
  keystroke typing and ANSI-colored output. Scenarios in
  `demos/consolesim/src/scenarios/`. Beats are `async` so a clip's typing
  animation is captured in full.

## Capturing media

The engine reads `SIM_PORT` (default 5178; set `SIM_PORT=5180` for consolesim).

```bash
# a still of a scenario played to beat N (caption-free)
SIM_PORT=5178 node engine/capture/still.mjs <scenario> <beatIndex> out/x.png dark 1600x900

# a narrated clip: plays every beat, records webm+mp4+poster
SIM_PORT=5180 node engine/capture/clip.mjs <scenario> out/clips/<name> dark 1120x780

# a one-off still of any URL (5th arg is JS run against window.__showcase)
node engine/capture/shot.mjs "http://localhost:5178/?scenario=…" out/x.png 1600x900
```

Then copy the output into `docs/public/media/`, add an entry to `_manifest.json`,
and reference it from a page with `<Clip name="…" />` or `<Screenshot name="…" />`.

## Generated content (run from the repo root)

These are the no-drift generators — **never hand-edit their output**; re-run the
generator instead.

```bash
mage blocks:capture   # real Slack Block Kit JSON → demos/slacksim/src/fixtures/blockkit/
mage docs:cli         # oap CLI reference (one page per family) from the live cobra tree
mage docs:crd         # CRD reference (one page per kind) from config/crds schemas
```

Sources: `pkg/gen/blockcapture`, `pkg/gen/clidocs`, `pkg/gen/crddocs` (shared MDX
helpers in `pkg/gen/mdxutil`). `docs:cli` runs a gated test in `cmd/oap`
(`NewRootCmd` is package `main`); `docs:crd` reads YAML directly.

## Real product captures

For surfaces that must show the real UI (the web chat, the admin console),
`webshot.mjs` (and ad-hoc scripts modeled on it) log in over the trusted origin
with a cookie jar and navigate read-only. Rules: never mint a cookie or read the
signing key — log in with the user-supplied password through the real flow; stay
read-only; and capture only clean, no-real-names state (see Conventions).

`webclip.mjs` is the clip-shaped sibling: it plays a **story** (`engine/capture/
stories/*.mjs` — ordered beats with a caption and a `run(ctx)` that drives the
real page and returns once it shows the state the caption describes), records
the browser throughout, cuts the recording down to each beat's window so a
model's thinking time never reaches the clip, and assembles webm + mp4 + poster
with the same lower-third captions the simulators use. Beats may take stills too.
It is not read-only — a story starts sessions and may approve stage cards — so
run it only against an instance you may leave state on, and clean up the session
it names when it finishes.

```bash
OAP_ADMIN_PASSWORD=… BASE=http://127.0.0.1:17080 SHOTS_DIR=out/shots \
  node engine/capture/webclip.mjs engine/capture/stories/builder-first-agent.mjs out/clips/builder-first-agent 1600x900
```

## Conventions

- **No real names.** Every workspace / channel / person / repo / company in a
  scenario is fabricated (`Acme Robotics`, `jordan`, `reviewbot`, `acme/widget`),
  per the repo-wide rule. Real screenshots are used for *design* only.
- **Deterministic.** Scenario timestamps come from a frozen clock; ids are
  author-assigned; no `Date.now()` / randomness — captures are byte-stable.
- **Real-not-drift.** Prefer captured Block Kit and generated reference over
  hand-authored approximations, so a change in the product surfaces as a diff.
