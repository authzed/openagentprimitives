import type { FieldOutcome } from "./api";

export interface OutcomeListProps {
  outcomes: FieldOutcome[];
}

// OUTCOME_TEXT/OUTCOME_GLYPH mirror settingsui.fieldOutcome's three Outcome
// values (config.go's doc: "applied" | "next-start" | "failed"). The glyph is
// decorative (aria-hidden below) — the text label is what makes each row
// understood by a screen reader, not the glyph alone.
const OUTCOME_TEXT: Record<string, string> = {
  applied: "Applied",
  "next-start": "Applies at next start",
  failed: "Failed",
};
const OUTCOME_GLYPH: Record<string, string> = {
  applied: "✓", // check mark
  "next-start": "⏱", // stopwatch
  failed: "✕", // multiplication x
};

// FIELD_LABEL renders settingsui.fieldOutcome's Field (the wire's camelCase
// group name) as the label the settings UI shows for it.
const FIELD_LABEL: Record<string, string> = {
  model: "Model",
  password: "Password",
  ngrok: "ngrok",
  healthNotifications: "Health notifications",
};

// OutcomeList renders the per-field-group result of a config PUT
// (settingsui.configUpdateResponse.outcomes): one row per changed field
// group, each carrying both a glyph and its text meaning so the result reads
// correctly whether or not the glyph itself renders/is announced.
export function OutcomeList({ outcomes }: OutcomeListProps) {
  if (outcomes.length === 0) return null;
  return (
    <ul aria-label="Save results" className="flex flex-col gap-1 text-sm">
      {outcomes.map((o) => (
        <li key={o.field} className="flex items-start gap-2">
          <span aria-hidden="true">{OUTCOME_GLYPH[o.outcome] ?? "•"}</span>
          <span>
            <span className="font-medium">{FIELD_LABEL[o.field] ?? o.field}:</span>{" "}
            {OUTCOME_TEXT[o.outcome] ?? o.outcome}
            {o.message && <span className="text-muted-foreground"> — {o.message}</span>}
          </span>
        </li>
      ))}
    </ul>
  );
}
