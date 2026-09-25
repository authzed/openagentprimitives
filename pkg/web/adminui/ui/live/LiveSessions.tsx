import * as React from "react";
import {
  Alert, AlertDescription,
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@ap/design";
import { getJSON, openSessionStream, sessionKey, type SessionState } from "../lib/api";
import { navigate } from "../lib/router";
import { useCookieState } from "../lib/cookies";
import { ViewToggle } from "../lib/ViewToggle";
import { ResourceName } from "../lib/ResourceName";
import { EntityLink } from "../lib/EntityLink";
import { KindBadge, KindDot } from "../lib/KindBadge";
import { decodeSubject } from "../lib/decodeSubject";
import { fmtTokens, fmtRelative } from "../lib/fmt";
import { SessionCard, PhasePill } from "./SessionCard";

// openSessionPage navigates to a session's detail PAGE (no more overlay).
function openSessionPage(s: SessionState): void {
  navigate({ type: "detail", entity: "session", id: `${s.namespace}/${s.name}` });
}

// KindFilter is the "All | <channelKind…>" chip row narrowing the session list
// by transport. Empty string ("") is the All selection. Rendered only when at
// least one distinct kind exists.
function KindFilter({ kinds, value, onChange }: { kinds: string[]; value: string; onChange: (v: string) => void }) {
  const base = "rounded-full border px-2 py-0.5 text-[10px] transition-colors";
  const inactive = "border-border bg-background text-muted-foreground hover:bg-accent/50";
  const active = "border-primary/40 bg-primary/10 text-primary";
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      <button type="button" aria-pressed={value === ""} onClick={() => onChange("")} className={`${base} ${value === "" ? active : inactive}`}>
        All
      </button>
      {kinds.map((k) => {
        const on = value === k;
        return (
          <button key={k} type="button" aria-pressed={on} onClick={() => onChange(on ? "" : k)} className={`${base} ${on ? active : inactive}`}>
            {k}
          </button>
        );
      })}
    </div>
  );
}

