# Next.js Site Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve the landing page (`/`) and the MDX docs (`/docs/<slug>`) as one
Next.js 16 app in a new top-level `site/` directory, deployable on Vercel.

**Architecture:** Plain Next.js App Router with `@next/mdx`. Guides stay MDX
files with an `export const meta = {...}` ESM export; a server-only loader reads
the directory and dynamically imports each guide. A shared root layout carries
`next-themes` and a cookieless PostHog client. Pagefind indexes the prerendered
HTML after `next build`.

**Tech Stack:** Next.js 16.3.6 (Turbopack), React 19, `@next/mdx` 16.3.6,
`remark-gfm`, `next-themes` 0.4.x, `posthog-js` 1.434.x, Pagefind 1.5.x,
vitest, tsx, pnpm.

**Spec:** `docs/superpowers/specs/2026-09-28-nextjs-site-migration-design.md`

## Global Constraints

- All site code lives in `site/`, with its own `package.json` and
  `pnpm-lock.yaml`. It joins no workspace.
- Docs URLs are `/docs/<slug>` and `/docs/<slug>#<anchor>`. When this plan is
  finished, no `#/` link remains anywhere.
- Guide metadata stays `export const meta = { title, section, group, order, description }`.
  Do not introduce YAML frontmatter.
- MDX plugins are given to `@next/mdx` as strings (`'remark-gfm'`); Turbopack
  cannot take functions.
- `suppressHydrationWarning` appears on `<html>` and nowhere else.
- PostHog: `api_host: 'https://i.authzed.com'`, `cookieless_mode: 'always'` for
  every visitor, `person_profiles: 'never'`; initialize only when
  `NEXT_PUBLIC_POSTHOG_KEY` is set and `NEXT_PUBLIC_VERCEL_ENV === 'production'`.
  No Next.js rewrites.
- Build script: `pnpm check && next build && pagefind --site .next/server/app --output-path public/_pagefind`.
- Brand files stay single-sourced in `docs/assets/brand/`.
- Tests: vitest with `describe`/`it` and `expect`; table-driven (`it.each`)
  when four or more cases share shape.
- Never push; commit on the current branch (`landing-page`) after each task.
- The uncommitted landing-page work in `showcase/docs/app` is the port source.
  Do not commit it on its own; Task 9 deletes it.

## Review Focus

1. **A `#/` or `/docs/` string inside a code fence or inline code** must be
   left alone by the link checker (the guides document CLI output and URLs).
   Test: Task 5, `extractDocLinks` ignores fenced and inline code.
2. **Same-page anchors, `mailto:`, and relative hrefs in MDX** must render as
   plain `<a>` without `target="_blank"`, and `/docs` links must go through
   `next/link`. Test: Task 4, `linkKind` table.
3. **The landing page's templated OWASP links**
   (`` `/docs/owasp-top10#${id.toLowerCase()}` ``) cannot be checked
   statically. Every id must match an `id=` in `owasp-top10.mdx`. Test: Task 6,
   `owasp.test.ts`.
4. **Search when the Pagefind bundle is missing** (every `next dev` session)
   must show a message, not throw. Test: Task 7, `loadPagefind` returns
   `null` when the import rejects, and `toPath` strips `.html`.
5. **Analytics gating across environments**: no key, a preview deploy, and
   local dev must all send nothing. Only production with a key initializes.
   Test: Task 8, `posthogOptions` table.

---

## File Structure

```
site/
  package.json            scripts: dev, build, start, check, test, typecheck
  pnpm-workspace.yaml     allowBuilds for native deps, if pnpm asks
  next.config.mjs         withMDX + turbopack.root / outputFileTracingRoot = repo root
  tsconfig.json           @/* -> ./*
  vitest.config.ts
  mdx.d.ts                types `*.mdx` default + meta
  mdx-components.tsx      global MDX component map
  .gitignore              .next, next-env.d.ts, public/_pagefind, node_modules
  README.md               dev/build commands; analytics disclosure
  AGENTS.md               authoring rules (moved from showcase/AGENTS.md)
  app/
    layout.tsx            root: ThemeProvider, metadata.icons, viewport.themeColor, <Analytics/>
    (landing)/page.tsx    renders <Landing/>
    (landing)/Landing.tsx ported
    (landing)/landing.css ported
    (landing)/owasp.ts    OWASP rows, extracted so a test can check their anchors
    (landing)/owasp.test.ts
    (landing)/OapMark.tsx ported
    docs/layout.tsx       sidebar + article shell
    docs/page.tsx         redirect to lowest-order guide
    docs/[slug]/page.tsx  one static route per guide
    docs/docs.css         ported styles.css
  components/
    Analytics.tsx         'use client'; posthog.init
    ThemeToggle.tsx       'use client'; next-themes
    theme-toggle.css
    Wordmark.tsx          two <img>, CSS picks by data-theme
    NavLink.tsx           'use client'; active state from usePathname
    Search.tsx            'use client'; Pagefind UI
    media.tsx             'use client'; Clip/Screenshot/ScreenshotSeries + lightbox
    Callout.tsx
    Coverage.tsx
  lib/
    nav.ts                pure: normalizeGuide, sortGuides, buildNav
    nav.test.ts
    guides.ts             server-only: guideSlugs, loadGuide, allGuides
    links.ts              pure: linkKind, extractDocLinks, anchorIds, checkLinks
    links.test.ts
    manifest.ts           getAsset over content/_manifest.json
    analytics.ts          pure: posthogOptions(env)
    analytics.test.ts
    pagefind.ts           loadPagefind, toPath
    pagefind.test.ts
  content/
    docs/*.mdx, *.refs.yaml
    _manifest.json
  public/media/
  scripts/check.ts        media, manifest, refs, and link checks
```

---

### Task 1: Scaffold `site/` and settle the brand-asset import

Settles the spec's one open risk first: importing SVGs from
`docs/assets/brand` across the site root under Turbopack.

**Files:**

- Create: `site/package.json`, `site/next.config.mjs`, `site/tsconfig.json`,
  `site/vitest.config.ts`, `site/mdx.d.ts`, `site/mdx-components.tsx`,
  `site/.gitignore`, `site/app/layout.tsx`, `site/app/(landing)/page.tsx`,
  `site/lib/smoke.test.ts` (deleted in Task 2)

**Interfaces:**

- Produces: `@/*` path alias rooted at `site/`; `pnpm build`, `pnpm test`,
  `pnpm typecheck` scripts; `useMDXComponents()` from `site/mdx-components.tsx`.

- [ ] **Step 1: Create `site/package.json`**

```json
{
  "name": "@oap/site",
  "private": true,
  "type": "module",
  "scripts": {
    "dev": "next dev --port 5179",
    "build": "next build && pagefind --site .next/server/app --output-path public/_pagefind",
    "start": "next start --port 5179",
    "test": "vitest run",
    "typecheck": "tsc --noEmit"
  }
}
```

Task 3 adds `check` and prepends `pnpm check &&` to `build`.

- [ ] **Step 2: Install dependencies**

Run from `site/`:

```bash
pnpm add next@16.3.6 @next/mdx@16.3.6 @mdx-js/loader @mdx-js/react react react-dom remark-gfm server-only next-themes posthog-js
pnpm add -D typescript @types/react @types/react-dom @types/node @types/mdx vitest pagefind tsx yaml
```

Expected: exit 0 and a `site/pnpm-lock.yaml`. If pnpm prints "Ignored build
scripts" for `sharp` or `esbuild`, create `site/pnpm-workspace.yaml` with
`allowBuilds: { sharp: true, esbuild: true }` (the same key
`showcase/pnpm-workspace.yaml` uses) and re-run `pnpm install`.

- [ ] **Step 3: Create `site/next.config.mjs`**

```js
import createMDX from "@next/mdx";
import { fileURLToPath } from "node:url";

// The brand marks live once, in the repo's docs/assets/brand, outside this
// package. Widening Turbopack's root (and file tracing with it) lets the site
// import them directly instead of keeping a copy that drifts.
const repoRoot = fileURLToPath(new URL("..", import.meta.url));

// Plugins are named by string: Turbopack runs the MDX compile in Rust and
// cannot receive JavaScript functions.
const withMDX = createMDX({
  options: { remarkPlugins: ["remark-gfm"] },
});

/** @type {import('next').NextConfig} */
const nextConfig = {
  pageExtensions: ["ts", "tsx", "mdx"],
  turbopack: { root: repoRoot },
  outputFileTracingRoot: repoRoot,
};

export default withMDX(nextConfig);
```

