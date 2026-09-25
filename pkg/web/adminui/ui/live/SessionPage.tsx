import * as React from "react";
import {
  Alert, AlertDescription, Badge, Button,
  Dialog, DialogClose, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger,
  ModelName,
} from "@ap/design";
import { DetailPage, type DetailTab } from "../shell/DetailPage";
import { navigate } from "../lib/router";
import { ResourceName } from "../lib/ResourceName";
import { EntityLink } from "../lib/EntityLink";
import {
  ApiError, del, getJSON, getSessionLogs,
  type BundleSandbox, type LogBlock, type SessionDetail as Detail, type SessionLogEntry, type SessionLogsResponse,
} from "../lib/api";
import { fmtTokens } from "../lib/fmt";
import { decodeSubject } from "../lib/decodeSubject";
import { JSONView, tryParseJSONContainer } from "../lib/JSONView";
import { FlowGraph } from "./FlowGraph";

const TABS: DetailTab[] = [
  { id: "overview", label: "Overview" },
  { id: "activity", label: "Recent Activity" },
  { id: "logs", label: "Full Logs" },
];

// splitId splits an "ns/name" session id into its two segments. Namespaces and
// names never contain "/", so a single split on the first slash is exact.
function splitId(id: string): { ns: string; name: string } {
  const i = id.indexOf("/");
  return i < 0 ? { ns: "", name: id } : { ns: id.slice(0, i), name: id.slice(i + 1) };
}

// toolsFromRecent derives the live tool set from the ring buffer's tool_activity
// payloads ({tool: string}) — the "what is it touching right now" set.
function toolsFromRecent(d: Detail): string[] {
  const set = new Set<string>();
  for (const ev of d.recentEvents) {
    if (ev.kind === "tool_activity") {
      const tool = (ev.payload as { tool?: string } | undefined)?.tool;
      if (tool) set.add(tool);
    }
  }
  return [...set];
}

// SessionPage is the AgentSession detail page (Overview / Recent Activity / Full
// Logs tabs). It reuses the FlowGraph + fetch + kill logic that previously lived
// in the SessionDetail overlay, promoted to a deep-linkable page.
export function SessionPage({ apiBase, id, tab }: { apiBase: string; id: string; tab?: string }) {
  const { ns, name } = splitId(id);
  const activeTab = tab ?? "overview";
  const url = `${apiBase}/sessions/${ns}/${name}`;

  const [detail, setDetail] = React.useState<Detail | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [notTracked, setNotTracked] = React.useState(false);
  const [killNote, setKillNote] = React.useState<string | null>(null);
  // Kill failures route here (not into `error`) so a failed kill surfaces inline
  // on the Overview instead of replacing the whole tab with the fetch-error box.
  const [killError, setKillError] = React.useState<string | null>(null);
  const [killOpen, setKillOpen] = React.useState(false);
  const closeTimerRef = React.useRef<ReturnType<typeof setTimeout> | null>(null);

  React.useEffect(() => {
    return () => { if (closeTimerRef.current !== null) clearTimeout(closeTimerRef.current); };
  }, []);

  React.useEffect(() => {
    let active = true;
    setDetail(null);
    setError(null);
    setNotTracked(false);
    getJSON<Detail>(url)
      .then((d) => { if (active) setDetail(d); })
      .catch((e: Error) => {
        if (!active) return;
        if (e instanceof ApiError && e.status === 404) setNotTracked(true);
        else setError(e.message);
      });
    return () => { active = false; };
  }, [url]);

  const kill = async () => {
    setKillError(null);
    try {
      await del(url); // 404 = already gone — treated as success by del()
      // Close the confirm dialog so the success note isn't hidden behind the
      // modal, then give the note a beat to render before returning to the list.
      setKillOpen(false);
      setKillNote("Session terminated.");
      closeTimerRef.current = setTimeout(() => navigate({ type: "view", view: "sessions" }), 800);
    } catch (e) {
      setKillOpen(false);
      setKillError((e as Error).message);
    }
  };

  const onTab = (t: string) => navigate({ type: "detail", entity: "session", id, tab: t });

  return (
    <DetailPage
      title={
        <span className="font-mono">
          {detail && `${detail.class ?? "?"} / `}
          <ResourceName ns={ns} name={name} />
        </span>
      }
      backRoute={{ type: "view", view: "sessions" }}
      tabs={TABS}
      activeTab={activeTab}
      onTab={onTab}
      headerRight={detail?.phase ? <Badge variant="outline">{detail.phase}</Badge> : undefined}
    >
      {activeTab === "logs" ? (
        <FullLogs apiBase={apiBase} ns={ns} name={name} />
      ) : error ? (
        <Alert variant="destructive">
          <AlertDescription>Could not load session: {error}</AlertDescription>
        </Alert>
      ) : notTracked ? (
        <p className="text-sm text-muted-foreground">Session {id} is not tracked — it may have completed or never started.</p>
      ) : !detail ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : activeTab === "activity" ? (
        <Activity detail={detail} />
      ) : (
        <Overview
          detail={detail}
          name={name}
          killNote={killNote}
          killError={killError}
          killOpen={killOpen}
          onKillOpenChange={setKillOpen}
          onKill={kill}
        />
      )}
    </DetailPage>
  );
}

