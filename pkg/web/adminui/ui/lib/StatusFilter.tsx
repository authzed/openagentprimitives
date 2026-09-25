import { statusTone, tonePillClass } from "./status";

export interface StatusFilterProps {
  statuses: string[];
  // The selected status, or "" for All.
  value: string;
  onChange: (v: string) => void;
}

// Shared chip layout; the color half is appended per-chip (tone when selected,
// neutral otherwise).
const BASE = "rounded-full border px-2 py-0.5 text-[10px] transition-colors";
const INACTIVE = "border-border bg-background text-muted-foreground hover:bg-accent/50";
const ALL_ACTIVE = "border-primary/40 bg-primary/10 text-primary";

// StatusFilter is the segmented `All | <status…>` selector shared by the config
// list views. Each status chip is tinted to its status-pill tone when selected —
// mirroring the Status column colors — and neutral when not. Selecting the active
// chip toggles back to All. The empty string ("") is the All selection.
export function StatusFilter({ statuses, value, onChange }: StatusFilterProps) {
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      <button
        type="button"
        aria-pressed={value === ""}
        onClick={() => onChange("")}
        className={`${BASE} ${value === "" ? ALL_ACTIVE : INACTIVE}`}
      >
        All
      </button>
      {statuses.map((s) => {
        const active = value === s;
        return (
          <button
            key={s}
            type="button"
            aria-pressed={active}
            onClick={() => onChange(active ? "" : s)}
            className={`${BASE} ${active ? tonePillClass(statusTone(s)) : INACTIVE}`}
          >
            {s}
          </button>
        );
      })}
    </div>
  );
}
