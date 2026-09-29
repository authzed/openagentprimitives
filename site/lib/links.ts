// How an MDX link renders: docs links navigate client-side, external links
// open a new tab, everything else (same-page anchors, mailto:, the landing
// page) is a plain anchor.
export type LinkKind = "docs" | "external" | "plain";

export function linkKind(href: string): LinkKind {
  if (href === "/docs" || href.startsWith("/docs/") || href.startsWith("/docs#")) return "docs";
  if (/^https?:\/\//.test(href)) return "external";
  return "plain";
}

// Code samples may legitimately show a URL or an old link; blank them out
// (keeping length, so nothing downstream shifts) before looking for links.
function stripCode(src: string): string {
  return src
    .replace(/^```[\s\S]*?^```/gm, (m) => " ".repeat(m.length))
    .replace(/`[^`\n]*`/g, (m) => " ".repeat(m.length));
}

// Matches a markdown link target `](href)`, a JSX/HTML `href="href"`
// attribute, or an object-literal `href: "href"` / `href: 'href'` field (the
// landing page stores card links as `{ href: "/docs/…" }`).
const LINK_RE = /\]\(([^)\s]+)\)|\bhref="([^"]+)"|\bhref:\s*["']([^"']+)["']/g;

export function extractDocLinks(src: string): string[] {
  const out: string[] = [];
  for (const m of stripCode(src).matchAll(LINK_RE)) {
    const href = m[1] ?? m[2] ?? m[3];
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