// Overview is the flow graph, the token/turn counters, the resolved model and
// start time, and the kill affordance.
function Overview({
  detail, name, killNote, killError, killOpen, onKillOpenChange, onKill,
}: {
  detail: Detail;
  name: string;
  killNote: string | null;
  killError: string | null;
  killOpen: boolean;
  onKillOpenChange: (open: boolean) => void;
  onKill: () => void;
}) {
  return (
    <div className="flex flex-col gap-4">
      {killNote && <p className="text-sm text-success">{killNote}</p>}
      {killError && (
        <Alert variant="destructive">
          <AlertDescription>Could not kill session: {killError}</AlertDescription>
        </Alert>
      )}

      <div className="rounded-lg border border-border bg-card p-4">
        <FlowGraph
          channelKind={detail.channelKind}
          agentClass={detail.class}
          tools={toolsFromRecent(detail)}
          active={detail.active}
        />
      </div>

      {detail.statusText && <p className="font-mono text-sm text-muted-foreground">{detail.statusText}</p>}
      <p className="font-mono text-xs text-muted-foreground">
        {fmtTokens(detail.inputTokens)} in · {fmtTokens(detail.outputTokens)} out · {detail.toolCallCount} tool calls · turn {detail.turnCount}
      </p>

      <dl className="grid grid-cols-2 gap-x-6 gap-y-2 text-xs sm:grid-cols-3">
        <Fact label="Phase" value={detail.phase ?? "—"} />
        <Fact label="Model">{detail.model ? <ModelName model={detail.model} /> : "—"}</Fact>
        <Fact label="Elapsed" value={`${detail.elapsedSeconds}s`} />
        {detail.startedAt && <Fact label="Started" value={new Date(detail.startedAt).toLocaleString()} />}
        {detail.activityCause && <Fact label="Activity" value={detail.activityCause} />}
      </dl>

      {detail.bundles && detail.bundles.length > 0 && <Bundles bundles={detail.bundles} />}

      <Dialog open={killOpen} onOpenChange={onKillOpenChange}>
        <DialogTrigger asChild>
          <Button variant="destructive" className="mt-2 self-start">Kill session</Button>
        </DialogTrigger>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Kill {name}?</DialogTitle>
            <DialogDescription>
              Deletes the AgentSession — the runner pod is torn down immediately. This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <DialogClose asChild>
              <Button variant="outline">Cancel</Button>
            </DialogClose>
            <Button variant="destructive" onClick={onKill}>Kill it</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

// Fact renders a plain string `value`, or — when the caller needs richer
// markup (e.g. <ModelName>) — `children` in its place. Exactly one of the two
// is expected per call site.
function Fact({ label, value, children }: { label: string; value?: string; children?: React.ReactNode }) {
  return (
    <div className="flex flex-col">
      <dt className="text-[10px] uppercase tracking-wide text-muted-foreground">{label}</dt>
      <dd className="font-mono text-foreground">{children ?? value}</dd>
    </div>
  );
}

// Bundles renders one row per tool bundle this session resolved to a
// SpiceboxSession, each with its OWN sandbox backend — deliberately not
// collapsed to a single session-level value, since a session's bundles can
// legitimately run on different sandbox kinds (e.g. one on `pod`, another on
// `agent-sandbox`).
function Bundles({ bundles }: { bundles: BundleSandbox[] }) {
  return (
    <div className="rounded-lg border border-border bg-card p-4">
      <h3 className="mb-2 text-[10px] uppercase tracking-wide text-muted-foreground">Bundles</h3>
      <ul className="space-y-2">
        {bundles.map((b) => (
          <li key={b.name} className="flex flex-wrap items-center gap-2 font-mono text-xs">
            <span className="font-semibold text-foreground">{b.name}</span>
            <span className="text-muted-foreground">{b.spiceboxSessionName}</span>
            {b.sandboxKind ? (
              <>
                <Badge variant="outline">{b.sandboxKind}</Badge>
                <span className="break-all text-muted-foreground">{b.sandboxRef}</span>
                {/* prewarmed=false is never shown — a cold sandbox on a backend
                    that cannot pre-warm at all would otherwise always read as a
                    spurious warning. */}
                {b.prewarmed && <Badge variant="outline">warm</Badge>}
              </>
            ) : (
              <span className="text-muted-foreground">sandbox not yet resolved</span>
            )}
            {/* workspaceMode: "shared" (RWX claim across bundles) or "isolated"
                (pod-local /work only) — neither is an error state, so both render
                the same neutral badge (unlike prewarmed's "only show when notable"
                rule). */}
            {b.workspaceMode && <Badge variant="outline">{b.workspaceMode} workspace</Badge>}
            {b.phase && <Badge variant={b.phase === "Failed" ? "destructive" : "outline"}>{b.phase}</Badge>}
            {b.reason && <span className="text-muted-foreground">{b.reason}</span>}
          </li>
        ))}
      </ul>
    </div>
  );
}

// Activity renders the ring-buffer recent events newest-first, mirroring the old
// overlay's activity list.
function Activity({ detail }: { detail: Detail }) {
  if (detail.recentEvents.length === 0) {
    return <p className="text-sm text-muted-foreground">No recent activity.</p>;
  }
  return (
    <ul className="space-y-1 font-mono text-xs text-muted-foreground">
      {detail.recentEvents.slice().reverse().map((ev, i) => (
        <li key={i}>{new Date(ev.at).toLocaleTimeString()} · {ev.kind}</li>
      ))}
    </ul>
  );
}

// FullLogs fetches and renders the decoded transcript chronologically. It owns
// its own fetch so the tab only loads when opened.
function FullLogs({ apiBase, ns, name }: { apiBase: string; ns: string; name: string }) {
  const [logs, setLogs] = React.useState<SessionLogsResponse | null>(null);
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    let active = true;
    setLogs(null);
    setError(null);
    getSessionLogs(apiBase, ns, name)
      .then((r) => { if (active) setLogs(r); })
      .catch((e: Error) => { if (active) setError(e.message); });
    return () => { active = false; };
  }, [apiBase, ns, name]);

  if (error) {
    return (
      <Alert variant="destructive">
        <AlertDescription>Could not load logs: {error}</AlertDescription>
      </Alert>
    );
  }
  if (!logs) return <p className="text-sm text-muted-foreground">Loading logs…</p>;
  if (logs.entries.length === 0) return <p className="text-sm text-muted-foreground">No transcript entries yet.</p>;

  return (
    <div className="space-y-3">
      {logs.truncated && (
        <p className="text-xs text-warning">Showing the latest {logs.entries.length} entries — older turns were truncated.</p>
      )}
      <ul className="space-y-2">
        {logs.entries.map((e) => (
          <LogEntry key={e.id} entry={e} />
        ))}
      </ul>
    </div>
  );
}

