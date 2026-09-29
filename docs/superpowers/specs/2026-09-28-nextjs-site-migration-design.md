# Migrate the landing page and docs site to Next.js

Date: 2026-09-28
Status: approved design, pending implementation plan

## Goal

Serve the marketing landing page and the MDX docs as one Next.js app from a
single top-level `site/` directory, deployed on Vercel. Docs move from hash
routes (`/docs/#/<slug>`) to real paths (`/docs/<slug>`). Both pages share one
root layout, which is where analytics and other site-wide concerns live.

## Context

Today the site is a Vite build in `showcase/docs/app` with two HTML entries:
`/` (landing, `landing/main.tsx`) and `/docs/` (the MDX app, a `#/<slug>` hash
router over `import.meta.glob("../guides/*.mdx")`). Nothing builds, checks, or
deploys it in CI, and it has no hosting.

Facts the design depends on:

- 129 guides in one flat directory. Metadata is an MDX ESM export,
  `export const meta = { title, section, group, order, description }`, not YAML
  frontmatter. `pkg/gen/mdxutil.Frontmatter()` emits the same form for the
  generated CLI and CRD reference pages.
- About 292 `#/slug` links across 58 guides, 18 `/docs/#/slug` links on the
  landing page, and two Go generators that emit `#/` links
  (`pkg/gen/clidocs/clidocs.go:84`, `pkg/gen/crddocs/crddocs.go:138`) with test
  assertions on that format.
- Guides import nothing; components (`Clip`, `Screenshot`, `ScreenshotSeries`,
  `Callout`, `Coverage`, an `a` override) arrive through the MDX provider.
- `showcase/` also holds the demo-video system (slacksim, consolesim, engine),
  which shares no code with the docs app.

## Decisions

| Question | Decision | Why |
| --- | --- | --- |
| Host | Vercel | Existing resources; easy deploys. |
| Location | New top-level `site/`, its own `package.json` and pnpm lockfile | Keeps Next.js dependencies apart from the Vite sims in `showcase/`. |
| Framework | Plain Next.js 16 App Router, no docs framework | Fumadocs, Nextra, Docusaurus, Starlight, and hosted options each cost a frontmatter conversion or a theme fight. omnigent.ai runs this same shape on Vercel. |
| MDX pipeline | `@next/mdx` (bundler-compiled) | Reads `export const meta` natively, hot-reloads, and is the Next 16 recommendation for local content. `authzed/web` compiles at runtime with `next-mdx-remote`, which HashiCorp archived and which cannot read ESM exports. |
| Search | Pagefind over the prerendered HTML | Static, no service; indexes rendered text, not MDX source. |
| Old `#/` links | Rewrite everywhere; no redirect | Nothing is deployed, so no external link depends on them. |
| Theme | `next-themes` | The Next.js-native toggle. Needs `suppressHydrationWarning` on `<html>` only; every alternative that keeps a toggle and static pages needs the same. |
| Analytics | `posthog-js`, cookieless for every visitor, via `https://i.authzed.com` | Public repo: collect usage counts only, no identity or lead data, and say so. |

A throwaway spike (Next 16.3.6, Turbopack, three sample guides) confirmed:
dynamic `import(\`@/content/docs/${slug}.mdx\`)` returns `{ default, meta }`;
the docs layout can build its nav from every guide's `meta`;
`generateStaticParams` plus `dynamicParams = false` prerenders every slug;
`remark-gfm` works as a string plugin; server and `'use client'` components
both render through `mdx-components.tsx`; and
`pagefind --site .next/server/app` indexes only `data-pagefind-body` pages.
Pagefind reports URLs with a `.html` suffix, and takes the first heading as the
result title unless told otherwise.

## Architecture

