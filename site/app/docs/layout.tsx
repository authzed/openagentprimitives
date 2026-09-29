import type { ReactNode } from "react";
import { allGuides } from "@/lib/guides";
import { buildNav } from "@/lib/nav";
import { NavLink } from "@/components/NavLink";
import { Search } from "@/components/Search";
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
        <Search />
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