// roleStyle maps a turn role to a subtle color-coded container + label pill so
// user / assistant / tool turns are visually distinct at a glance. Unknown
// roles (system, or an empty role → the raw kind) fall back to the neutral card.
function roleStyle(role: string): { container: string; badge: string } {
  switch (role) {
    case "user":
    case "inbox":
      return { container: "border-state/40 bg-state/5", badge: "bg-state/15 text-state" };
    case "assistant":
      return { container: "border-primary/40 bg-primary/5", badge: "bg-primary/15 text-primary" };
    case "tool":
      return { container: "border-warning/40 bg-warning/5", badge: "bg-warning/15 text-warning" };
    default:
      return { container: "border-border bg-card", badge: "bg-muted text-muted-foreground" };
  }
}

// LogEntry renders one transcript turn: a role-colored card with a role pill, an
// optional decoded requester — the turn's per-turn author, or (only for
// legacy/inferred turns predating per-turn authorship) the session's
// started-by fallback — a timestamp, and either the structured blocks or —
// when absent — the flat content fallback.
function LogEntry({ entry }: { entry: SessionLogEntry }) {
  const role = entry.role || entry.kind;
  const style = roleStyle(entry.role ?? "");
  return (
    <li className={`rounded-md border p-3 ${style.container}`}>
      <div className="mb-2 flex items-center justify-between gap-2 font-mono text-[11px] text-muted-foreground">
        <span className="flex items-center gap-2">
          <span className={`rounded px-1.5 py-0.5 text-[10px] uppercase tracking-wide ${style.badge}`}>{role}</span>
          {entry.actor && (
            <span className="flex items-center gap-1">
              {entry.actorInferred ? "requested by" : "by"}{" "}
              <EntityLink entity="user" id={entry.actor} className="text-link hover:underline">
                {decodeSubject(entry.actor)}
              </EntityLink>
              {entry.actorInferred && (
                <span
                  className="text-muted-foreground/60"
                  title="Session initiator — this legacy turn predates per-turn authorship."
                >
                  {" "}(session initiator)
                </span>
              )}
            </span>
          )}
        </span>
        <span>{new Date(entry.createdAt).toLocaleString()}</span>
      </div>
      {entry.blocks && entry.blocks.length > 0 ? (
        <div className="space-y-2">
          {entry.blocks.map((b, i) => (
            <Block key={i} block={b} role={entry.role ?? ""} />
          ))}
        </div>
      ) : (
        <TrustAwareText text={entry.content} className="text-foreground" />
      )}
    </li>
  );
}