- [ ] **Step 4: Create `site/tsconfig.json`, `site/mdx.d.ts`, `site/.gitignore`**

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "lib": ["dom", "dom.iterable", "esnext"],
    "module": "esnext",
    "moduleResolution": "bundler",
    "jsx": "preserve",
    "strict": true,
    "noUnusedLocals": true,
    "noEmit": true,
    "skipLibCheck": true,
    "esModuleInterop": true,
    "isolatedModules": true,
    "resolveJsonModule": true,
    "incremental": true,
    "plugins": [{ "name": "next" }],
    "paths": { "@/*": ["./*"] }
  },
  "include": ["next-env.d.ts", "**/*.ts", "**/*.tsx", ".next/types/**/*.ts"],
  "exclude": ["node_modules"]
}
```

```ts
// site/mdx.d.ts
declare module "*.mdx" {
  import type { ComponentType } from "react";
  export const meta:
    | { title: string; section?: string; group?: string; order?: number; description?: string }
    | undefined;
  const MDXComponent: ComponentType;
  export default MDXComponent;
}
```

The type is inlined because `next build` typechecks and `@/lib/nav` doesn't exist yet. Task 2 replaces it with an import of `GuideMeta`.

```
# site/.gitignore
node_modules/
.next/
next-env.d.ts
public/_pagefind/
```

- [ ] **Step 5: Create `site/vitest.config.ts` and a smoke test**

```ts
import { defineConfig } from "vitest/config";
import { fileURLToPath } from "node:url";

export default defineConfig({
  resolve: { alias: { "@": fileURLToPath(new URL(".", import.meta.url)) } },
  test: { environment: "node", include: ["**/*.test.ts"], exclude: ["node_modules/**", ".next/**"] },
});
```

```ts
// site/lib/smoke.test.ts
import { describe, expect, it } from "vitest";
describe("vitest wiring", () => {
  it("runs", () => expect(1 + 1).toBe(2));
});
```

Run: `pnpm test` → Expected: 1 passed.

- [ ] **Step 6: Create a minimal layout, an MDX component map, and a page that imports a brand SVG**

```tsx
// site/mdx-components.tsx
import type { MDXComponents } from "mdx/types";

const components: MDXComponents = {};

export function useMDXComponents(): MDXComponents {
  return components;
}
```

```tsx
// site/app/layout.tsx
import type { ReactNode } from "react";

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
```

```tsx
// site/app/(landing)/page.tsx  (placeholder; Task 6 replaces it)
import wordmark from "../../../docs/assets/brand/oap-wordmark-light.svg";

export default function Page() {
  return <img src={wordmark.src} alt="Open Agent Primitives" />;
}
```

- [ ] **Step 7: Build and confirm the cross-root import**

Run: `pnpm build` (from `site/`)
Expected: `▲ Next.js 16.3.6 (Turbopack)`, route `/` listed as static, Pagefind finds no `data-pagefind-body` yet, so it indexes every HTML file, and its page count here doesn't matter. Then:

```bash
grep -o '/_next/static/media/oap-wordmark-light[^"]*\.svg' .next/server/app/index.html
```

Expected: one match.

**If the build fails on the import** ("outside of the project root", a
lockfile warning that selects the wrong root, or a tracing error), switch to
the fallback. Add `"prebrand": "node scripts/copy-brand.mjs"` and run it as
the first command in both `dev` and `build`. The script copies
`../docs/assets/brand/*.svg` into `public/brand/`. Add `public/brand/` to
`site/.gitignore`, reference `/brand/<file>.svg` by URL, remove `turbopack.root`
and `outputFileTracingRoot`, and note the switch in `site/README.md`. Every
later task that says "import from `docs/assets/brand`" then means
"`/brand/<file>`".

- [ ] **Step 8: Commit**

```bash
git add site
git commit -m "Scaffold the Next.js site package"
```

---

### Task 2: Guide index and nav model

Ports the sort and nav-block logic from `showcase/docs/app/App.tsx:44-91` into
pure, tested functions.

**Files:**

- Create: `site/lib/nav.ts`, `site/lib/nav.test.ts`, `site/lib/guides.ts`
- Delete: `site/lib/smoke.test.ts`

**Interfaces:**

- Produces (`@/lib/nav`, safe to import anywhere):
  - `interface GuideMeta { title: string; section?: string; group?: string; order?: number; description?: string }`
  - `interface Guide { slug: string; title: string; section: string; group: string; order: number; description?: string }`
  - `normalizeGuide(slug: string, meta: GuideMeta | undefined): Guide`
  - `sortGuides(guides: Guide[]): Guide[]` (new array: `order` ascending, then `title` via `localeCompare`)
  - `interface NavBlock { group: string; guides: Guide[] }`
  - `interface NavSection { section: string; blocks: NavBlock[] }`
  - `buildNav(sorted: Guide[]): NavSection[]`
- Produces (`@/lib/guides`, server only):
  - `guideSlugs(): Promise<string[]>`
  - `loadGuide(slug: string): Promise<{ Component: ComponentType; guide: Guide }>`
  - `allGuides(): Promise<Guide[]>` (sorted)

- [ ] **Step 1: Write failing tests**

```ts
// site/lib/nav.test.ts
import { describe, expect, it } from "vitest";
import { buildNav, normalizeGuide, sortGuides, type Guide } from "./nav";

const g = (slug: string, order: number, section = "Guides", group = "", title = slug): Guide => ({
  slug, title, section, group, order,
});

describe("normalizeGuide", () => {
  it("missing meta: title=slug, section=Guides, group='', order=100", () => {
    expect(normalizeGuide("x", undefined)).toEqual({
      slug: "x", title: "x", section: "Guides", group: "", order: 100, description: undefined,
    });
  });
  it("partial meta: fills only the absent fields", () => {
    expect(normalizeGuide("x", { title: "X", order: 5 })).toMatchObject({
      title: "X", section: "Guides", group: "", order: 5,
    });
  });
});

describe("sortGuides", () => {
  it("orders by `order`, then title on ties, without mutating input", () => {
    const input = [g("b", 2), g("zeta", 1), g("alpha", 1)];
    expect(sortGuides(input).map((x) => x.slug)).toEqual(["alpha", "zeta", "b"]);
    expect(input.map((x) => x.slug)).toEqual(["b", "zeta", "alpha"]);
  });
});

describe("buildNav", () => {
  it("sections appear in first-seen order of the sorted list", () => {
    const nav = buildNav([g("a", 1, "Guides"), g("b", 2, "Reference"), g("c", 3, "Guides")]);
    expect(nav.map((s) => s.section)).toEqual(["Guides", "Reference"]);
  });
  it("an ungrouped guide is its own block at its own position; a group anchors at its lowest-order child", () => {
    const nav = buildNav([
      g("intro", 1),
      g("c1", 2, "Guides", "Concepts"),
      g("mid", 3),
      g("c2", 4, "Guides", "Concepts"),
    ]);
    expect(nav[0].blocks.map((b) => [b.group, b.guides.map((x) => x.slug)])).toEqual([
      ["", ["intro"]],
      ["Concepts", ["c1", "c2"]],
      ["", ["mid"]],
    ]);
  });
});
```

Run: `pnpm test lib/nav.test.ts` → Expected: FAIL, cannot resolve `./nav`.

- [ ] **Step 2: Implement `site/lib/nav.ts`**

```ts
// The docs sidebar model. Pure, so it is tested without a filesystem or a
// bundler; lib/guides.ts feeds it.
export interface GuideMeta {
  title: string;
  /** Top-level nav category (e.g. "Concepts"). */
  section?: string;
  /** Sub-heading within the section. */
  group?: string;
  order?: number;
  description?: string;
}

export interface Guide {
  slug: string;
  title: string;
  section: string;
  group: string;
  order: number;
  description?: string;
}

export function normalizeGuide(slug: string, meta: GuideMeta | undefined): Guide {
  return {
    slug,
    title: meta?.title ?? slug,
    section: meta?.section ?? "Guides",
    group: meta?.group ?? "",
    order: meta?.order ?? 100,
    description: meta?.description,
  };
}

export function sortGuides(guides: Guide[]): Guide[] {
  return [...guides].sort((a, b) => a.order - b.order || a.title.localeCompare(b.title));
}

// A nav block is one rendered unit: a single ungrouped guide, or a titled group.
// Blocks follow `order`, so an ungrouped guide sits at its own position instead
// of being hoisted above every group; a group is anchored at its lowest-order
// child.
export interface NavBlock {
  group: string;
  guides: Guide[];
}

export interface NavSection {
  section: string;
  blocks: NavBlock[];
}

