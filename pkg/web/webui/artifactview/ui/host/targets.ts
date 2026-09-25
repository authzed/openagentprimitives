import type { CapturedTarget } from "./capture";

const REGION_TAGS = new Set(["section", "article", "aside", "nav", "header", "footer", "dialog", "form"]);
const REGION_ROLES = new Set(["dialog", "region", "banner", "navigation", "form"]);

function isRegion(el: Element): boolean {
  const tag = el.tagName.toLowerCase();
  if (REGION_TAGS.has(tag)) return true;
  const role = el.getAttribute("role");
  if (role && REGION_ROLES.has(role)) return true;
  return /(^|\s)(card|modal|panel|section)(\s|$)/i.test(el.className || "");
}

export function elementLabel(el: Element): string {
  const tag = el.tagName.toLowerCase();
  if (el.id) return `${tag}#${el.id}`;
  const cls = (el.classList && el.classList[0]) || "";
  return cls ? `${tag}.${cls}` : tag;
}

export function resolveClickTarget(el: Element): { el: Element; target: CapturedTarget } {
  if (isRegion(el)) return { el, target: "region" };
  return { el, target: "element" };
}

export function selectionTarget(doc: Document): { el: Element; selectedText: string } | null {
  const sel = doc.getSelection ? doc.getSelection() : null;
  if (!sel || sel.isCollapsed || sel.rangeCount === 0) return null;
  const text = sel.toString().replace(/\s+/g, " ").trim();
  if (!text) return null;
  let node: Node | null = sel.getRangeAt(0).commonAncestorContainer;
  while (node && node.nodeType !== 1) node = node.parentNode;
  if (!node) return null;
  return { el: node as Element, selectedText: text };
}
