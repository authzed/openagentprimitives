import type { InteractionExcerpt, InteractionField } from "./types";

// The two body parts every channelevents card shares — an interaction prompt
// (InteractionCard) and a one-way notice (NoticeCard) carry the SAME
// label/value rows and the SAME untrusted excerpt, because both render the
// same wire fields. They live here rather than being copied per card so the
// excerpt's security contract is written down once: a second copy is a second
// place for it to be got wrong.

// CardFields renders the label/value supporting rows. Publisher-authored and
// trusted (unlike CardExcerpt), but still plain text nodes — never markup.
export function CardFields({ fields }: { fields?: InteractionField[] }) {
  if (!fields || fields.length === 0) return null;
  return (
    <div className="flex flex-col gap-0.5">
      {fields.map((f, i) => (
        // Keyed by label+index, not label alone: two fields can share a label
        // (e.g. repeated "Scope:" rows) and a bare-label key would collide,
        // causing React to conflate/misrender the rows.
        <div key={`${f.label}:${i}`} className="text-xs leading-snug">
          <span className="font-medium text-card-foreground">{f.label}:</span>{" "}
          <span className="text-muted-foreground">{f.value}</span>
        </div>
      ))}
    </div>
  );
}

// CardExcerpt renders the UNTRUSTED excerpt (see InteractionExcerpt in
// types.ts).
//
// SECURITY CONTRACT: the content is attacker-influenced — a content-inspection
// preview, tool output, an upstream controller's condition message — so it
// renders as literal text inside a <pre><code> block, never interpreted as
// markup, and visually fenced so a reader can tell it apart from the
// publisher-authored copy above it. Crossing a wire does not launder it.
export function CardExcerpt({ excerpt }: { excerpt?: InteractionExcerpt }) {
  if (!excerpt) return null;
  return (
    <pre className="max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-md border border-border bg-background px-2 py-1.5 text-[11px] leading-snug text-card-foreground">
      <code>
        {excerpt.label ? `${excerpt.label}:\n` : ""}
        {excerpt.content}
      </code>
    </pre>
  );
}