export function buildNav(sorted: Guide[]): NavSection[] {
  const sections = new Map<string, { blocks: NavBlock[]; byGroup: Map<string, NavBlock> }>();
  for (const guide of sorted) {
    let s = sections.get(guide.section);
    if (!s) {
      s = { blocks: [], byGroup: new Map() };
      sections.set(guide.section, s);
    }
    if (guide.group === "") {
      s.blocks.push({ group: "", guides: [guide] });
      continue;
    }
    let block = s.byGroup.get(guide.group);
    if (!block) {
      block = { group: guide.group, guides: [] };
      s.byGroup.set(guide.group, block);
      s.blocks.push(block);
    }
    block.guides.push(guide);
  }
  return [...sections].map(([section, { blocks }]) => ({ section, blocks }));
}
```

Run: `pnpm test` → Expected: all nav tests PASS. Delete `site/lib/smoke.test.ts`.

- [ ] **Step 3: Implement `site/lib/guides.ts`**

```ts
import "server-only";
import { readdir } from "node:fs/promises";
import path from "node:path";
import { cache, type ComponentType } from "react";
import { normalizeGuide, sortGuides, type Guide, type GuideMeta } from "./nav";

// Guides are the .mdx files in content/docs; the filename is the slug. Each
// module exports a default component and `meta`, which the Go generators
// (pkg/gen/mdxutil) emit in the same shape.
const GUIDES_DIR = path.join(process.cwd(), "content/docs");

interface GuideModule {
  default: ComponentType;
  meta?: GuideMeta;
}

export const guideSlugs = cache(async (): Promise<string[]> =>
  (await readdir(GUIDES_DIR)).filter((f) => f.endsWith(".mdx")).map((f) => f.slice(0, -".mdx".length)),
);

export async function loadGuide(slug: string): Promise<{ Component: ComponentType; guide: Guide }> {
  const mod = (await import(`@/content/docs/${slug}.mdx`)) as GuideModule;
  return { Component: mod.default, guide: normalizeGuide(slug, mod.meta) };
}

