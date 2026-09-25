// props.ts holds the prop readers every registry entry needs, plus the two
// standalone cards (question.tsx, progress.tsx) that must NOT import
// registry.tsx themselves — registry.tsx is what imports THEM to register
// their renderers, so a reverse import would be a cycle. This is the single
// definition; nothing here may be duplicated in a sibling file again.
import type { Node } from "./types";

export const p = (n: Node): Record<string, unknown> => n.props ?? {};

// s reads a string-shaped prop.
//
// Numbers and booleans are RENDERED, not discarded. The prop types are
// declared `string` in pkg/web/uicomponents, but several of these props are also
// declared bindable there (ap:metric's value/delta/caption, ap:progress's now,
// among them), and a binding's value arrives from a live endpoint that has no
// idea what Go type the prop was written as. A CRM answering `{"total": 42}`
// is the ordinary case, not a malformed one.
//
// Rejecting a number left the platform advertising a binding and then quietly
// dropping it: the metric rendered blank, with nothing anywhere saying a value
// had arrived and been thrown away. Blank is also indistinguishable from
// "genuinely empty", so the failure could not even be seen.
//
// Objects, arrays and null still fall back. Those are not a value the author
// meant to display in a text slot — they are a selector pointed at the wrong
// place — and "[object Object]" would state that mistake less clearly than an
// empty slot does.
export const s = (n: Node, key: string, fallback = ""): string => {
  const v = p(n)[key];
  if (typeof v === "string") return v;
  if (typeof v === "number") return Number.isFinite(v) ? String(v) : fallback;
  if (typeof v === "boolean") return String(v);
  return fallback;
};