```
site/
  package.json            build: pnpm check && next build && pagefind --site .next/server/app --output-path public/_pagefind
  next.config.mjs         withMDX({ options: { remarkPlugins: ['remark-gfm'] } }); turbopack.root = repo root
  mdx-components.tsx      Clip, Screenshot, ScreenshotSeries, Callout, Coverage, `a` override
  app/
    layout.tsx            <html suppressHydrationWarning>; ThemeProvider; metadata.icons; <Analytics/>
    (landing)/page.tsx    the landing page, plus landing.css
    docs/layout.tsx       sidebar from lib/guides index(); <article data-pagefind-body>; docs.css
    docs/page.tsx         redirects to the lowest-order guide
    docs/[slug]/page.tsx  loads the guide; generateStaticParams; dynamicParams = false; generateMetadata from meta
  lib/guides.ts           server-only: slugs(), load(slug), index() returning sorted sections and groups
  components/             ported components; 'use client' on media, ThemeToggle, Search, NavLink, Analytics
  content/docs/*.mdx      the 129 guides and 3 .refs.yaml sidecars
  content/_manifest.json
  public/media/
  scripts/check.ts
  AGENTS.md               authoring rules (moved from showcase/AGENTS.md)
  README.md               dev and build commands; the analytics note
```

### Routing

- Each guide is a static route at `/docs/<slug>`. An unknown slug returns 404;
  today it silently renders the first guide.
- `/docs` redirects to the lowest-order guide, matching today's default.
- Anchors (`/docs/owasp-top10#asi03`) use native fragment scrolling.
- Links within the docs go through `next/link`. Links between the landing page
  and the docs stay plain `<a>`.

### Styles

Both global sheets stay. Their classes are already disjoint (`lp-*` and
`doc-*`); only their `html` and `body` rules overlap, so those move onto each
section's wrapper element.

### Theme

- `<ThemeProvider attribute="data-theme" defaultTheme="system" enableSystem>` in
  the root layout. `suppressHydrationWarning` sits on `<html>` and nowhere else.
- The existing `data-theme` CSS tokens stay unchanged.
- Delete `theme.ts`, both inline pre-paint scripts, and the manual `storage`
  sync.
- `ThemeToggle` wraps `useTheme()` and renders its active state only after
  mount.
- The wordmark swap stays pure CSS on `data-theme`.
- Favicons move to `metadata.icons` with `media: '(prefers-color-scheme: …)'`,
  replacing `installFavicons()`.
- Brand SVGs stay in `docs/assets/brand` and are imported across the site root
  by setting `turbopack.root`. If that misbehaves on Vercel (lockfile detection,
  file tracing), fall back to a prebuild copy into `site/public/brand/`. The
  first implementation task settles this.

### Analytics

`components/Analytics.tsx` (`'use client'`), rendered once in the root layout.

- Initializes only when `NEXT_PUBLIC_POSTHOG_KEY` is set and
  `NEXT_PUBLIC_VERCEL_ENV === 'production'`. Forks, previews, and local dev send
  nothing.
- `api_host: 'https://i.authzed.com'`. No Next.js rewrites.
- `cookieless_mode: 'always'` for every visitor: no cookies, no storage, no
  identity. No EU detection is needed.
- Autocapture, session replay, heatmaps, surveys, and feature flags off.
  Captures `$pageview` (including client-side navigation) and `$pageleave`.
  `respect_dnt: true`.
- `site/README.md` states what is collected and where the config lives.

### Search

`components/Search.tsx` (`'use client'`) in the docs sidebar.

- Loads `/_pagefind/pagefind.js` on first focus through a dynamic import the
  bundler ignores. Turbopack would otherwise try to resolve it; verify the
  ignore comment works.
- Strips `.html` from result URLs.
- The guide title element carries `data-pagefind-meta="title"`.
- Only `<article data-pagefind-body>` is indexed, so the landing page and the
  nav stay out.
- With no index (under `next dev`), shows "Search is available after
  `pnpm build`" instead of failing silently.

### Components

- `mdx-components.tsx` ports the existing `mdxComponents` map.
- `media.tsx` (the lightbox) and `ThemeToggle` are client components; `Callout`
  and `Coverage` stay server components.