export const allGuides = cache(async (): Promise<Guide[]> => {
  const slugs = await guideSlugs();
  const guides = await Promise.all(slugs.map(async (slug) => (await loadGuide(slug)).guide));
  return sortGuides(guides);
});
```

Replace the inline `meta` type in `site/mdx.d.ts` with `import type { GuideMeta } from "@/lib/nav";` and `export const meta: GuideMeta | undefined;`.

Run: `pnpm typecheck` → Expected: exit 0.

- [ ] **Step 4: Commit**

```bash
git add site
git commit -m "Add the docs guide index and nav model"
```

---

### Task 3: Move content, the checker, and the generator output dir

**Files:**

- Move: `showcase/docs/guides/*` → `site/content/docs/`;
  `showcase/docs/_manifest.json` → `site/content/_manifest.json`;
  `showcase/docs/public/media` → `site/public/media`;
  `showcase/docs/check.ts` → `site/scripts/check.ts`
- Modify: `site/scripts/check.ts:15-20`, `site/package.json`,
  `magefiles/clidocs.go:16-18`

**Interfaces:**

- Produces: `pnpm check` (in `site/`); `cliDocsDir = "site/content/docs"`.

- [ ] **Step 1: Move with history**

```bash
mkdir -p site/content/docs site/public site/scripts
git mv showcase/docs/guides/* site/content/docs/
git mv showcase/docs/_manifest.json site/content/_manifest.json
git mv showcase/docs/public/media site/public/media
git mv showcase/docs/check.ts site/scripts/check.ts
```

Expected: `ls site/content/docs/*.mdx | wc -l` → `129`;
`ls site/content/docs/*.refs.yaml | wc -l` → `3`.

- [ ] **Step 2: Fix the checker's paths**

Replace lines 15-20 of `site/scripts/check.ts`:

```ts
const siteDir = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const repoRoot = path.dirname(siteDir);
const guidesDir = path.join(siteDir, "content/docs");
const publicDir = path.join(siteDir, "public");
const manifestPath = path.join(siteDir, "content/_manifest.json");
```

Change the header comment's first line to
`// Static docs validator. Runs before \`next build\` (via \`pnpm check\`).`
Replace every `docs:check` string in messages with `check`, and change
`docs/public` in the header comment to `public/`.

- [ ] **Step 3: Wire the script**

In `site/package.json`, add `"check": "tsx scripts/check.ts"`, and change
`build` to
`"pnpm check && next build && pagefind --site .next/server/app --output-path public/_pagefind"`.

Run: `pnpm check` (in `site/`)
Expected: `check OK — 129 guide(s), 3 refs sidecar(s), 59 media entr(ies).`

- [ ] **Step 4: Point the generators at the new directory**

In `magefiles/clidocs.go`:

```go
// cliDocsDir is where the generated reference MDX lands, alongside the
// hand-written guides the site renders.
const cliDocsDir = "site/content/docs"
```

Run: `mage docs:crd && git status --short site/content/docs`
Expected: no output. The generator rewrote the files it owns byte for byte,
because only their location changed.

- [ ] **Step 5: Commit**

Commit only the moves and the edited files. The rest of `showcase/docs/app`
stays uncommitted as the port source.

```bash
git add site magefiles/clidocs.go showcase/docs/guides showcase/docs/_manifest.json showcase/docs/public showcase/docs/check.ts
git commit -m "Move docs content and checker into site/"
```

---

### Task 4: Root layout, theme, and the docs routes

**Files:**

- Move: `showcase/docs/app/components/{media.tsx,Callout.tsx,Coverage.tsx,ThemeToggle.tsx,theme-toggle.css}` → `site/components/`;
  `showcase/docs/app/manifest.ts` → `site/lib/manifest.ts`;
  `showcase/docs/app/styles.css` → `site/app/docs/docs.css`
- Create: `site/components/Wordmark.tsx`, `site/components/NavLink.tsx`,
  `site/lib/links.ts` (the `linkKind` part), `site/lib/links.test.ts`,
  `site/app/docs/layout.tsx`, `site/app/docs/page.tsx`,
  `site/app/docs/[slug]/page.tsx`
- Modify: `site/app/layout.tsx`, `site/mdx-components.tsx`

These files are uncommitted port sources, so copy them with `cp` rather than
`git mv`. Task 9 deletes the originals.

**Interfaces:**

- Consumes: `allGuides`, `guideSlugs`, `loadGuide` (Task 2); `buildNav`, `Guide` (Task 2).
- Produces:
  - `linkKind(href: string): "docs" | "external" | "plain"` in `@/lib/links`
  - `<ThemeToggle className?: string />`, `<Wordmark className?: string />`
  - `<NavLink slug: string title: string />`
  - root layout exports `metadata` and `viewport`

- [ ] **Step 1: Write the failing `linkKind` test**

```ts
// site/lib/links.test.ts
import { describe, expect, it } from "vitest";
import { linkKind } from "./links";

describe("linkKind", () => {
  it.each([
    ["/docs/safe-tools", "docs"],
    ["/docs/owasp-top10#asi03", "docs"],
    ["/docs", "docs"],
    ["https://example.com", "external"],
    ["http://example.com", "external"],
    ["#asi03", "plain"],
    ["mailto:a@b.c", "plain"],
    ["/", "plain"],
    ["relative/path", "plain"],
    ["/docsx", "plain"],
  ] as const)("%s → %s", (href, want) => {
    expect(linkKind(href)).toBe(want);
  });
});
```

Run: `pnpm test lib/links.test.ts` → Expected: FAIL (no `./links`).

- [ ] **Step 2: Implement `linkKind`**

```ts
// site/lib/links.ts
// How an MDX link renders: docs links navigate client-side, external links
// open a new tab, everything else (same-page anchors, mailto:, the landing
// page) is a plain anchor.
export type LinkKind = "docs" | "external" | "plain";

export function linkKind(href: string): LinkKind {
  if (href === "/docs" || href.startsWith("/docs/") || href.startsWith("/docs#")) return "docs";
  if (/^https?:\/\//.test(href)) return "external";
  return "plain";
}
```

Run: `pnpm test` → Expected: PASS.

- [ ] **Step 3: Port the components**

```bash
mkdir -p site/components site/app/docs
cp showcase/docs/app/components/{media.tsx,Callout.tsx,Coverage.tsx,ThemeToggle.tsx,theme-toggle.css} site/components/
cp showcase/docs/app/manifest.ts site/lib/manifest.ts
cp showcase/docs/app/styles.css site/app/docs/docs.css
```

Then edit:

- `site/lib/manifest.ts`: change the import to
  `import manifest from "@/content/_manifest.json";`
- `site/components/media.tsx`: add `"use client";` as the first line, and
  change `from "../manifest"` to `from "@/lib/manifest"`.

- [ ] **Step 4: Rewrite `ThemeToggle` on `next-themes`**

Replace `site/components/ThemeToggle.tsx`. The icons are unchanged from the original. `JSX` is imported from `react` because React 19 has no global `JSX` namespace.

```tsx
"use client";
import { useEffect, useState, type JSX } from "react";
import { useTheme } from "next-themes";
import "./theme-toggle.css";

type Mode = "light" | "dark" | "system";

// Native radios rather than buttons with aria-checked: a radio group gives
// arrow-key navigation, roving focus and the correct grouping semantics for
// free, and the three options are genuinely mutually exclusive.
const MODES: { id: Mode; label: string; icon: JSX.Element }[] = [
    {
    id: "light",
    label: "Light",
    icon: (
      <svg viewBox="0 0 16 16" aria-hidden="true">
        <circle cx="8" cy="8" r="3.1" />
        <path d="M8 1v1.6M8 13.4V15M1 8h1.6M13.4 8H15M3.05 3.05l1.13 1.13M11.82 11.82l1.13 1.13M12.95 3.05l-1.13 1.13M4.18 11.82l-1.13 1.13" />
      </svg>
    ),
  },
  {
    id: "dark",
    label: "Dark",
    icon: (
      <svg viewBox="0 0 16 16" aria-hidden="true">
        <path d="M13.5 9.6A5.8 5.8 0 0 1 6.4 2.5a5.8 5.8 0 1 0 7.1 7.1Z" />
      </svg>
    ),
  },
  {
    id: "system",
    label: "System",
    icon: (
      <svg viewBox="0 0 16 16" aria-hidden="true">
        <rect x="1.6" y="2.6" width="12.8" height="9" rx="1.2" />
        <path d="M5.6 13.9h4.8" />
      </svg>
    ),
  },
];

export function ThemeToggle({ className }: { className?: string }) {
  const { theme, setTheme } = useTheme();
  // The server cannot know the stored choice, so no radio is checked until
  // mount; rendering one would mismatch hydration.
  const [mounted, setMounted] = useState(false);
  useEffect(() => setMounted(true), []);

  return (
    <fieldset className={`theme-toggle${className ? ` ${className}` : ""}`}>
      <legend className="tt-sr">Color theme</legend>
      {MODES.map((mode) => (
        <label key={mode.id} className="tt-opt" title={`${mode.label} theme`}>
          <input
            type="radio"
            name="oap-theme"
            value={mode.id}
            checked={mounted && theme === mode.id}
            onChange={() => setTheme(mode.id)}
          />
          <span className="tt-icon">{mode.icon}</span>
          <span className="tt-sr">{mode.label}</span>
        </label>
      ))}
    </fieldset>
  );
}
```

 In `site/components/theme-toggle.css`, delete the
`:root[data-theme-switching]` rule and its comment (lines 6-14);
`disableTransitionOnChange` replaces it. Update the file's header comment to
say the palettes come from `.lp` and `.doc-app`.

- [ ] **Step 5: Add `Wordmark`**

```tsx
// site/components/Wordmark.tsx
// "light" and "dark" name the INK, not the background, so the light-ink file
// is the one for the dark theme. Both render; CSS on the data-theme attribute
// next-themes stamps before paint shows one, so the mark is right on the first
// frame without waiting for React.
import lightInk from "../../docs/assets/brand/oap-wordmark-light.svg";
import darkInk from "../../docs/assets/brand/oap-wordmark-dark.svg";

export function Wordmark({ className = "" }: { className?: string }) {
  return (
    <>
      <img className={`${className} wordmark--on-dark`} src={lightInk.src} alt="" />
      <img className={`${className} wordmark--on-light`} src={darkInk.src} alt="" />
    </>
  );
}
```

In `site/app/docs/docs.css`, rename `.doc-brand-wordmark--on-dark` and
`.doc-brand-wordmark--on-light` to `.wordmark--on-dark` and
`.wordmark--on-light`.

- [ ] **Step 6: Root layout with `next-themes`, icons, and theme color**

```tsx
// site/app/layout.tsx
import type { Metadata, Viewport } from "next";
import type { ReactNode } from "react";
import { ThemeProvider } from "next-themes";
import iconDarkInk from "../../docs/assets/brand/oap-icon-dark.svg";
import iconLightInk from "../../docs/assets/brand/oap-icon-light.svg";

export const metadata: Metadata = {
  title: { default: "Open Agent Primitives", template: "%s · OAP docs" },
  description:
    "OAP is a Kubernetes-native runtime for LLM agents. Every state-touching tool call is checked against a SpiceDB relationship graph before it runs, so the model is never asked whether it may act.",
  // The favicon sits on the browser's tab strip, not on this page, so its ink
  // follows the OS scheme rather than the site's toggle.
  icons: {
    icon: [
      { url: iconDarkInk.src, type: "image/svg+xml", media: "(prefers-color-scheme: light)" },
      { url: iconLightInk.src, type: "image/svg+xml", media: "(prefers-color-scheme: dark)" },
    ],
  },
};

export const viewport: Viewport = {
  colorScheme: "dark light",
  themeColor: [
    { media: "(prefers-color-scheme: dark)", color: "#0c050f" },
    { media: "(prefers-color-scheme: light)", color: "#fcfcfc" },
  ],
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    // next-themes sets data-theme on <html> before hydration; this is the one
    // element whose attributes are allowed to differ from the server render.
    <html lang="en" suppressHydrationWarning>
      <body>
        <ThemeProvider attribute="data-theme" defaultTheme="system" enableSystem disableTransitionOnChange>
          {children}
        </ThemeProvider>
      </body>
    </html>
  );
}
```

The landing page's `og:title`, `og:description` and `og:type` come in Task 6.

- [ ] **Step 7: Scope `docs.css`'s document-level rules to the docs**

In `site/app/docs/docs.css`:

- Replace the `html, body, #root { margin: 0; height: 100%; }` rule with
  `html, body { margin: 0; }`.
- Change the `body { background… }` selector to `body:has(.doc-app)`. That
  keeps the page background on the document (overscroll included) only when
  the docs are mounted.
- Move the `--tt-*` declarations out of `:root` into a new `.doc-app { … }`
  block, and the light-theme `--tt-*` (if any) into
  `:root[data-theme='light'] .doc-app`.
- Add styles to `.doc-nav-link`, which is now an `<a>`, not a `<button>`:
  `text-decoration: none;` and `box-sizing: border-box;`.

- [ ] **Step 8: `NavLink`, the docs layout, and the routes**

```tsx
// site/components/NavLink.tsx
"use client";
import Link from "next/link";
import { usePathname } from "next/navigation";

export function NavLink({ slug, title }: { slug: string; title: string }) {
  const href = `/docs/${slug}`;
  const active = usePathname() === href;
  return (
    <Link className={`doc-nav-link${active ? " is-active" : ""}`} href={href} aria-current={active ? "page" : undefined}>
      {title}
    </Link>
  );
}
```

```tsx
// site/app/docs/layout.tsx
import type { ReactNode } from "react";
import { allGuides } from "@/lib/guides";
import { buildNav } from "@/lib/nav";
import { NavLink } from "@/components/NavLink";
import { ThemeToggle } from "@/components/ThemeToggle";
import { Wordmark } from "@/components/Wordmark";
import "./docs.css";

export default async function DocsLayout({ children }: { children: ReactNode }) {
  const nav = buildNav(await allGuides());
  return (
    <div className="doc-app">
      <aside className="doc-nav" data-pagefind-ignore>
        <div className="doc-brand-row">
          {/* A plain <a>: the landing page is a different stylesheet, so leave by full load. */}
          <a className="doc-brand" href="/" aria-label="Open Agent Primitives home">
            <Wordmark className="doc-brand-wordmark" />
          </a>
          <div className="doc-brand-meta">
            <span className="doc-brand-sub">docs</span>
            <ThemeToggle />
          </div>
        </div>
        {nav.map(({ section, blocks }) => (
          <div className="doc-nav-section" key={section}>
            <div className="doc-nav-section-title">{section}</div>
            {blocks.map((block, i) => (
              <div className="doc-nav-group" key={`${section}/${block.group}/${i}`}>
                {block.group && <div className="doc-nav-group-title">{block.group}</div>}
                <ul>
                  {block.guides.map((g) => (
                    <li key={g.slug}>
                      <NavLink slug={g.slug} title={g.title} />
                    </li>
                  ))}
                </ul>
              </div>
            ))}
          </div>
        ))}
      </aside>
      <main className="doc-main">
        <article className="doc-article" data-pagefind-body>
          {children}
        </article>
      </main>
    </div>
  );
}
```

```tsx
// site/app/docs/page.tsx
import { redirect } from "next/navigation";
import { allGuides } from "@/lib/guides";

// /docs has no page of its own: it opens the lowest-order guide, as the old
// hash router did for an empty hash.
export default async function DocsIndex() {
  const [first] = await allGuides();
  redirect(`/docs/${first.slug}`);
}
```

```tsx
// site/app/docs/[slug]/page.tsx
import type { Metadata } from "next";
import { guideSlugs, loadGuide } from "@/lib/guides";