export function LiveSessions({ apiBase }: { apiBase: string }) {
  const [sessions, setSessions] = React.useState<Map<string, SessionState>>(new Map());
  const [stale, setStale] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [kind, setKind] = React.useState("");
  const [mode, setMode] = useCookieState<"grid" | "table">("sessions_view", "grid");

  React.useEffect(() => {
    let closed = false;
    // Frames the live stream applied while a GET /sessions was in flight, in
    // arrival order (null = a `remove`). The GET's list was computed before
    // them, so they are replayed on top of its result — otherwise the GET
    // resurrects a just-deleted row, and DeleteSession broadcasts no follow-up
    // upsert, so nothing would ever correct it.
    const sinceLoad = new Map<string, SessionState | null>();
    let loadSeq = 0;

    // load reconciles against the authoritative list. It is the only path that
    // DROPS a row the server no longer has — the stream cannot, because the
    // server's reconnect snapshot arrives as plain `session` frames that can
    // only re-add — and the only one that surfaces a hard API error, since the
    // stream's onerror just marks the feed stale.
    const load = () => {
      const seq = ++loadSeq;
      sinceLoad.clear();
      getJSON<SessionState[]>(`${apiBase}/sessions`)
        .then((list) => {
          if (closed || seq !== loadSeq) return; // a newer load supersedes this one
          const next = new Map(list.map((s) => [sessionKey(s), s]));
          for (const [key, s] of sinceLoad) {
            if (s) next.set(key, s);
            else next.delete(key);
          }
          sinceLoad.clear();
          setSessions(next);
          setError(null);
        })
        .catch((e: Error) => {
          if (!closed) setError(e.message);
        });
    };
    load();

    // The browser silently reconnects a dropped EventSource, and openSessionStream
    // reports the round trip as stale=true then stale=false. Reconcile on that
    // transition: a session that ended while we were disconnected is simply
    // ABSENT from the reconnect snapshot, and absence is not a signal a merge
    // can act on — without this it keeps its stale phase until a page reload.
    let wasStale = false;
    const close = openSessionStream(apiBase, {
      onSession: (s) => {
        sinceLoad.set(sessionKey(s), s);
        setSessions((prev) => new Map(prev).set(sessionKey(s), s));
      },
      onRemove: (ref) => {
        sinceLoad.set(sessionKey(ref), null);
        setSessions((prev) => {
          const next = new Map(prev);
          next.delete(sessionKey(ref));
          return next;
        });
      },
      onStale: (v) => {
        setStale(v);
        if (!v && wasStale) load();
        wasStale = v;
      },
    });
    return () => {
      closed = true;
      close();
    };
  }, [apiBase]);

  const all = [...sessions.values()].sort((a, b) => (b.startedAt ?? "").localeCompare(a.startedAt ?? ""));
  const kinds = [...new Set(all.map((s) => s.channelKind).filter((k): k is string => !!k))].sort();
  const list = kind ? all.filter((s) => s.channelKind === kind) : all;

  return (
    <div className="space-y-4">
      {error && (
        <Alert variant="destructive">
          <AlertDescription>Could not load sessions: {error}</AlertDescription>
        </Alert>
      )}
      {stale && (
        <Alert>
          <AlertDescription>Live feed stalled — showing last known state.</AlertDescription>
        </Alert>
      )}

      <div className="flex items-center justify-between gap-4">
        {kinds.length > 0 ? <KindFilter kinds={kinds} value={kind} onChange={setKind} /> : <span />}
        <ViewToggle value={mode} onChange={setMode} />
      </div>

      {list.length === 0 && !error && (
        <p className="text-sm text-muted-foreground">{all.length === 0 ? "No sessions." : "No sessions match this filter."}</p>
      )}

      {list.length > 0 && mode === "grid" && (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          {list.map((s) => (
            <SessionCard key={sessionKey(s)} s={s} onOpen={() => openSessionPage(s)} />
          ))}
        </div>
      )}

      {list.length > 0 && mode === "table" && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Session</TableHead>
              <TableHead>Class</TableHead>
              <TableHead>Channel</TableHead>
              <TableHead>Phase</TableHead>
              <TableHead>Tokens</TableHead>
              <TableHead>Started</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {list.map((s) => (
              <TableRow
                key={sessionKey(s)}
                className="cursor-pointer"
                onClick={() => openSessionPage(s)}
              >
                <TableCell className="font-mono text-xs text-foreground">
                  <ResourceName id={sessionKey(s)} />
                </TableCell>
                <TableCell className="font-mono text-xs">
                  {s.class ? (
                    <EntityLink
                      entity="agent"
                      id={`${s.namespace}/${s.class}`}
                      className="text-link hover:underline"
                      onClick={(e) => e.stopPropagation()}
                    >
                      {s.class}
                    </EntityLink>
                  ) : (
                    <span className="text-muted-foreground">—</span>
                  )}
                </TableCell>
                <TableCell className="text-xs">
                  {s.channelName ? (
                    <EntityLink
                      entity="channel"
                      id={`${s.namespace}/${s.channelName}`}
                      className="font-mono text-link hover:underline"
                      onClick={(e) => e.stopPropagation()}
                    >
                      <ResourceName ns={s.namespace} name={s.channelName} />
                    </EntityLink>
                  ) : (
                    !s.channelKind && <span className="font-mono text-muted-foreground">—</span>
                  )}
                  {/* Channel kind as a stable color-coded badge (same color per
                      kind everywhere), scoped so it doesn't collide with the
                      KindFilter chip above the table. */}
                  {s.channelKind && (
                    <KindBadge kind={s.channelKind} className={s.channelName ? "ml-1.5" : ""} />
                  )}
                </TableCell>
                <TableCell><PhasePill phase={s.phase} /></TableCell>
                <TableCell className="whitespace-nowrap font-mono text-xs text-muted-foreground">
                  {fmtTokens(s.inputTokens)} in · {fmtTokens(s.outputTokens)} out
                </TableCell>
                <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                  {fmtRelative(s.startedAt)}
                  {s.startedBy && (
                    <span className="mt-0.5 flex items-center gap-1.5 text-[10px] text-muted-foreground/70">
                      {/* stable per-subject color dot so the same user reads the
                          same color across the console (matches the rollups) */}
                      <KindDot kind={s.startedBy} />
                      <EntityLink entity="user" id={s.startedBy} className="font-mono text-link hover:underline">
                        {decodeSubject(s.startedBy)}
                      </EntityLink>
                    </span>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  );
}
