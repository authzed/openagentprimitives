// Shared status → tone → Tailwind-pill mapping for the admin UI.
//
// Extracted from config/ResourceTable so the Status column, the StatusFilter
// chips, and detail-page status badges all read the same colors. The healthy
// primary conditions (Valid/Ready/Connected/…) read success, the in-flight ones
// warning, the hard-failure ones destructive, Running gets the primary accent,
// and everything else (Unknown, empty, unmapped) falls through to the muted
// default.

export type Tone = "success" | "warning" | "destructive" | "primary" | "muted";

const STATUS_TONE: Record<string, Tone> = {
  Valid: "success", Ready: "success", Connected: "success", Reachable: "success",
  Synced: "success", Healthy: "success", Active: "success",
  Degraded: "warning", Syncing: "warning", Pending: "warning", Progressing: "warning",
  Failed: "destructive", Invalid: "destructive", Error: "destructive", Down: "destructive",
  // Deferred: a secret-gated sidecar the operator intentionally does not probe —
  // reachability is verified per-session by the runner. Distinct from healthy
  // (green), failure (amber Degraded / red), and unknown (grey): informational.
  Running: "primary", Deferred: "primary",
};

// Literal class strings (not template-built) so Tailwind's content scanner keeps
// the opacity-modifier utilities in the bundle.
const TONE_PILL: Record<Tone, string> = {
  success: "border-success/40 bg-success/10 text-success",
  warning: "border-warning/40 bg-warning/10 text-warning",
  destructive: "border-destructive/40 bg-destructive/10 text-destructive",
  primary: "border-primary/40 bg-primary/10 text-primary",
  muted: "border-border bg-background text-muted-foreground",
};

// statusTone maps a status word to its tone; Unknown / empty / unmapped → muted.
export function statusTone(status: string): Tone {
  return STATUS_TONE[status] ?? "muted";
}

// tonePillClass returns the border/bg/text color classes for a tone.
export function tonePillClass(tone: Tone): string {
  return TONE_PILL[tone];
}

// statusPillClass returns the color classes for a status word (via its tone).
export function statusPillClass(status: string): string {
  return tonePillClass(statusTone(status));
}