// Every guide is prerendered; any other slug is a real 404, not a silent
// fallback to the first guide.
export const dynamicParams = false;

export async function generateStaticParams() {
  return (await guideSlugs()).map((slug) => ({ slug }));
}

type Props = { params: Promise<{ slug: string }> };

export async function generateMetadata({ params }: Props): Promise<Metadata> {
  const { guide } = await loadGuide((await params).slug);
  return { title: guide.title, description: guide.description };
}

export default async function GuidePage({ params }: Props) {
  const { Component, guide } = await loadGuide((await params).slug);
  return (
    <div data-pagefind-meta={`title:${guide.title}`}>
      <Component />
    </div>
  );
}
```

- [ ] **Step 9: The MDX component map**

```tsx
// site/mdx-components.tsx
import type { MDXComponents } from "mdx/types";
import Link from "next/link";
import { Clip, Screenshot, ScreenshotSeries } from "@/components/media";
import { Callout } from "@/components/Callout";
import { Coverage } from "@/components/Coverage";
import { linkKind } from "@/lib/links";

// The component set every guide gets without importing anything. Custom
// components are capitalized so MDX resolves them here, not as HTML tags.
const components: MDXComponents = {
  Clip,
  Screenshot,
  ScreenshotSeries,
  Callout,
  Coverage,
  a: ({ href = "", ...props }) => {
    switch (linkKind(href)) {
      case "docs":
        return <Link href={href} {...props} />;
      case "external":
        return <a href={href} target="_blank" rel="noreferrer" {...props} />;
      default:
        return <a href={href} {...props} />;
    }
  },
};