// Block renders one structured content block. text follows the turn's role
// color; tool_use and tool_result carry their own color-coded frames with a
// JSONView of the input / output.
function Block({ block, role }: { block: LogBlock; role: string }) {
  if (block.type === "tool_use") {
    return (
      <div className="rounded border border-warning/40 bg-warning/5 p-2">
        <div className="mb-1 font-mono text-[11px] text-warning">→ {block.name || "tool"}</div>
        <JSONView value={block.input ?? {}} />
      </div>
    );
  }
  if (block.type === "tool_result") {
    const err = block.isError;
    const frame = err ? "border-destructive/50 bg-destructive/10" : "border-success/40 bg-success/5";
    const label = err ? "text-destructive" : "text-success";
    const untrusted = untrustedResultText(block.content);
    return (
      <div className={`rounded border p-2 ${frame}`}>
        <div className={`mb-1 font-mono text-[11px] ${label}`}>← {err ? "error" : "result"}</div>
        {untrusted !== null ? (
          <TrustAwareText text={untrusted} className="text-foreground/90" />
        ) : (
          <JSONView value={block.content ?? ""} />
        )}
      </div>
    );
  }
  // text (and any unknown type that still carried text) — styled by role.
  const textClass = role === "assistant" ? "text-foreground" : "text-foreground/90";
  return <TrustAwareText text={block.text ?? ""} className={textClass} />;
}

