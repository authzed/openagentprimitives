import { useEffect, useState, type ComponentType } from "react";
import { MDXProvider } from "@mdx-js/react";
import { mdxComponents } from "./components/mdxComponents";
// The brand files live once, at the repo root (docs/assets/brand); the docs
// chrome is dark-only, so it takes the light-ink variant.
import wordmarkUrl from "../../../docs/assets/brand/oap-wordmark-light.svg";

interface GuideMeta {
  title: string;
  /** Top-level nav category (e.g. "Concepts"). */
  section?: string;
  /** Sub-heading within the section (e.g. "Multiplayer & Permissions"). */
  group?: string;
  order?: number;
  description?: string;
}
interface GuideModule {
  default: ComponentType;
  meta?: GuideMeta;
}

// Eagerly load every guide. Each MDX module exports a default component and a
// `meta` object (title/section/group/order) used to build the two-level sidebar.
const modules = import.meta.glob<GuideModule>("../guides/*.mdx", {
  eager: true,
});

interface Guide {
  slug: string;
  title: string;
  section: string;
  group: string;
  order: number;
  description?: string;
  Component: ComponentType;
}

const guides: Guide[] = Object.entries(modules)
  .map(([path, mod]) => {
    const slug = path
      .split("/")
      .pop()!
      .replace(/\.mdx$/, "");
    const meta = mod.meta ?? { title: slug };
    return {
      slug,
      title: meta.title,
      section: meta.section ?? "Guides",
      group: meta.group ?? "",
      order: meta.order ?? 100,
      description: meta.description,
      Component: mod.default,
    };
  })
  .sort((a, b) => a.order - b.order || a.title.localeCompare(b.title));

// Preserve first-seen order for sections.
const sections = [...new Set(guides.map((g) => g.section))];

// A nav block is one rendered unit in the sidebar: either a single ungrouped
// guide or a titled group of guides. Blocks are emitted in `order` sequence, so
// an ungrouped guide sits at its own order position rather than being hoisted
// above every group — a group is anchored at its first-seen (lowest-order) child.
interface NavBlock {
  group: string;
  guides: Guide[];
}
const navBlocksIn = (section: string): NavBlock[] => {
  const blocks: NavBlock[] = [];
  const byGroup = new Map<string, NavBlock>();
  for (const g of guides.filter((x) => x.section === section)) {
    if (g.group === "") {
      blocks.push({ group: "", guides: [g] });
      continue;
    }
    let block = byGroup.get(g.group);
    if (!block) {
      block = { group: g.group, guides: [] };
      byGroup.set(g.group, block);
      blocks.push(block);
    }
    block.guides.push(g);
  }
  return blocks;
};

// A route is a guide slug plus an optional intra-page anchor. The URL hash is
// `#/<slug>` or `#/<slug>#<anchor>` — the second `#` deep-links into a page (e.g.
// a concept overview links to `#/owasp-top10#asi03`).
function parseHash(): { slug: string; anchor: string } {
  const raw = window.location.hash.replace(/^#\/?/, "");
  const [slug, anchor] = raw.split("#");
  return { slug, anchor: anchor ?? "" };
}

export function App() {
  const [route, setRoute] = useState(() => {
    const { slug, anchor } = parseHash();
    return {
      slug: guides.some((g) => g.slug === slug)
        ? slug
        : (guides[0]?.slug ?? ""),
      anchor,
    };
  });
  const active = guides.find((g) => g.slug === route.slug) ?? guides[0];

  // Any `#/…` link (nav button or in-content) flows through hashchange, so cross-
  // doc links work without wiring each one.
  useEffect(() => {
    const onHash = () => {
      const { slug, anchor } = parseHash();
      if (guides.some((g) => g.slug === slug)) setRoute({ slug, anchor });
    };
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  // Scroll to the anchor after the page renders, else to the top on page change.
  useEffect(() => {
    if (route.anchor) {
      document.getElementById(route.anchor)?.scrollIntoView({ block: "start" });
    } else {
      window.scrollTo(0, 0);
    }
  }, [route]);

  const link = (g: Guide) => (
    <li key={g.slug}>
      <button
        className={`doc-nav-link ${g.slug === active?.slug ? "is-active" : ""}`}
        onClick={() => {
          window.location.hash = `/${g.slug}`;
        }}
      >
        {g.title}
      </button>
    </li>
  );

  return (
    <div className="doc-app">
      <aside className="doc-nav">
        <div className="doc-brand">
          <img
            className="doc-brand-wordmark"
            src={wordmarkUrl}
            alt="Open Agent Primitives"
          />
          <span className="doc-brand-sub">docs</span>
        </div>
        {sections.map((section) => (
          <div className="doc-nav-section" key={section}>
            <div className="doc-nav-section-title">{section}</div>
            {navBlocksIn(section).map((block, i) => (
              <div
                className="doc-nav-group"
                key={`${section}/${block.group}/${i}`}
              >
                {block.group && (
                  <div className="doc-nav-group-title">{block.group}</div>
                )}
                <ul>{block.guides.map(link)}</ul>
              </div>
            ))}
          </div>
        ))}
      </aside>
      <main className="doc-main">
        <article className="doc-article">
          {active && (
            <MDXProvider components={mdxComponents}>
              <active.Component />
            </MDXProvider>
          )}
        </article>
      </main>
    </div>
  );
}