export function useMDXComponents(): MDXComponents {
  return components;
}
```

- [ ] **Step 10: Build and verify the routes**

Run: `pnpm typecheck && pnpm test && pnpm build`
Expected: the route list shows `● /docs/[slug]` with 129 prerendered paths:

```bash
ls .next/server/app/docs/*.html | wc -l   # 129
```

Then run `pnpm start &` and:

```bash
curl -s -o /dev/null -w '%{http_code}\n' localhost:5179/docs/safe-tools         # 200
curl -s -o /dev/null -w '%{http_code}\n' localhost:5179/docs/no-such-guide      # 404
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' localhost:5179/docs    # 307 …/docs/<lowest-order slug>
curl -s localhost:5179/docs/safe-tools | grep -c '<title>Safe tools · OAP docs</title>'  # 1
```

Stop the server.

- [ ] **Step 11: Commit**

```bash
git add site
git commit -m "Serve the docs from Next.js routes with next-themes"
```

---

### Task 5: Rewrite links to real paths and enforce them

**Files:**

- Modify: `site/lib/links.ts`, `site/lib/links.test.ts`,
  `site/scripts/check.ts`, 58 files in `site/content/docs/`,
  `pkg/gen/clidocs/clidocs.go:84`, `pkg/gen/clidocs/clidocs_test.go:38`,
  `pkg/gen/crddocs/crddocs.go:138`, `pkg/gen/crddocs/crddocs_test.go:70`

**Interfaces:**

- Produces in `@/lib/links`:
  - `extractDocLinks(src: string): string[]`: every `](href)` and
    `href="href"` target starting with `/docs` or `#/`, skipping fenced code
    blocks and inline code spans.
  - `anchorIds(src: string): Set<string>`: every `id="…"` value.
  - `interface LinkProblem { file: string; href: string; reason: string }`
  - `checkLinks(files: { file: string; src: string }[], guides: Map<string, Set<string>>): LinkProblem[]`,
    where `guides` maps slug to anchor ids.

- [ ] **Step 1: Go: change the assertions first**

In `pkg/gen/clidocs/clidocs_test.go:38`:

```go
assert.Contains(t, over, "[`oap agent`](/docs/oap-agent)", "family listed in overview")
```

In `pkg/gen/crddocs/crddocs_test.go:70`:

```go
assert.Contains(t, over, "[Widget](/docs/crd-widget)", "kind listed in the overview")
```

Run: `go test ./pkg/gen/clidocs/ ./pkg/gen/crddocs/`
Expected: FAIL on both assertions.

- [ ] **Step 2: Go: change the format strings**

`pkg/gen/clidocs/clidocs.go:84`:

```go
fmt.Fprintf(&b, "| [`oap %s`](/docs/oap-%s) | %s |\n", f.Name(), f.Name(), mdxutil.TableText(f.Short))
```

`pkg/gen/crddocs/crddocs.go:138`:

```go
fmt.Fprintf(&b, "| [%s](/docs/crd-%s) | %s | %s |\n", c.kind, c.singular, c.scope, mdxutil.TableText(c.desc))
```

Run: `go test ./pkg/gen/...` → Expected: PASS.

- [ ] **Step 3: Write failing link-checker tests**

Append to `site/lib/links.test.ts`:

```ts
import { anchorIds, checkLinks, extractDocLinks } from "./links";

describe("extractDocLinks", () => {
  it("finds markdown and JSX doc links, with and without anchors", () => {
    const src = 'See [a](/docs/a) and [b](/docs/b#x). <a href="/docs/c">c</a> [old](#/d)';
    expect(extractDocLinks(src)).toEqual(["/docs/a", "/docs/b#x", "/docs/c", "#/d"]);
  });
  it("skips external links, same-page anchors, and mailto", () => {
    expect(extractDocLinks("[e](https://x.io/docs/a) [h](#top) [m](mailto:a@b.c)")).toEqual([]);
  });
  it("ignores links inside fenced code blocks and inline code", () => {
    const src = "```\n[a](/docs/nope)\n```\nand `[b](#/nope)` but [c](/docs/yes)";
    expect(extractDocLinks(src)).toEqual(["/docs/yes"]);
  });
});

describe("anchorIds", () => {
  it("collects id attributes", () => {
    expect(anchorIds('<h3 id="asi01">x</h3> <span id="b" />')).toEqual(new Set(["asi01", "b"]));
  });
});

describe("checkLinks", () => {
  const guides = new Map([
    ["a", new Set<string>()],
    ["owasp", new Set(["asi01"])],
  ]);
  it.each([
    ["/docs/a", null],
    ["/docs", null],
    ["/docs/owasp#asi01", null],
    ["/docs/missing", "no guide 'missing'"],
    ["/docs/owasp#asi99", "no id 'asi99' in 'owasp'"],
    ["#/a", "hash route; use /docs/a"],
  ])("%s → %s", (href, reason) => {
    const got = checkLinks([{ file: "f.mdx", src: `[x](${href})` }], guides);
    expect(got).toEqual(reason ? [{ file: "f.mdx", href, reason }] : []);
  });
});
```

Run: `pnpm test lib/links.test.ts` → Expected: FAIL (exports missing).

- [ ] **Step 4: Implement the checker functions**

Append to `site/lib/links.ts`:

```ts
// Code samples may legitimately show a URL or an old link; blank them out
// (keeping length, so nothing downstream shifts) before looking for links.
function stripCode(src: string): string {
  return src
    .replace(/^```[\s\S]*?^```/gm, (m) => " ".repeat(m.length))
    .replace(/`[^`\n]*`/g, (m) => " ".repeat(m.length));
}

const LINK_RE = /\]\(([^)\s]+)\)|\bhref="([^"]+)"/g;

export function extractDocLinks(src: string): string[] {
  const out: string[] = [];
  for (const m of stripCode(src).matchAll(LINK_RE)) {
    const href = m[1] ?? m[2];
    if (href === "/docs" || href.startsWith("/docs/") || href.startsWith("#/")) out.push(href);
  }
  return out;
}

export function anchorIds(src: string): Set<string> {
  return new Set([...src.matchAll(/\bid="([^"]+)"/g)].map((m) => m[1]));
}

export interface LinkProblem {
  file: string;
  href: string;
  reason: string;
}

export function checkLinks(files: { file: string; src: string }[], guides: Map<string, Set<string>>): LinkProblem[] {
  const problems: LinkProblem[] = [];
  for (const { file, src } of files) {
    for (const href of extractDocLinks(src)) {
      if (href.startsWith("#/")) {
        problems.push({ file, href, reason: `hash route; use /docs/${href.slice(2)}` });
        continue;
      }
      if (href === "/docs" || href === "/docs/") continue;
      const [slug, anchor] = href.slice("/docs/".length).split("#");
      const ids = guides.get(slug);
      if (!ids) problems.push({ file, href, reason: `no guide '${slug}'` });
      else if (anchor && !ids.has(anchor)) problems.push({ file, href, reason: `no id '${anchor}' in '${slug}'` });
    }
  }
  return problems;
}
```

Run: `pnpm test` → Expected: PASS.

- [ ] **Step 5: Wire the link check into `scripts/check.ts`**

Add after the refs block (before the `if (errors.length)` summary):

```ts
import { anchorIds, checkLinks } from "../lib/links";
// (put the import with the others at the top of the file)

// 4: every /docs link names a guide, and every #anchor names an id in it. The
// landing page is plain TSX, so it is scanned the same way; a templated href
// (the OWASP table) cannot be, and is covered by app/(landing)/owasp.test.ts.
const guideSources = new Map(mdxFiles.map((f) => [f.slice(0, -4), readFileSync(path.join(guidesDir, f), "utf8")]));
const guideAnchors = new Map([...guideSources].map(([slug, src]) => [slug, anchorIds(src)]));
const linkFiles = [
  ...[...guideSources].map(([slug, src]) => ({ file: `content/docs/${slug}.mdx`, src })),
  ...readdirSync(path.join(siteDir, "app"), { recursive: true, encoding: "utf8" })
    .filter((f) => f.endsWith(".tsx"))
    .map((f) => ({ file: `app/${f}`, src: readFileSync(path.join(siteDir, "app", f), "utf8") })),
];
for (const p of checkLinks(linkFiles, guideAnchors)) err(`${p.file}: ${p.href}: ${p.reason}`);
```

Run: `pnpm check`
Expected: FAIL with about 292 `hash route` problems. This confirms the check sees the old links. `app/` has none yet: the Task 1 placeholder page has no links, and Task 6 ports the landing page.

- [ ] **Step 6: Run the one-off codemod (not committed)**

Write it to the session scratchpad, not the repo:

```js
// $SCRATCH/rewrite-links.mjs — run from the repo root
import { readFileSync, writeFileSync, readdirSync } from "node:fs";
const dir = "site/content/docs";
let changed = 0;
for (const f of readdirSync(dir).filter((f) => f.endsWith(".mdx"))) {
  const p = `${dir}/${f}`;
  const before = readFileSync(p, "utf8");
  const after = before.replaceAll("](#/", "](/docs/").replaceAll('href="#/', 'href="/docs/');
  if (after !== before) {
    writeFileSync(p, after);
    changed++;
  }
}
console.log(`rewrote ${changed} file(s)`);
```

Run: `node $SCRATCH/rewrite-links.mjs` → Expected: `rewrote 58 file(s)`.
Then run `(cd site && pnpm check)` → Expected: `check OK`.

- [ ] **Step 7: Cross-check against the generators**

The generators now emit `/docs/` links, so regenerating over the codemodded
tree must change nothing:

```bash
git diff site/content/docs > "$SCRATCH/after-codemod.patch"
mage docs:cli && mage docs:crd
git diff site/content/docs > "$SCRATCH/after-regen.patch"
cmp "$SCRATCH/after-codemod.patch" "$SCRATCH/after-regen.patch" && echo SAME
```

Expected: `SAME`. A difference means the codemod and the generators disagree
on the link format. Fix whichever one is wrong before continuing.

- [ ] **Step 8: Commit**

```bash
git add site pkg/gen/clidocs pkg/gen/crddocs
git commit -m "Link docs by path instead of hash route; check links in the build"
```

---

### Task 6: Port the landing page

**Files:**

- Copy: `showcase/docs/app/landing/{Landing.tsx,landing.css,OapMark.tsx}` → `site/app/(landing)/`
- Create: `site/app/(landing)/owasp.ts`, `site/app/(landing)/owasp.test.ts`
- Modify: `site/app/(landing)/page.tsx` (replaces the Task 1 placeholder)

**Interfaces:**

- Consumes: `ThemeToggle` (Task 4); `anchorIds` (Task 5).
- Produces: `OWASP: readonly (readonly [id: string, title: string, level: CoverageLevel, gap: string])[]`
  and `CoverageLevel` exported from `owasp.ts`.

- [ ] **Step 1: Copy and rewrite links**

```bash
cp showcase/docs/app/landing/{Landing.tsx,landing.css,OapMark.tsx} "site/app/(landing)/"
sed -i '' 's#/docs/\#/#/docs/#g' "site/app/(landing)/Landing.tsx"
grep -c '#/' "site/app/(landing)/Landing.tsx"   # 0
```

In `Landing.tsx`, add `"use client";` as the first line (it uses `useState`
and `navigator.clipboard`), change `from "../components/ThemeToggle"` to
`from "@/components/ThemeToggle"`, and replace any `JSX.Element` with
`React.JSX.Element` (or import `type JSX` from `react`).

- [ ] **Step 2: Extract the OWASP rows and write the failing anchor test**

Move the `OWASP` array (and its tuple and level types) from `Landing.tsx`
into `site/app/(landing)/owasp.ts` as named exports, unchanged, and import it
back into `Landing.tsx`.

```ts
// site/app/(landing)/owasp.test.ts
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { anchorIds } from "@/lib/links";
import { OWASP } from "./owasp";

// The landing table builds each row's link as /docs/owasp-top10#<id>, which
// the static link check cannot follow. Pin it here instead.
describe("landing OWASP links", () => {
  const ids = anchorIds(readFileSync(fileURLToPath(new URL("../../content/docs/owasp-top10.mdx", import.meta.url)), "utf8"));
  it.each(OWASP.map(([id]) => id))("%s has an anchor in owasp-top10", (id) => {
    expect(ids.has(id.toLowerCase())).toBe(true);
  });
});
```

Run: `pnpm test` → Expected: 10 PASS. As a mutation check, temporarily change
one id in `owasp.ts` to `ASI99` and confirm the test fails. Then revert.

- [ ] **Step 3: The page, its metadata, and CSS scoping**

```tsx
// site/app/(landing)/page.tsx
import type { Metadata } from "next";
import { Landing } from "./Landing";
import "./landing.css";

export const metadata: Metadata = {
  title: { absolute: "Open Agent Primitives — the agent proposes, SpiceDB decides" },
  openGraph: {
    title: "Open Agent Primitives",
    description: "A Kubernetes-native runtime for LLM agents, with authorization decided outside the model.",
    type: "website",
  },
};

export default function Page() {
  return <Landing />;
}
```

In `landing.css`, make the same scoping changes as Task 4 Step 7:

- change `body { … }` to `body:has(.lp)`;
- move the `--tt-*` declarations from `:root` to `.lp` (and the light-theme
  ones to `:root[data-theme='light'] .lp`);
- delete any `data-theme-switching` rule.

Then `import "./landing.css";` must not also be imported by `Landing.tsx`;
remove it there.

- [ ] **Step 4: Build and verify**

Run: `pnpm check && pnpm typecheck && pnpm build`, then `pnpm start &` and:

```bash
curl -s localhost:5179/ | grep -c 'class="lp"'                    # 1
curl -s localhost:5179/ | grep -o 'href="/docs/[^"]*"' | sort -u | head
```

Expected: every href is `/docs/<slug>` or `/docs/<slug>#<id>`, with no `#/`.
Stop the server.

- [ ] **Step 5: Commit**

```bash
git add site
git commit -m "Port the landing page to the Next.js site"
```

---

### Task 7: Search with Pagefind

**Files:**

- Create: `site/lib/pagefind.ts`, `site/lib/pagefind.test.ts`,
  `site/components/Search.tsx`
- Modify: `site/app/docs/layout.tsx` (mount `<Search />` under the brand
  row), `site/app/docs/docs.css` (search styles)

**Interfaces:**

- Produces:
  - `interface PagefindResult { url: string; meta: { title?: string }; excerpt: string }`
  - `interface Pagefind { search(q: string): Promise<{ results: { data(): Promise<PagefindResult> }[] }> }`
  - `loadPagefind(importer?: () => Promise<Pagefind>): Promise<Pagefind | null>`
  - `toPath(url: string): string`

- [ ] **Step 1: Write failing tests**

```ts
// site/lib/pagefind.test.ts
import { describe, expect, it } from "vitest";
import { loadPagefind, toPath } from "./pagefind";

describe("toPath", () => {
  it.each([
    ["/docs/safe-tools.html", "/docs/safe-tools"],
    ["/docs/owasp-top10.html#asi03", "/docs/owasp-top10#asi03"],
    ["/docs/safe-tools", "/docs/safe-tools"],
    ["/index.html", "/"],
  ])("%s → %s", (url, want) => expect(toPath(url)).toBe(want));
});

describe("loadPagefind", () => {
  it("returns null when the bundle is missing (next dev)", async () => {
    expect(await loadPagefind(() => Promise.reject(new Error("404")))).toBeNull();
  });
  it("returns the module when the import succeeds", async () => {
    const pf = { search: async () => ({ results: [] }) };
    expect(await loadPagefind(async () => pf)).toBe(pf);
  });
});
```

Run: `pnpm test lib/pagefind.test.ts` → Expected: FAIL.

- [ ] **Step 2: Implement `site/lib/pagefind.ts`**

```ts
// Pagefind's bundle is written to public/_pagefind by `pnpm build`, after
// Next has compiled everything, so the bundler must not try to resolve it.
// Under `next dev` there is no bundle and the import rejects.
export interface PagefindResult {
  url: string;
  meta: { title?: string };
  excerpt: string;
}

export interface Pagefind {
  search(q: string): Promise<{ results: { data(): Promise<PagefindResult> }[] }>;
}

const BUNDLE = "/_pagefind/pagefind.js";

const importBundle = () => import(/* webpackIgnore: true */ /* turbopackIgnore: true */ BUNDLE) as Promise<Pagefind>;

