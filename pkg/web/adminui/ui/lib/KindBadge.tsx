import { ordinalColor } from "./ordinalColor";

// KindBadge renders a "kind" value (slack / oauth / mcpserver / static / …) as a
// small mono pill prefixed with a stable per-value color dot. The color is
// derived by hashing the value into the shared ordinal palette (lib/ordinalColor)
// so the SAME kind reads in the SAME color everywhere in the console — the
// live-session Channel cell, the Identities credential column, the config tables,
// and the detail pages all agree without coordinating a shared value set. An
// empty/absent kind renders nothing (no category to color).
export function KindBadge({ kind, className }: { kind?: string; className?: string }) {
  if (!kind) return null;
  return (
    <span
      className={`inline-flex items-center gap-1.5 rounded-full border bg-background px-2 py-0.5 font-mono text-[10px] text-muted-foreground${
        className ? ` ${className}` : ""
      }`}
    >
      <KindDot kind={kind} />
      {kind}
    </span>
  );
}

// KindDot is the bare stable color swatch for a kind, for inline use where a full
// chip is too heavy (a cell that already shows the kind as text). Decorative →
// aria-hidden; the adjacent label carries the meaning for assistive tech.
export function KindDot({ kind }: { kind: string }) {
  const color = ordinalColor(kind);
  if (!color) return null;
  return (
    <span
      aria-hidden="true"
      className="inline-block h-2 w-2 shrink-0 rounded-full"
      style={{ backgroundColor: color }}
    />
  );
}
