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
  (await readdir(GUIDES_DIR))
    .filter((f) => f.endsWith(".mdx"))
    .map((f) => f.slice(0, -".mdx".length)),
);

export async function loadGuide(
  slug: string,
): Promise<{ Component: ComponentType; guide: Guide }> {
  const mod = (await import(`@/content/docs/${slug}.mdx`)) as GuideModule;
  return { Component: mod.default, guide: normalizeGuide(slug, mod.meta) };
}

export const allGuides = cache(async (): Promise<Guide[]> => {
  const slugs = await guideSlugs();
  const guides = await Promise.all(
    slugs.map(async (slug) => (await loadGuide(slug)).guide),
  );
  return sortGuides(guides);
});
