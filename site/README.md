# site — OAP landing page + docs

A Next.js app that serves the Open Agent Primitives marketing landing page and
product documentation.

> Working in here as an agent? Read **[AGENTS.md](./AGENTS.md)** first — it has
> the authoring recipes and the validation gate.

## Commands

```bash
pnpm dev      # dev server → http://localhost:5179 (search needs a build; see below)
pnpm build    # pnpm check && next build && pagefind --site .next/server/app --output-path public/_pagefind
pnpm start    # serve the production build
pnpm check    # validates docs structure: media/refs resolve, refs sidecars are well-formed,
              # and every /docs link + anchor resolves
pnpm test     # vitest
```

Pagefind builds its search index from the built `.next/server/app` output, so
search only works after `pnpm build` (or `pnpm start` against a build) — not
under `pnpm dev`.

## Layout

- `app/` — routes: the `(landing)` group (the marketing page: `Landing.tsx`,
  `landing.css`, `OapMark.tsx`, `owasp.ts`) and `docs/` (sidebar layout, one
  static route per guide, `docs.css`)
- `components/` — shared client components: `Analytics`, `ThemeToggle`,
  `Wordmark`, `NavLink`, `Search` (Pagefind UI), `media` (Clip/Screenshot/
  ScreenshotSeries + lightbox), `Callout`, `Coverage`
- `lib/` — pure and server helpers: `nav`, `guides`, `links`, `manifest`,
  `analytics`, `pagefind`
- `content/` — `docs/*.mdx` guide pages (with optional `*.refs.yaml`
  anti-hallucination sidecars) and `_manifest.json`, the media asset map. The
  `mage docs:cli` / `mage docs:crd` generators write into `content/docs`.
- `public/` — static assets: `media/` (committed stills and clips) and, after a
  build, the generated `_pagefind/` search index
- `scripts/` — `check.ts`, the validator behind `pnpm check`

## Analytics

The site records page views and page leaves through PostHog's cookieless mode,
via `i.authzed.com`. It sets no cookies, writes no browser storage, and does not
identify visitors. Autocapture, session replay, heatmaps, surveys, feature
flags, conversations, and product tours are off. Nothing is sent unless
`NEXT_PUBLIC_POSTHOG_KEY` is set on a production Vercel deploy. The whole
configuration is [`lib/analytics.ts`](./lib/analytics.ts).

## Deploying

Vercel project root: `site`, with source files outside the root included — the
brand SVGs are imported directly from
[`docs/assets/brand`](../docs/assets/brand), one directory up from this
project's root.
