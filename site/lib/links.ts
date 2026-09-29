// How an MDX link renders: docs links navigate client-side, external links
// open a new tab, everything else (same-page anchors, mailto:, the landing
// page) is a plain anchor.
export type LinkKind = "docs" | "external" | "plain";

export function linkKind(href: string): LinkKind {
  if (href === "/docs" || href.startsWith("/docs/") || href.startsWith("/docs#")) return "docs";
  if (/^https?:\/\//.test(href)) return "external";
  return "plain";
}
