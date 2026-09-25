import { cn } from "@ap/design";
import type { PlanItem, StatusSnapshot } from "./types";

// stateText maps (paused, cause) to the banner label — the same mapping the
// original shell used (mirrors channelevents pause causes).
export function stateText(paused: boolean, cause: string): string {
  if (!paused) return "Working";
  switch (cause) {
    case "awaiting_approval":
    case "awaiting_leakage_approval":
      return "Awaiting approval";
    // Both ways a turn stops without failing leave the same thing true for this
    // toolbar: the agent is not working and the next message is the person's.
    // The distinction between an ask and the idle exit belongs to the surface
    // that decides whether to PROMPT for a reply, which this label does not.
    case "awaiting_reply":
    case "idle":
      return "Awaiting your reply";
    case "awaiting_retry":
      return "Retrying…";
    case "failed":
      return "Failed";
    case "complete":
      return "Done";
    default:
      return "Paused";
  }
}

function statusIcon(st: string): string {
  return st === "done" ? "✓" : st === "error" ? "✕" : st === "in_progress" ? "▸" : "○";
}

function currentStep(plan: PlanItem[]): PlanItem | null {
  return (
    plan.find((p) => p.status === "in_progress") ??
    plan.find((p) => p.status !== "done" && p.status !== "error") ??
    null
  );
}

// StatusBar is the prominent center of the live-view toolbar: a colored state dot
// (pulsing while working), the bold colored state label, the agent's latest status
// message as the main text, and an optional plan-step affordance (the current step
// label, with a hover/focus popover listing all steps). Renders nothing until a
// status snapshot arrives, so the toolbar center stays empty when the session is
// idle.
export function StatusBar({ status }: { status: StatusSnapshot | null }) {
  if (!status) return null;
  const paused = !!status.paused;
  const cause = status.pauseCause || "";
  const plan = Array.isArray(status.plan) ? status.plan : [];
  const cur = currentStep(plan);
  const failed = cause === "failed";
  const done = cause === "complete";
  // Working = active and not paused: teal dot that pulses + teal label. A done
  // (succeeded) session is teal but static. Paused is amber, failed is red.
  const dotColor = failed ? "bg-destructive" : done ? "bg-success" : paused ? "bg-warning" : "bg-success";
  const labelColor = failed ? "text-destructive" : done ? "text-success" : paused ? "text-warning" : "text-success";
  const working = !paused;

  return (
    <div className="flex flex-1 min-w-0 items-center gap-[9px] text-xs">
      <span className={cn("h-[9px] w-[9px] rounded-full shrink-0", dotColor, working && "animate-ap-pulse-dot")} />
      <span className={cn("text-xs font-semibold shrink-0", labelColor)}>{stateText(paused, cause)}</span>
      {status.statusMessage && (
        <span className="text-[13px] text-foreground truncate min-w-0">{status.statusMessage}</span>
      )}
      {plan.length > 0 && cur && (
        <span className="relative group min-w-0 truncate shrink-0" tabIndex={0}>
          <span className="text-[11px] text-muted-foreground before:content-['·_']">{cur.label || ""}</span>
          {plan.length > 1 && (
            <div aria-hidden="true" className="hidden group-hover:block group-focus-within:block absolute top-[calc(100%+4px)] left-0 z-20 bg-card border border-border rounded-md p-2 min-w-[240px] max-w-[420px] shadow-xl whitespace-normal">
              {plan.map((p, i) => (
                <div key={i} className="flex gap-2 px-1 py-0.5 leading-snug items-baseline">
                  <span
                    className={cn(
                      "w-3 text-center shrink-0",
                      p.status === "in_progress" && "text-primary",
                      p.status === "done" && "text-success",
                      p.status === "error" && "text-destructive",
                      (p.status === "pending" || !p.status) && "text-muted-foreground",
                    )}
                  >
                    {statusIcon(p.status)}
                  </span>
                  <span>{p.label || ""}</span>
                </div>
              ))}
            </div>
          )}
        </span>
      )}
    </div>
  );
}
