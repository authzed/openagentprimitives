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