// untrustedResultText returns a tool_result's content as plain text when it is
// (or JSON-string-encodes) a string carrying the runner's
// <untrusted-tool-output> trust markers, so the Full Logs viewer renders it
// trust-aware instead of as a JSON-escaped blob. One level of JSON string
// encoding is unwrapped when present (some results arrive double-encoded, with
// literal surrounding double quotes and \n / \" escapes). Returns null for
// everything else — structured JSON results keep the JSONView tree.
function untrustedResultText(content: unknown): string | null {
  if (typeof content !== "string") return null;
  let s = content;
  if (s.length >= 2 && s.startsWith('"') && s.endsWith('"')) {
    try {
      const parsed: unknown = JSON.parse(s);
      if (typeof parsed === "string") s = parsed;
    } catch {
      // Not actually JSON-encoded — render the string as-is below.
    }
  }
  return s.includes("<untrusted-tool-output") ? s : null;
}

// UNTRUSTED_TAG matches both the open (<untrusted-tool-output nonce="…">) and
// close (</untrusted-tool-output>) trust-boundary markers the runner wraps tool
// output in. The tag markers are rendered muted so the boundary is visible but
// quiet, and the enclosed span is de-emphasized as "untrusted".
const UNTRUSTED_TAG = /<\/?untrusted-tool-output(?:\s[^>]*)?>/g;

// UntrustedChunk renders one span of content enclosed by the trust markers. A
// payload that is a JSON object/array gets the JSONView tree (still visually
// inside the muted trust frame); anything else stays the de-emphasized text
// span.
function UntrustedChunk({ chunk }: { chunk: string }) {
  const parsed = tryParseJSONContainer(chunk);
  if (parsed !== undefined) {
    return <JSONView value={parsed} className="my-1" />;
  }
  return <span className="rounded-sm bg-muted/40 text-muted-foreground">{chunk}</span>;
}

// TrustAwareText renders transcript text, graying out any literal
// <untrusted-tool-output> wrapper tags (and de-emphasizing their enclosed
// content) so the trust boundary reads quietly. An enclosed JSON payload
// renders as a JSONView tree (see UntrustedChunk). Text without the markers
// renders verbatim.
function TrustAwareText({ text, className }: { text: string; className?: string }) {
  const base = `whitespace-pre-wrap break-words font-mono text-xs ${className ?? ""}`;
  if (!text.includes("<untrusted-tool-output")) {
    // Marker-free content that is itself a JSON document (e.g. a system_note
    // like {"delivered":[…]}) reads better as the tree than as one text line.
    const parsed = tryParseJSONContainer(text);
    if (parsed !== undefined) {
      return <JSONView value={parsed} className="my-1" />;
    }
    return <pre className={base}>{text}</pre>;
  }
  const parts: React.ReactNode[] = [];
  let last = 0;
  let inside = false;
  let key = 0;
  const re = new RegExp(UNTRUSTED_TAG);
  let m: RegExpExecArray | null;
  while ((m = re.exec(text)) !== null) {
    const chunk = text.slice(last, m.index);
    if (chunk) {
      parts.push(
        inside ? (
          <UntrustedChunk key={key++} chunk={chunk} />
        ) : (
          <React.Fragment key={key++}>{chunk}</React.Fragment>
        ),
      );
    }
    parts.push(<span key={key++} className="text-muted-foreground/50">{m[0]}</span>);
    inside = !m[0].startsWith("</");
    last = re.lastIndex;
  }
  const tail = text.slice(last);
  if (tail) {
    parts.push(
      inside ? (
        <UntrustedChunk key={key++} chunk={tail} />
      ) : (
        <React.Fragment key={key++}>{tail}</React.Fragment>
      ),
    );
  }
  // A <div> (not <pre>) because an enclosed JSON payload renders as JSONView's
  // block-level tree; whitespace-pre-wrap preserves the text chunks' newlines.
  return <div className={base}>{parts}</div>;
}
