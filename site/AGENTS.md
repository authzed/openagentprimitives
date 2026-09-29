# AGENTS.md — working in `site/`

Directions for an agent extending the OAP docs + landing site. Read
[README.md](./README.md) for what the pieces are; this is how to change them.

## Orientation

- This tree is its **own pnpm project**, with its own `package.json` and
  `pnpm-lock.yaml`. It joins no workspace. Run `pnpm` commands from **`site/`**.
- The generators the docs content depends on are Go, under `pkg/gen/` in the
  repo, driven by `mage` from the **repo root**; they write into
  `site/content/docs`.
- Every doc page is one `*.mdx` in `content/docs/`. Its `export const meta`
  block (`title`, `section`, `group`, `order`, `description`) drives the
  two-level sidebar; the body is MDX with `<Clip>`, `<Screenshot>`, `<Callout>`,
  `<Coverage>` components.

## The gate — run before you call anything done

```bash
pnpm check       # media/refs resolve + exist; every /docs link + anchor resolves
pnpm typecheck
pnpm test
pnpm build       # pnpm check, then next build, then the Pagefind index
```

If you touched Go (a generator), also `go build ./...`, `go vet ./pkg/gen/...`,
and `go test ./pkg/gen/<pkg>/` from the repo root.

## Recipes

### Add or edit a doc page

1. Create `content/docs/<slug>.mdx` with a `meta` block. Pick `section` +
   `group` + `order` so it lands where you want — the nav orders sections and
   groups by their lowest child `order`. Existing bands: Guides `10–290` (Get
   started, Concepts, Admin console, Operations, Security), Reference
   `2010–3320` (the 7 primitives, then CLI, then CRD reference).
2. Cross-link with `[text](/docs/other-slug)`, or
   `[text](/docs/other-slug#anchor)` for a specific section — `pnpm check`
   enforces that every link resolves.
3. Match the house voice — a `doc-lede` paragraph, then `##` sections, a
   security `<Callout>` where relevant. Ground claims in code; don't invent
   behavior.

### Regenerate the CLI / CRD reference

Never hand-edit `content/docs/oap-*.mdx` or `crd-*.mdx`. From the repo root:
`mage docs:cli` / `mage docs:crd`. If the CRD schemas are stale, `mage gen:api`
first. Read the diff before committing.

### Add media

Capture stills and clips with the `showcase/` engine (see
[`showcase/AGENTS.md`](../showcase/AGENTS.md)), then copy the output into
`site/public/media/`, add an entry to `site/content/_manifest.json`, and
reference it from a page with `<Clip name="…" />` or `<Screenshot name="…" />`.

## Rules (non-negotiable)

- **Real-not-drift.** Prefer captured fixtures and generated reference over
  hand-authored approximations. When you must author, say so in the file header
  and keep it faithful.
- **A blocking assertion.** New generator logic gets a unit test; `pnpm check`
  must pass before a doc page counts as done.

## Gotchas (learned the hard way)

- **`go test` runs with cwd = the package dir**, so `mage docs:cli`'s driver
  needs an **absolute** output path (the mage target handles it) — a relative
  path would write under `cmd/oap/` instead of `site/content/docs`.
- **MDX escaping in generators.** Prose containing `<` or `{` must be entity-
  escaped (`mdxutil.EscapeMDX`); usages/flags/paths go in code spans/fences,
  which MDX takes literally — never escape those.
- **`pnpm check` covers dead links too.** It validates `/docs/<slug>` and
  `#anchor` links along with media/refs — no separate scan needed.
- **Search needs a build.** Pagefind indexes `.next/server/app`, so `pnpm dev`
  never has search; run `pnpm build` (or `pnpm start` against one) to test it.
