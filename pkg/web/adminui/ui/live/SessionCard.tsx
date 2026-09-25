import { Badge, Card, CardContent, CardHeader, CardTitle, cn } from "@ap/design";
import type { SessionState } from "../lib/api";
import { ResourceName } from "../lib/ResourceName";
import { statusPillClass } from "../lib/status";
import { fmtTokens } from "../lib/fmt";

// PhasePill renders an AgentSession phase as a rounded status pill, color-coded
// via the shared status→tone map (same palette as the config Status column and
// the detail-page pills) so a phase reads at a glance instead of as plain text.
// Lives here (not LiveSessions) so both the card and the table can share it
// without a circular import.
export function PhasePill({ phase }: { phase?: string }) {
  return (
    <span className={`inline-flex items-center rounded-full border px-2 py-0.5 text-[10px] ${statusPillClass(phase ?? "")}`}>
      {phase || "—"}
    </span>
  );
}

export function SessionCard({ s, onOpen }: { s: SessionState; onOpen: () => void }) {
  // Ended-and-fine is neutral (--success is stone since palette round 5, C1):
  // sand here read as a warning on every finished session.
  const dotColor = s.active ? "bg-primary" : s.phase === "Failed" ? "bg-destructive" : "bg-success";
  const pending = s.pendingToolGrants + s.pendingLeakageApprovals;
  return (
    <Card className="cursor-pointer transition-colors hover:border-primary/50" onClick={onOpen}>
      <CardHeader className="pb-2">
        <CardTitle className="flex items-center justify-between text-sm font-medium">
          <span className="flex items-center gap-2">
            <span className={cn("inline-block h-2 w-2 rounded-full", dotColor, s.active && "animate-ap-pulse-dot")} />
            <span className="font-mono">
              {s.class ?? "?"} / <ResourceName ns={s.namespace} name={s.name} />
            </span>
          </span>
          <PhasePill phase={s.phase} />
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-2 text-xs text-muted-foreground">
        {s.statusText && <p className="truncate font-mono">{s.statusText}</p>}
        <p className="font-mono">
          {fmtTokens(s.inputTokens)} in · {fmtTokens(s.outputTokens)} out · {s.elapsedSeconds}s · {s.toolCallCount} tools
        </p>
        <div className="flex gap-2">
          {s.channelKind && <Badge variant="outline">{s.channelKind}</Badge>}
          {pending > 0 && <Badge variant="destructive">{pending} approval{pending > 1 ? "s" : ""} pending</Badge>}
        </div>
        {s.active && (
          <div className="h-0.5 rounded bg-gradient-to-r from-primary via-state to-primary bg-[length:200%_100%] animate-ap-flow" />
        )}
      </CardContent>
    </Card>
  );
}
