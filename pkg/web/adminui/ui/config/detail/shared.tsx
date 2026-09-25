import * as React from "react";
import { Alert, AlertDescription, AlertTitle } from "@ap/design";
import { statusPillClass } from "../../lib/status";
import type { ResourceRow } from "../../lib/api";

// DEGRADED_STATUSES are the hard/degraded primary-condition words that warrant a
// prominent destructive warning box on a detail page (in addition to the muted
// status pill every page shows). Kept in sync with lib/status's destructive +
// warning tones for the failure end of the spectrum.
export const DEGRADED_STATUSES: ReadonlySet<string> = new Set([
  "Degraded",
  "Failed",
  "Down",
  "Invalid",
]);

export function isDegraded(status: string): boolean {
  return DEGRADED_STATUSES.has(status);
}

// StatusPill is the small colored readiness pill shown in the detail header.
export function StatusPill({ status, reason }: { status: string; reason?: string }) {
  return (
    <span
      title={reason || undefined}
      className={`inline-flex items-center rounded-full border px-2.5 py-0.5 text-[11px] ${statusPillClass(status)}`}
    >
      {status || "Unknown"}
    </span>
  );
}

// DegradedBox is the prominent destructive Alert rendered on a detail page's
// Overview when the resource is degraded/failed — it surfaces the FULL
// statusReason so an admin sees *why* without source-diving (the header pill
// only shows the word). Renders nothing for healthy resources.
export function DegradedBox({ status, reason }: { status: string; reason?: string }) {
  if (!isDegraded(status)) return null;
  return (
    <Alert variant="destructive">
      <AlertTitle>{status}</AlertTitle>
      {reason && <AlertDescription>{reason}</AlertDescription>}
    </Alert>
  );
}

// Field is one labeled fact on a detail page (label above value).
export function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-1">
      <span className="text-[10px] font-medium uppercase tracking-wide text-muted-foreground">{label}</span>
      <div className="text-sm text-foreground">{children}</div>
    </div>
  );
}

// Chip renders one badge value as a neutral mono pill (shared by the config
// tables and the detail SectionRenderer so both render one identical chip).
export function Chip({ children }: { children: React.ReactNode }) {
  return (
    <span className="rounded-full border bg-background px-2 py-0.5 text-[10px] font-mono text-muted-foreground">
      {children}
    </span>
  );
}

// badgeVal reads a single badge value off a row by key — the config LIST views
// (channels/sources/users) pull specific facts out of the flat ResourceRow.
export function badgeVal(row: ResourceRow, key: string): string | undefined {
  return row.badges?.find((b) => b.key === key)?.value;
}
