import { useEffect, useState } from "react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  ModelName,
} from "@ap/design";
import type { SessionDetail } from "../../chat/ui/types";

// SessionDetail is imported from the chat package because GET
// /sessions/api/{ns}/{name}/detail is served there (pkg/web/webui/chat/handlers.go's
// sessionDetailHandler) — the wire type belongs beside the handler that writes
// it, not re-declared here where it could drift from it silently.

// fetchDetail reads the info panel's fields for one session.
//
// A non-2xx is NOT parsed as JSON. The route-level Authorize gate
// (pkg/web/webui/chat's Routes) answers an unauthorized or indeterminate caller
// with an HTML system page BEFORE the handler runs, so reading any failure as
// JSON would throw on the HTML body and surface as "failed to load" with no
// cause — instead of telling the viewer they do not have access.
async function fetchDetail(ns: string, name: string): Promise<SessionDetail> {
  const resp = await fetch(`/sessions/api/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/detail`);
  if (!resp.ok) {
    console.error("sessions: load session detail failed", { ns, name, status: resp.status });
    throw new Error(
      resp.status === 403
        ? "you do not have access to this session"
        : resp.status === 401
          ? "you are no longer signed in"
          : "these settings could not be loaded right now",
    );
  }
  return (await resp.json()) as SessionDetail;
}

// Row is one labeled field in the info panel (label left, value right). Most
// callers pass a plain string `value`; a caller needing richer markup (e.g.
// <ModelName>) passes `children` in its place — exactly one of the two.
function Row({ label, value, children }: { label: string; value?: string; children?: React.ReactNode }) {
  return (
    <div className="flex items-start justify-between gap-4 py-1.5 text-sm">
      <span className="shrink-0 text-muted-foreground">{label}</span>
      <span className="break-all text-right font-mono text-[13px]">{children ?? value}</span>
    </div>
  );
}

// SessionInfoPanel is the chrome-owned "session details" surface: a modal
// showing the selected session's resolved settings — agent class, model,
// budget (max tokens/turns/duration), owner, created, phase. It refetches
// GET /sessions/api/{ns}/{name}/detail lazily each time it opens (no stale
// cache), mirroring what Slack's "Show settings" surface shows.
//
// It deliberately does NOT render the session's own object name, which the
// detail payload also carries: that name is control-plane vocabulary, and the
// panel is already scoped to the one session the viewer has open — the shell's
// session list names it by its AGENT, which is the name a person recognizes.
export function SessionInfoPanel({
  ns,
  name,
  open,
  onOpenChange,
}: {
  ns: string;
  name: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const [detail, setDetail] = useState<SessionDetail | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setDetail(null);
    setError(null);
    fetchDetail(ns, name)
      .then((d) => {
        if (!cancelled) setDetail(d);
      })
      .catch((e: Error) => {
        if (!cancelled) setError(e.message || "these settings could not be loaded right now");
      });
    return () => {
      cancelled = true;
    };
  }, [open, ns, name]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Session details</DialogTitle>
          <DialogDescription>Resolved settings for this conversation.</DialogDescription>
        </DialogHeader>
        {error ? (
          <div className="text-sm text-destructive" data-testid="session-shell-session-info-error">
            Couldn't load details: {error}
          </div>
        ) : !detail ? (
          <div className="text-sm text-muted-foreground">Loading…</div>
        ) : (
          <div className="divide-y divide-border">
            <Row label="Agent class" value={detail.agentClass} />
            <Row label="Model">
              {detail.model.name ? (
                <ModelName model={`${detail.model.provider || "?"}/${detail.model.name}`} />
              ) : (
                "not resolved"
              )}
            </Row>
            <Row label="Max tokens" value={detail.maxTokens ? String(detail.maxTokens) : "—"} />
            <Row label="Max turns" value={detail.maxTurns ? String(detail.maxTurns) : "—"} />
            <Row label="Max duration" value={detail.maxDuration || "—"} />
            <Row label="Owner" value={detail.owner} />
            <Row label="Phase" value={detail.phase || "—"} />
            <Row label="Created" value={new Date(detail.createdAt).toLocaleString()} />
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}