export async function loadPagefind(importer: () => Promise<Pagefind> = importBundle): Promise<Pagefind | null> {
  try {
    return await importer();
  } catch {
    return null;
  }
}

// Pagefind records the prerendered file's path (`/docs/x.html`); the route is
// `/docs/x`.
export function toPath(url: string): string {
  const [p, hash] = url.split("#");
  const clean = p.replace(/\/index\.html$/, "/").replace(/\.html$/, "");
  return hash ? `${clean}#${hash}` : clean;
}
```

Run: `pnpm test` → Expected: PASS.

- [ ] **Step 3: The `Search` component**

```tsx
// site/components/Search.tsx
"use client";
import Link from "next/link";
import { useRef, useState } from "react";
import { loadPagefind, toPath, type Pagefind, type PagefindResult } from "@/lib/pagefind";

type State = { kind: "idle" } | { kind: "unavailable" } | { kind: "results"; items: PagefindResult[] };

export function Search() {
  const pf = useRef<Pagefind | null | undefined>(undefined);
  const [state, setState] = useState<State>({ kind: "idle" });

  async function ensure() {
    if (pf.current === undefined) pf.current = await loadPagefind();
    if (pf.current === null) setState({ kind: "unavailable" });
    return pf.current;
  }

  async function onInput(q: string) {
    const engine = await ensure();
    if (!engine) return;
    if (!q.trim()) return setState({ kind: "idle" });
    const { results } = await engine.search(q);
    setState({ kind: "results", items: await Promise.all(results.slice(0, 8).map((r) => r.data())) });
  }

  return (
    <div className="doc-search" role="search">
      <input
        className="doc-search-input"
        type="search"
        placeholder="Search docs"
        aria-label="Search docs"
        onFocus={ensure}
        onChange={(e) => void onInput(e.target.value)}
      />
      {state.kind === "unavailable" && (
        <p className="doc-search-note">Search is available after <code>pnpm build</code>.</p>
      )}
      {state.kind === "results" && (
        <ul className="doc-search-results">
          {state.items.length === 0 && <li className="doc-search-note">No matches.</li>}
          {state.items.map((r) => (
            <li key={r.url}>
              <Link href={toPath(r.url)}>{r.meta.title ?? toPath(r.url)}</Link>
              {/* Pagefind's excerpt is its own escaped text with <mark> highlights. */}
              <p dangerouslySetInnerHTML={{ __html: r.excerpt }} />
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
```

Mount it in `site/app/docs/layout.tsx` right after the `doc-brand-row` div:
`<Search />` (import from `@/components/Search`). Add to `docs.css`:

```css
.doc-search { margin-bottom: 20px; }
.doc-search-input {
  width: 100%;
  padding: 7px 10px;
  border: 1px solid var(--doc-border);
  border-radius: 6px;
  background: var(--doc-bg);
  color: var(--doc-text);
  font: inherit;
  font-size: 14px;
}
.doc-search-input:focus-visible { outline: 2px solid var(--doc-accent); outline-offset: 1px; }
.doc-search-note { color: var(--doc-muted); font-size: 13px; margin: 8px 2px 0; }
.doc-search-results { list-style: none; margin: 8px 0 0; padding: 0; font-size: 13.5px; }
.doc-search-results li { padding: 6px 4px; border-bottom: 1px solid var(--doc-border); }
.doc-search-results p { margin: 2px 0 0; color: var(--doc-muted); }
.doc-search-results mark { background: var(--doc-accent-soft); color: inherit; }
```

- [ ] **Step 4: Verify the index**

Run: `pnpm build`. Expected: Pagefind reports `Indexed 129 pages`. It must be
129 exactly: the landing page and the `/docs` redirect aren't indexed. Then
decode one fragment and confirm the title and URL:

```bash
f=$(ls public/_pagefind/fragment | head -1); gzip -dc "public/_pagefind/fragment/$f" | tail -c +13 | head -c 300
```

Expected: `"url":"/docs/<slug>.html"`, and `"meta":{"title":"<the guide's meta.title>"`.

Then run `pnpm start &`, open `localhost:5179/docs/safe-tools`, and type
`sandbox` in the search box. Expected: results appear, and clicking one lands
on `/docs/<slug>` (no `.html`). Under `pnpm dev`, focusing the box shows "Search
is available after `pnpm build`." Stop the servers.

- [ ] **Step 5: Commit**

```bash
git add site
git commit -m "Add Pagefind search to the docs"
```

---

### Task 8: Cookieless analytics

**Files:**

- Create: `site/lib/analytics.ts`, `site/lib/analytics.test.ts`,
  `site/components/Analytics.tsx`
- Modify: `site/app/layout.tsx` (render `<Analytics />` inside `ThemeProvider`)

**Interfaces:**

- Produces:
  - `posthogOptions(env: { key?: string; vercelEnv?: string }): { key: string; config: Partial<PostHogConfig> } | null`
  - `<Analytics />`

- [ ] **Step 1: Write failing tests**

```ts
// site/lib/analytics.test.ts
import { describe, expect, it } from "vitest";
import { posthogOptions } from "./analytics";

describe("posthogOptions", () => {
  it.each([
    ["no key, production", { vercelEnv: "production" }, false],
    ["key, preview deploy", { key: "phc_x", vercelEnv: "preview" }, false],
    ["key, local dev (no VERCEL_ENV)", { key: "phc_x" }, false],
    ["empty key, production", { key: "", vercelEnv: "production" }, false],
    ["key, production", { key: "phc_x", vercelEnv: "production" }, true],
  ])("%s → initializes: %s", (_name, env, want) => {
    expect(posthogOptions(env) !== null).toBe(want);
  });

  it("production config is cookieless, anonymous, and pageview-only", () => {
    const opts = posthogOptions({ key: "phc_x", vercelEnv: "production" })!;
    expect(opts.key).toBe("phc_x");
    expect(opts.config).toMatchObject({
      api_host: "https://i.authzed.com",
      cookieless_mode: "always",
      person_profiles: "never",
      capture_pageview: "history_change",
      capture_pageleave: true,
      autocapture: false,
      disable_session_recording: true,
      capture_heatmaps: false,
      disable_surveys: true,
      advanced_disable_flags: true,
      disable_external_dependency_loading: true,
      respect_dnt: true,
    });
  });
});
```

Run: `pnpm test lib/analytics.test.ts` → Expected: FAIL.

- [ ] **Step 2: Implement `site/lib/analytics.ts`**

```ts
import type { PostHogConfig } from "posthog-js";

// This is a public repo, so tracking is deliberately minimal and legible in
// this one file: page views and page leaves, counted without cookies, storage,
// or identity (PostHog's cookieless server-hash mode, which the PostHog project
// must have enabled). No autocapture, replay, heatmaps, surveys, or flags.
// Only a production deploy with a key configured sends anything; forks,
// previews and local dev never do.
export function posthogOptions(env: {
  key?: string;
  vercelEnv?: string;
}): { key: string; config: Partial<PostHogConfig> } | null {
  if (!env.key || env.vercelEnv !== "production") return null;
  return {
    key: env.key,
    config: {
      api_host: "https://i.authzed.com",
      defaults: "2026-01-30",
      cookieless_mode: "always",
      person_profiles: "never",
      capture_pageview: "history_change",
      capture_pageleave: true,
      autocapture: false,
      capture_heatmaps: false,
      capture_dead_clicks: false,
      capture_exceptions: false,
      disable_session_recording: true,
      disable_surveys: true,
      disable_web_experiments: true,
      advanced_disable_flags: true,
      disable_external_dependency_loading: true,
      respect_dnt: true,
    },
  };
}
```

Run: `pnpm test` → Expected: PASS. If `tsc` rejects a key (the option names
were checked against `@posthog/types` 1.412 on 2026-09-28), fix it against
the installed types rather than casting it away.

- [ ] **Step 3: The client component**

```tsx
// site/components/Analytics.tsx
"use client";
import posthog from "posthog-js";
import { useEffect } from "react";
import { posthogOptions } from "@/lib/analytics";

export function Analytics() {
  useEffect(() => {
    const opts = posthogOptions({
      key: process.env.NEXT_PUBLIC_POSTHOG_KEY,
      vercelEnv: process.env.NEXT_PUBLIC_VERCEL_ENV,
    });
    if (opts) posthog.init(opts.key, opts.config);
  }, []);
  return null;
}
```

`process.env.NEXT_PUBLIC_*` must be written out literally like this; Next
inlines those references at build time. Render `<Analytics />` in
`site/app/layout.tsx` as the last child inside `ThemeProvider`.

- [ ] **Step 4: Verify in a browser**

Build with no key: run `pnpm build && pnpm start`. In Chrome DevTools
(Network), load `/` and `/docs/safe-tools`. Expected: no request to
`i.authzed.com`.

Build as production with a throwaway key:
`NEXT_PUBLIC_POSTHOG_KEY=phc_test NEXT_PUBLIC_VERCEL_ENV=production pnpm build && pnpm start`.
Expected:

- Loading `/` sends requests to `https://i.authzed.com/…`.
- Clicking a nav link in the docs sends one more `$pageview`.
- `document.cookie` has no `ph_` entries.
- `localStorage` and `sessionStorage` have no `ph_` keys.

The requests may be rejected because the key is fake. That's expected, since
this step only checks where they go and what they store.

- [ ] **Step 5: Commit**

```bash
git add site
git commit -m "Add cookieless PostHog page-view analytics"
```

---

### Task 9: Remove the Vite docs app; update tooling and docs

**Files:**

- Delete: `showcase/docs/` (all remaining files, committed and uncommitted),
  `showcase/vite.docs.config.ts`
- Modify: `showcase/package.json` (drop `docs:*` scripts and docs-only
  dependencies), `showcase/tsconfig.json` (`include`), `magefiles/fmt.go`
  (`fmtTargets`), `README.md:344-349`, `CLAUDE.md` and `AGENTS.md` (the
  "Documentation regeneration" paragraph), `pkg/gen/README.md`,
  `docs/assets/brand/README.md`, `showcase/README.md`, `showcase/AGENTS.md`
- Create: `site/README.md`, `site/AGENTS.md`

- [ ] **Step 1: Delete the Vite docs app**

```bash
git rm -r -q showcase/docs showcase/vite.docs.config.ts
rm -rf showcase/docs     # the uncommitted port sources (theme.ts, favicon.ts, landing/, docs/index.html, …)
```

Expected: `ls showcase/docs` → "No such file or directory".

- [ ] **Step 2: Trim `showcase/`**

In `showcase/package.json`, remove the `docs:dev`, `docs:build`,
`docs:preview` and `docs:check` scripts. For each of `@mdx-js/react`,
`@mdx-js/rollup`, `@types/mdx`, `remark-gfm` and `react-markdown`, run
`grep -rl "<name>" showcase/demos showcase/engine`. Remove it with
`pnpm remove <name>` (in `showcase/`) only if the grep prints nothing. In
`showcase/tsconfig.json`, remove `"docs"` and `"vite.docs.config.ts"` from
`include`.

Run: `(cd showcase && pnpm typecheck && pnpm test)`
Expected: both exit 0.

- [ ] **Step 3: Formatter targets**

In `magefiles/fmt.go`, add `"site/**/*.ts"` and `"site/**/*.tsx"` to
`fmtTargets`. Then run `mage fmt:all && mage fmt:check`. Expected: `fmt:check`
exits 0. Commit the reformat together with this task's changes.

- [ ] **Step 4: Write `site/README.md` and `site/AGENTS.md`**

`site/README.md` must contain:

- the commands: `pnpm dev` (localhost:5179; search needs a build),
  `pnpm build`, `pnpm start`, `pnpm check`, `pnpm test`;
- the layout, one line per top-level directory from the File Structure above;
- an **Analytics** section: "The site records page views and page leaves
  through PostHog's cookieless mode, via `i.authzed.com`. It sets no cookies,
  writes no browser storage, and does not identify visitors. Autocapture,
  session replay, heatmaps, surveys and feature flags are off. Nothing is sent
  unless `NEXT_PUBLIC_POSTHOG_KEY` is set on a production Vercel deploy. The
  whole configuration is `lib/analytics.ts`."
- **Deploying:** Vercel project root `site`, with source files outside the
  root included (the brand SVGs come from `docs/assets/brand`).

For `site/AGENTS.md`, move the authoring sections of `showcase/AGENTS.md`
here. Replace `[text](#/other-slug)` with `[text](/docs/other-slug)` and
`pnpm docs:check` with `pnpm check`. Replace
`vite build --config vite.docs.config.ts` with `pnpm build`, and delete the
manual dead-link shell loop, since `pnpm check` now does that. Media now goes
into `site/public/media/` and gets registered in `site/content/_manifest.json`.

- [ ] **Step 5: Update the other docs**

Find every stale reference:

```bash
grep -rn "showcase/docs\|docs:dev\|docs:build\|docs:check\|vite.docs\|#/<slug>\|(#/" \
  --include='*.md' --include='*.go' . | grep -v '^./site/' | grep -v node_modules | grep -v docs/superpowers
```

Update each one:

- `README.md` should say `cd site && pnpm install && pnpm dev` at
  localhost:5179.
- The `CLAUDE.md` and `AGENTS.md` regeneration paragraphs should say the
  generators write into `site/content/docs`.
- `pkg/gen/README.md` gets the output directory.
- In `docs/assets/brand/README.md`, change the `showcase/docs/app` consumer
  entry to `site/` (the `Wordmark` component, the root layout icons, and
  `app/(landing)/OapMark.tsx`).
- In `showcase/README.md` and `showcase/AGENTS.md`, drop the docs sections and
  add one line pointing to `site/`.

Re-run the grep. Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add -A showcase site magefiles/fmt.go README.md CLAUDE.md AGENTS.md pkg/gen/README.md docs/assets/brand/README.md
git status --short   # confirm nothing unexpected is staged (docs/marketing/ stays untracked)
git commit -m "Remove the Vite docs app; point tooling and docs at site/"
```

---

### Task 10: Whole-site verification and the ship gate

No new code. Every item needs recorded evidence (command output or a
screenshot). If anything fails, fix it in the task that owns the code, then
re-run this whole task from the top.

- [ ] **Step 1: Site gates**

Run (in `site/`): `pnpm install --frozen-lockfile && pnpm typecheck && pnpm test && pnpm build`
Expected: all exit 0; `check OK — 129 guide(s)…`; 129 `/docs/[slug]` paths;
Pagefind `Indexed 129 pages`.

- [ ] **Step 2: No hash links anywhere**

```bash
grep -rn '#/' site/content/docs site/app pkg/gen --include='*.mdx' --include='*.tsx' --include='*.go' | grep -v '_test.go' | grep -E '\]\(#/|href="#/|/docs/#/'
```

Expected: no output.

- [ ] **Step 3: Browser pass (Chrome DevTools MCP, on `pnpm start`)**

Check each item and record the evidence:

1. `/` renders in dark and light. Take a screenshot of each.
2. `/docs/safe-tools` renders in dark and light, with the wordmark matching
   the theme.
3. Choose Light on `/`, then open `/docs/…`. The page is light on its first
   paint (no flash), and the toggle shows Light. Reload, and it's still Light.
4. After all of the above, the console has **no hydration warnings or errors**
   (use `list_console_messages`).
5. Navigating between two guides through the sidebar is client-side (the
   network shows no document request), and the active link highlight moves.
6. `/docs/owasp-top10#asi03` scrolls to ASI03. The landing page's ASI03 link
   lands at the same place.
7. A `Screenshot` lightbox opens on click and closes on Escape and on a
   backdrop click. A `Clip` plays.
8. Search returns results, and a result link opens the guide without `.html`.
9. An external link in a guide opens in a new tab. A `/docs/…` link does not.
10. `/docs/nope` shows the 404 page.

- [ ] **Step 4: Ship gate**

Check that the machine isn't overloaded first (`uptime`). Then run from the
repo root:

```bash
mage test:unit
mage test:integration
mage test:e2e
mage fmt:check
```

Expected: every command exits 0. Quote the final line of each. If a failure
looks like a timeout, re-run that test alone with
`go test -tags=e2e -run '^TestName$' ./its/package/` before deciding whether
it's a flake, as `CLAUDE.md` requires.

- [ ] **Step 5: Hand off the owner actions**

Report to the user, without performing any of it:

- Create the Vercel project with root directory `site` and source files
  outside the root directory included.
- Set `NEXT_PUBLIC_POSTHOG_KEY` for production only.
- Enable "Cookieless server hash mode" in the PostHog project.
- After the first preview deploy, confirm that
  `https://<preview>/_pagefind/pagefind.js` returns 200. That proves Vercel
  deployed the Pagefind bundle written after `next build`.
