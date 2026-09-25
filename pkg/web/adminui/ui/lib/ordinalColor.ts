// A D3-style categorical (ordinal) color scale for the admin UI. Used by the
// Logs view to color each audit-field's distinct values (kind / outcome / agent
// / tool / actor) with a stable, visually-distinct palette so a scan of the
// table shows the categories at a glance.
//
// Stability contract: ordinalColorMap sorts the distinct values before assigning
// palette entries by index, so the SAME set of values always maps to the SAME
// colors regardless of the input order (and regardless of duplicates). Distinct
// values get distinct colors until the palette is exhausted, then it wraps.

// ORDINAL_PALETTE is the five chart series tokens (see design tokens.css).
// Five, not twelve: past five categories a colour legend stops being readable
// and the sixth value wraps to the first; that is the palette's own rule.
export const ORDINAL_PALETTE: readonly string[] = [
  // The chart series, in order, via the theme tokens so the legend
  // follows light/dark. These replaced five Tailwind hexes (blue/green/orange/
  // red/purple) that were the only non-design colours in the admin UI.
  "hsl(var(--chart-1))",
  "hsl(var(--chart-2))",
  "hsl(var(--chart-3))",
  "hsl(var(--chart-4))",
  "hsl(var(--chart-5))",
];

// ordinalColorMap builds a stable value→color map from a list of category values
// (typically one per row, with duplicates). Empty values are dropped (they carry
// no category), the remaining distinct values are sorted for determinism, and
// each is assigned a palette color by index, wrapping when there are more
// distinct values than palette entries.
export function ordinalColorMap(values: string[]): Map<string, string> {
  const distinct = Array.from(new Set(values.filter((v) => v !== ""))).sort();
  const map = new Map<string, string>();
  distinct.forEach((v, i) => map.set(v, ORDINAL_PALETTE[i % ORDINAL_PALETTE.length]));
  return map;
}

// ordinalColor returns a stable palette color for ONE value, independent of any
// surrounding set — it hashes the value into ORDINAL_PALETTE so the SAME value
// always maps to the SAME color everywhere in the app (a live-session channel
// kind, an identity credential kind, and a detail-page kind field all agree).
// This differs from ordinalColorMap, whose colors depend on which other values
// share the set. An empty value has no category → undefined.
export function ordinalColor(value: string): string | undefined {
  if (value === "") return undefined;
  // Java String.hashCode-style 32-bit rolling hash (h = h*31 + charCode);
  // Math.imul keeps the multiply in int32.
  let h = 0;
  for (let i = 0; i < value.length; i++) {
    h = (Math.imul(h, 31) + value.charCodeAt(i)) | 0;
  }
  return ORDINAL_PALETTE[(h >>> 0) % ORDINAL_PALETTE.length];
}