- The `a` override sends `/docs/…` through `next/link` and keeps
  `target="_blank"` for `http…`.
- The sidebar renders on the server in `docs/layout.tsx` and persists across
  guide navigation. A small `NavLink` client component reads `usePathname()` to
  mark the current guide.

## Migration

### Moves (`git mv`, preserving history)

| From | To |
| --- | --- |
| `showcase/docs/guides/*` | `site/content/docs/` |
| `showcase/docs/_manifest.json` | `site/content/_manifest.json` |
| `showcase/docs/public/media/` | `site/public/media/` |
| `showcase/docs/check.ts` | `site/scripts/check.ts`, path math fixed for the new depth |

Then port the landing and docs components into `site/`, and delete
`showcase/docs/`, `showcase/vite.docs.config.ts`, the `docs:*` scripts, and any
dependencies only the docs used. Trim `showcase/tsconfig.json`'s `include`.

### Links

- A one-off codemod (run, not committed) rewrites `](#/slug…)` and
  `href="#/slug…"` in the guides, and `/docs/#/slug` on the landing page
  (including the templated OWASP link), to `/docs/slug…`.
- Change the generator format strings to `/docs/oap-%s` and `/docs/crd-%s`,
  and update their test assertions.
- Point `cliDocsDir` (`magefiles/clidocs.go:18`) at `site/content/docs`.
- Cross-check: after the codemod, `mage docs:cli docs:crd` must leave the
  generated files unchanged in `git diff`.

### Checks (`site/scripts/check.ts`)

Keeps the three existing checks: manifest files exist; media names resolve with
the right kind; `.refs.yaml` claims are valid. Adds a link check:

- every `/docs/<slug>` link in the guides and the landing page names a guide;
- every `#anchor` on such a link matches an `id=` in the target guide;
- no `#/` link remains.

The build script runs the checks, so every Vercel build, including PR previews,
enforces them. No separate CI job.

## Testing

- **Unit (vitest):** `lib/guides.ts` index — section order by first
  appearance, groups anchored at their lowest-order child, defaults for missing
  `meta` fields, and sort ties.
- **Go:** the updated `clidocs` and `crddocs` tests, via `mage test:unit`.
- **Build:** all 129 routes prerender as static; an unknown slug returns 404;
  `/docs` redirects.
- **Browser (Chrome DevTools on `next build && next start`):**
  - landing and docs render in both themes;
  - the toggle persists across pages with no hydration warnings in the console;
  - search returns results that link to paths without `.html`;
  - the lightbox opens and closes;
  - with no key, PostHog makes no requests;
  - with a test key, requests go to `i.authzed.com` and no cookies or storage
    entries appear.
- **Ship gate:** `mage test:unit`, `mage test:integration`, and `mage test:e2e`
  before merge, as the repo requires, since the Go generators change.

## Documentation updates

- `README.md:344-349` (dev server and port)
- Root `CLAUDE.md` and `AGENTS.md` sections on `mage docs:*`
- `pkg/gen/README.md`, `docs/assets/brand/README.md`
- `showcase/README.md` and `showcase/AGENTS.md`: remove the docs sections;
  authoring rules move to `site/AGENTS.md`

## Sequencing

The landing-page work on the `landing-page` branch is uncommitted. Commit it
as-is first, so the migration diff reads as a move rather than mixing with
unfinished work.

## Outside the repo (owner action)

- Create the Vercel project: root directory `site`, "Include files outside the
  root directory" on (the brand SVG imports need it).
- Set `NEXT_PUBLIC_POSTHOG_KEY` for production.
- Enable "Cookieless server hash mode" in the PostHog project.

## Out of scope

- Redirects from `/docs/#/…` URLs.
- `rehype-slug` auto heading ids (the hand-written `id=` anchors keep working).
- A per-page table of contents.
- Consent UI, identified users, or lead capture.
- Any change to the demo-video system in `showcase/`.
