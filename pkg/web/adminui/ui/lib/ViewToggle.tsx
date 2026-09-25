import type { ReactNode } from "react";
import { LayoutGrid, Table } from "lucide-react";

export type ViewMode = "grid" | "table";

export interface ViewToggleProps {
  value: ViewMode;
  onChange: (v: ViewMode) => void;
}

// ViewToggle is a two-button grid/table switch shared by the Agents and Agent
// Sessions views. It is purely presentational — the caller owns persistence
// (typically via useCookieState).
export function ViewToggle({ value, onChange }: ViewToggleProps) {
  return (
    <div className="inline-flex items-center rounded-md border bg-background p-0.5">
      <ToggleBtn label="Grid view" active={value === "grid"} onClick={() => onChange("grid")}>
        <LayoutGrid className="h-3.5 w-3.5" />
      </ToggleBtn>
      <ToggleBtn label="Table view" active={value === "table"} onClick={() => onChange("table")}>
        <Table className="h-3.5 w-3.5" />
      </ToggleBtn>
    </div>
  );
}

function ToggleBtn({
  label,
  active,
  onClick,
  children,
}: {
  label: string;
  active: boolean;
  onClick: () => void;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      aria-label={label}
      aria-pressed={active}
      onClick={onClick}
      className={`inline-flex h-6 w-6 items-center justify-center rounded ${
        active ? "bg-accent text-primary" : "text-muted-foreground hover:text-foreground"
      }`}
    >
      {children}
    </button>
  );
}
