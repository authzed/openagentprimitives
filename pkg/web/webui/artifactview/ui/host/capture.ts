export type CapturedTarget = "element" | "selection" | "region";

export interface Captured {
  target: CapturedTarget;
  elementPath: string;
  fullPath: string;
  tagName: string;
  cssClasses: string[];
  elementText: string;
  selectedText: string;
  nearbyText: string;
  nearbyElements: string[];
  computedStyles: Record<string, string>;
  accessibility: { role: string; label: string };
  boundingBox: { x: number; y: number; width: number; height: number };
}

// A curated, safe subset of computed styles — enough for the agent to reason
// about appearance without dumping the whole CSSOM.
const STYLE_KEYS = [
  "color", "backgroundColor", "fontSize", "fontWeight", "lineHeight",
  "padding", "margin", "border", "display", "textAlign",
];

// cssPath builds a short selector: an id short-circuits; otherwise a bounded
// nth-of-type chain up to a nearest id or the body.
export function cssPath(el: Element): string {
  const esc = (window as any).CSS?.escape ?? ((s: string) => s);
  if (el.id) return "#" + esc(el.id);
  const parts: string[] = [];
  let cur: Element | null = el;
  while (cur && cur.nodeType === 1 && cur.tagName.toLowerCase() !== "body" && parts.length < 6) {
    if (cur.id) { parts.unshift("#" + esc(cur.id)); break; }
    const tag = cur.tagName.toLowerCase();
    const parent: Element | null = cur.parentElement;
    if (!parent) { parts.unshift(tag); break; }
    const sibs = Array.from(parent.children).filter((c) => c.tagName === cur!.tagName);
    const idx = sibs.indexOf(cur) + 1;
    parts.unshift(sibs.length > 1 ? `${tag}:nth-of-type(${idx})` : tag);
    cur = parent;
  }
  return parts.join(" > ");
}

function textOf(el: Element, cap: number): string {
  return (el.textContent || "").replace(/\s+/g, " ").trim().slice(0, cap);
}

export function captureElement(el: Element, target: CapturedTarget, selectedText: string): Captured {
  const cs = typeof window !== "undefined" && (window as any).getComputedStyle
    ? window.getComputedStyle(el as any)
    : null;
  const styles: Record<string, string> = {};
  if (cs) for (const k of STYLE_KEYS) { const v = (cs as any)[k]; if (v) styles[k] = String(v).slice(0, 80); }

  const parent = el.parentElement;
  const nearbyElements = parent
    ? Array.from(parent.children).slice(0, 6).map((c) => c.tagName.toLowerCase())
    : [];
  const rect = el.getBoundingClientRect ? el.getBoundingClientRect() : ({ x: 0, y: 0, width: 0, height: 0 } as DOMRect);

  return {
    target,
    elementPath: cssPath(el),
    fullPath: cssPath(el),
    tagName: el.tagName.toLowerCase(),
    cssClasses: Array.from(el.classList || []),
    elementText: textOf(el, 200),
    selectedText: selectedText.slice(0, 500),
    nearbyText: parent ? textOf(parent, 300) : "",
    nearbyElements,
    computedStyles: styles,
    accessibility: {
      role: el.getAttribute("role") || "",
      label: el.getAttribute("aria-label") || el.getAttribute("alt") || "",
    },
    boundingBox: { x: rect.x || 0, y: rect.y || 0, width: rect.width || 0, height: rect.height || 0 },
  };
}
