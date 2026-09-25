import * as React from "react";
import { Alert, AlertDescription, Badge, Button, Card, CardContent, CardHeader, CardTitle, cn } from "@ap/design";
import { getApprovals, sessionKey, type Approval } from "../lib/api";

const POLL_MS = 4000;

// kindMeta maps an approval Kind to its human label + badge tint. Mirrors the
// prototype's leakage=accent / others=warning coloring.
const kindMeta: Record<string, { label: string; cls: string }> = {
  tool_call: { label: "Tool call", cls: "border-warning/40 text-warning" },
  leakage: { label: "Leakage", cls: "border-state/40 text-state" },
  content_inspection: { label: "Content", cls: "border-warning/40 text-warning" },
};

// fmtAge renders how long an approval has been waiting, from its RequestedAt
// to now. A missing/zero/invalid timestamp yields "" (no waiting badge).
function fmtAge(requestedAt: string): string {
  const t = new Date(requestedAt).getTime();
  if (Number.isNaN(t) || t < Date.UTC(2000, 0, 1)) return "";
  const secs = Math.max(0, Math.floor((Date.now() - t) / 1000));
  if (secs < 60) return `${secs}s`;
  if (secs < 3600) return `${Math.floor(secs / 60)}m`;
  return `${Math.floor(secs / 3600)}h`;
}

export function ApprovalsView({
  apiBase,
  onOpenSession,
}: {
  apiBase: string;
  onOpenSession?: (ns: string, name: string) => void;
}) {
  const [rows, setRows] = React.useState<Approval[]>([]);
  const [error, setError] = React.useState<string | null>(null);
  const [loaded, setLoaded] = React.useState(false);

  React.useEffect(() => {
    let active = true;
    const load = () => {
      getApprovals(apiBase)
        .then((r) => { if (active) { setRows(r); setError(null); setLoaded(true); } })
        .catch((e: Error) => { if (active) { setError(e.message); setLoaded(true); } });
    };
    load();
    const id = setInterval(load, POLL_MS);
    return () => { active = false; clearInterval(id); };
  }, [apiBase]);

  return (
    <div className="space-y-3">
      {error && (
        <Alert variant="destructive">
          <AlertDescription>Could not load approvals: {error}</AlertDescription>
        </Alert>
      )}
      {loaded && rows.length === 0 && !error && (
        <p className="text-sm text-muted-foreground">Nothing awaiting a decision — pending approvals appear here as agents request them.</p>
      )}
      {rows.length > 0 && (
        <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
          {rows.map((a, i) => {
            const meta = kindMeta[a.kind] ?? { label: a.kind, cls: "border-input text-muted-foreground" };
            const age = fmtAge(a.requestedAt);
            return (
              <Card key={`${sessionKey(a)}-${a.kind}-${a.requestedAt}-${i}`} className="ring-1 ring-warning/20">
                <CardHeader className="pb-2">
                  <CardTitle className="flex items-center justify-between text-sm font-medium">
                    <span className="font-mono">{a.tool || meta.label}</span>
                    <Badge variant="outline" className={cn(meta.cls)}>{meta.label}</Badge>
                  </CardTitle>
                </CardHeader>
                <CardContent className="space-y-3 text-xs text-muted-foreground">
                  <p className="font-mono">{a.agentClass || "(unknown)"} / {sessionKey(a)}</p>
                  <div className="flex items-center justify-between">
                    {a.requester ? (
                      <span>requested by <span className="text-foreground">{a.requester}</span></span>
                    ) : <span />}
                    {age && (
                      <span className="rounded-full border border-warning/40 bg-warning/10 px-2 py-0.5 text-[10px] text-warning">waiting {age}</span>
                    )}
                  </div>
                  <div className="flex items-center gap-3">
                    <Button
                      variant="outline"
                      size="sm"
                      disabled={!onOpenSession}
                      onClick={() => onOpenSession?.(a.namespace, a.name)}
                    >
                      Open session
                    </Button>
                    <span className="text-[11px] text-muted-foreground">decide in the channel thread</span>
                  </div>
                </CardContent>
              </Card>
            );
          })}
        </div>
      )}
      {rows.length > 0 && (
        <p className="text-[11px] text-muted-foreground">
          Read-only — decisions happen in the originating channel thread. Open the session to watch it.
        </p>
      )}
    </div>
  );
}
