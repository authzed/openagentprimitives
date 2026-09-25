import { useCallback, useEffect, useRef, useState, type KeyboardEvent } from "react";
import {
  AlertCircle,
  AlertTriangle,
  CheckCircle2,
  Circle,
  CircleDollarSign,
  Clock,
  Download,
  Info,
  Loader2,
  Lock,
  MessageCircle,
  SendHorizontal,
  Wifi,
  WifiOff,
  XCircle,
  ListChecks,
} from "lucide-react";
import { Button, Markdown, Tooltip, TooltipContent, TooltipProvider, TooltipTrigger, cn } from "@ap/design";
import type { UIActionResult, UIActionResultToolCall } from "@mcp-ui/client";
import { appToolResponseToCallResult, interactRequestFromUIAction, type AppToolCallResponse, type CallToolResultLike } from "./mcpUiActionSend";

// This app is the session-scoped shared live view (pkg/web/webui/sessionview): a
// read-only-by-default mirror of a session's conversation for a subject who
// can interact with it but did not originate it from its own channel (Slack,
// etc). It has no artifact and no revisions — just the transcript, a live
// status line, and (when the class grants it) a compose box. Mirrors
// pkg/web/webui/artifactview/ui's chat panel + pkg/web/webui/chat/ui's transcript,
// trimmed to what this page actually needs.

// WidgetProp mirrors pkg/web/webui/sessionview/page.go's widgetProp: one MCP-UI
// interactive widget persisted before this page loaded (status.activeWidgets),
// with an already-minted /mcpui-host URL — the browser holds no signing key,
// so the content-capability token is always minted server-side.
interface WidgetProp {
  artifactId: string;
  hostUrl: string;
}

// SessionViewProps mirrors pkg/web/webui/sessionview/page.go's sessionViewProps —
// the ENTIRE server-rendered bootstrap. Everything else (history, live
// updates, the interactions this session's class grants) is fetched/opened
// client-side. sandboxOrigin/activeWidgets are optional so this type still
// matches props from before Plan 4 Task 2 landed (defensive — mount() always
// passes the real server props in practice).
export interface SessionViewProps {
  ns: string;
  name: string;
  sandboxOrigin?: string;
  activeWidgets?: WidgetProp[];
}

// ConnState is the live-socket connection state surfaced by the header's
// connection indicator. Mirrors artifactview/chat's ConnState.
type ConnState = "connecting" | "connected" | "reconnecting" | "offline";

// PlanItemRef mirrors channelevents.PlanItemRef (only the fields this view
// renders — a checklist label + status glyph).
interface PlanItemRef {
  id: string;
  label: string;
  status: string; // pending | in_progress | done | error (see isDoneStatus)
}

// PlanPayload mirrors channelevents.PlanUpdatePayload (trimmed to what this
// view reads: the snapshot is authoritative, so Diff is not needed here).
interface PlanPayload {
  planName: string;
  items: PlanItemRef[] | null;
}

// AttachmentRef mirrors channelevents.AttachmentRef (trimmed to what a
// download chip needs).
interface AttachmentRef {
  artifactId?: string;
  mime?: string;
  filename: string;
}

// OutboundUserMessagePayload mirrors channelevents.OutboundUserMessagePayload.
// Despite the envelope kind name ("user_message"), this carries the AGENT's
// finalized reply — see channelevents.KindUserMessage's doc comment.
interface OutboundUserMessagePayload {
  text: string;
  attachments?: AttachmentRef[];
}

// UserEchoPayload mirrors channelevents.UserEchoPayload: a message another
// view of this session (or this page's own compose box) sent, mirrored back
// so every mirror of the conversation shows the same thing.
interface UserEchoPayload {
  text: string;
}

// NotificationPayload mirrors channelevents.NotificationPayload (trimmed).
interface NotificationPayload {
  text: string;
}

// TurnActivityPayload mirrors channelevents.TurnActivityPayload: the coarse
// active/paused turn signal that drives the working indicator.
interface TurnActivityPayload {
  active: boolean;
}

// WidgetOfferClientPayload mirrors sessionview's widgetOfferClientPayload
// (live.go): channelevents.WidgetOfferPayload PLUS a server-minted
// /mcpui-host URL. The raw envelope carries only the artifact identity — the
// browser holds no signing key, so live.go rewrites the payload at relay
// time; hostUrl is "" when that mint failed (logged server-side), in which
// case this view has nothing to frame yet.
interface WidgetOfferClientPayload {
  artifactId: string;
  tool?: string;
  rendererKind?: string;
  hostUrl: string;
}

// Envelope mirrors channelevents.Envelope (trimmed to the fields this view
// reads: `kind` selects how `payload` is interpreted).
interface Envelope {
  kind: string;
  payload: unknown;
}

// InteractionExcerpt mirrors channelevents.InteractionExcerpt: UNTRUSTED
// content shown for human judgment. CONTRACT: it MUST render inert — fenced
// inside a <pre>/<code> block, never interpreted as markup. Crossing a wire
// does not launder it.
interface InteractionExcerpt {
  label?: string;
  content: string;
}

// InteractionField mirrors channelevents.InteractionField: one label/value
// supporting row. Publisher-authored and trusted (unlike the excerpt).
interface InteractionField {
  label: string;
  value: string;
}

// NoticeWire mirrors channelevents.NoticeWire — the one-way, user-facing
// message a POST /session/{ns}/{name}/interact answers a 200 with
// (interactResponse.Notice in pkg/web/webui/interact/handlers.go). It rides on the
// NON-routed outcomes (refused, denied by permission, fork pending) and on
// takeover/inherit continuations, and it is this page's ONLY copy of that
// message — nothing is published over the live socket for those paths, so not
// reading it is literal silence after the user presses Send.
//
// `tone` is denormalised from the category (channelinteractions.Tone) exactly
// so a surface can pick a colour with no registry — see NOTICE_TONE_STYLES.
interface NoticeWire {
  category?: string;
  tone?: string;
  terminal?: boolean;
  glyph?: string;
  lead: string;
  body?: string;
  nextStep?: string;
  fields?: InteractionField[];
  excerpt?: InteractionExcerpt;
}

// TimelineEntry mirrors pkg/web/webui/livemirror.TimelineEntry: one ordered item
// of the history snapshot — a message or a plan card.
interface TimelineEntry {
  kind: "message" | "plan";
  role?: "user" | "agent";
  text?: string;
  plan?: PlanPayload;
}

// LiveFrame mirrors pkg/web/webui/sessionview/live.go's liveMessage: "snapshot"
// (the initial history replay, History set) or "event" (one live outbound
// envelope, Event set).
type LiveFrame = { type: "snapshot"; history?: TimelineEntry[] } | { type: "event"; event?: Envelope };

// parseLiveFrame safely parses one server frame; returns null on bad JSON or
// a frame with no type (so the caller can ignore it without throwing).
function parseLiveFrame(raw: string): LiveFrame | null {
  try {
    const m = JSON.parse(raw) as LiveFrame;
    if (!m || typeof m.type !== "string") return null;
    return m;
  } catch {
    return null;
  }
}

// wsURLFor builds the ws(s):// URL for this session's live socket —
// GET /session-view/{ns}/{name}/live (see sessionview.go's Routes).
function wsURLFor(loc: { protocol: string; host: string }, ns: string, name: string): string {
  const proto = loc.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${loc.host}/session-view/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/live`;
}

// OFFLINE_AFTER is the consecutive-failure count past which the indicator
// stops claiming "reconnecting" and reports a sustained outage. Retries
// continue in the background regardless. Mirrors artifactview/chat.
const OFFLINE_AFTER = 3;

// useLiveSocket connects to the session-view live socket, calls onFrame for
// each parsed frame, and exposes a connection state with a 2s-backoff
// reconnect — mirrors artifactview's useLiveSocket / chat's useChatSocket.
// A reconnect naturally resyncs the whole transcript: every new connection
// gets a fresh "snapshot" frame from the server (see live.go's
// runLiveMirror), so there is no separate resync path to maintain here.
function useLiveSocket(ns: string, name: string, onFrame: (f: LiveFrame) => void): { state: ConnState } {
  const [state, setState] = useState<ConnState>("connecting");
  const cbRef = useRef(onFrame);
  cbRef.current = onFrame;

  useEffect(() => {
    let ws: WebSocket | null = null;
    let timer: number | undefined;
    let closed = false;
    let failures = 0;

    const onDrop = () => {
      if (closed) return;
      failures += 1;
      setState(failures >= OFFLINE_AFTER ? "offline" : "reconnecting");
      timer = window.setTimeout(connect, 2000);
    };

    const connect = () => {
      if (closed) return;
      setState(failures === 0 ? "connecting" : failures >= OFFLINE_AFTER ? "offline" : "reconnecting");
      try {
        ws = new WebSocket(wsURLFor(window.location, ns, name));
      } catch {
        onDrop();
        return;
      }
      ws.onopen = () => {
        failures = 0;
        setState("connected");
      };
      ws.onmessage = (ev) => {
        const f = parseLiveFrame(ev.data as string);
        if (f) cbRef.current(f);
      };
      ws.onerror = () => {
        if (!closed) setState(failures >= OFFLINE_AFTER ? "offline" : "reconnecting");
      };
      ws.onclose = onDrop;
    };
    connect();

    return () => {
      closed = true;
      if (timer) window.clearTimeout(timer);
      if (ws) ws.close();
    };
  }, [ns, name]);

  return { state };
}

// connMeta maps each socket state to its header presentation. Mirrors
// chat/artifactview's ConnectionIndicator.
const connMeta: Record<ConnState, { Icon: typeof Wifi; color: string; spin: boolean; tip: string }> = {
  connected: { Icon: Wifi, color: "text-success", spin: false, tip: "Live — connected" },
  connecting: { Icon: Loader2, color: "text-warning", spin: true, tip: "Connecting…" },
  reconnecting: { Icon: Loader2, color: "text-warning", spin: true, tip: "Reconnecting…" },
  offline: { Icon: WifiOff, color: "text-destructive", spin: false, tip: "Offline — retrying" },
};

function ConnectionIndicator({ state }: { state: ConnState }) {
  const meta = connMeta[state];
  const { Icon } = meta;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <div className={`flex h-7 w-7 items-center justify-center rounded-md cursor-default ${meta.color}`}>
          <Icon className={`h-[18px] w-[18px] ${meta.spin ? "animate-spin" : ""}`} aria-hidden="true" />
        </div>
      </TooltipTrigger>
      <TooltipContent>{meta.tip}</TooltipContent>
    </Tooltip>
  );
}

// Line is this view's local rendered-timeline model — one entry per user
// turn, agent reply, plan snapshot, system/error note, or MCP-UI widget.
// Never sent to the server; built entirely from the history snapshot + live
// envelopes + the activeWidgets bootstrap prop.
interface Line {
  id: string;
  role: "user" | "agent" | "system" | "error" | "plan" | "widget";
  text: string;
  plan?: PlanPayload;
  attachments?: AttachmentRef[];
  // hostUrl is set only for role "widget": the sandbox-origin /mcpui-host
  // URL this widget's OUTER iframe frames (see WidgetFrame below).
  hostUrl?: string;
}

// widgetLineID is the stable Line.id for one widget's timeline entry, keyed
// by artifactId so a bootstrap-seeded widget and its (potential) later live
// widget_offer echo never produce two entries for the same artifact.
function widgetLineID(artifactId: string): string {
  return `widget:${artifactId}`;
}

// artifactIdFromWidgetLineID is widgetLineID's inverse. The app-tool-call POST
// carries WHICH widget is calling so the server can look up what that widget
// is (its MCPServer origin, from the session's own status.activeWidgets) and
// refuse a call reaching for another server's tool. The frame map is already
// keyed by Line.id, so the id is right here — it was simply being dropped once
// the source match succeeded.
//
// Returns "" for an id that is not a widget line, which leaves the call
// unpinned exactly as a non-widget caller is. Only ids the frame map produced
// reach this, so that branch is unreachable in practice.
function artifactIdFromWidgetLineID(lineId: string): string {
  return lineId.startsWith("widget:") ? lineId.slice("widget:".length) : "";
}

// RegisterWidgetFrame threads one widget's OUTER iframe element into
// SessionView's per-widget-id source map, keyed by Line.id (widgetLineID) —
// see the widgetFramesRef doc in SessionView below for why a map, not a
// single ref, is required here (unlike ArtifactView.tsx's single artifact
// iframe).
type RegisterWidgetFrame = (id: string, el: HTMLIFrameElement | null) => void;

// WidgetFrame renders one MCP-UI interactive widget: the OUTER frame to the
// sandbox-origin host (mirrors ArtifactView.tsx's outer iframe — sandbox=
// "allow-scripts allow-same-origin" is safe here because the framed
// document is a DIFFERENT origin (the sandbox base URL), not because this
// frame is trusted to run the widget's own script; /mcpui-host's OWN inner
// iframe is what isolates the widget's script — see widgets.go).
function WidgetFrame({ id, hostUrl, registerFrame }: { id: string; hostUrl: string; registerFrame: RegisterWidgetFrame }) {
  return (
    <div className="mx-auto w-full max-w-[85%] rounded-lg border border-border overflow-hidden bg-card/60" style={{ height: 480 }}>
      <iframe
        ref={(el) => registerFrame(id, el)}
        sandbox="allow-scripts allow-same-origin"
        src={hostUrl}
        referrerPolicy="no-referrer"
        title="interactive widget"
        className="w-full h-full border-0 block bg-white"
      />
    </div>
  );
}

// isDoneStatus is the "this plan step is terminally finished" predicate —
// mirrors chat/ui/MessageList.tsx's isDoneStatus.
function isDoneStatus(status: string): boolean {
  return status === "done" || status === "complete" || status === "completed" || status === "skipped";
}

// planItemIcon maps a plan item's status to its checklist glyph. `live` gates
// the in-progress spin: only spin while the turn is actually active.
function planItemIcon(status: string, live: boolean) {
  if (isDoneStatus(status)) {
    return <CheckCircle2 className="h-3.5 w-3.5 shrink-0 text-success" aria-hidden="true" />;
  }
  switch (status) {
    case "in_progress":
    case "active":
    case "running":
      return <Loader2 className={cn("h-3.5 w-3.5 shrink-0 text-primary", live && "animate-spin")} aria-hidden="true" />;
    case "blocked":
    case "failed":
    case "error":
      return <XCircle className="h-3.5 w-3.5 shrink-0 text-destructive" aria-hidden="true" />;
    default:
      return <Circle className="h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden="true" />;
  }
}

// PlanBlock renders a plan snapshot as a checklist card. Mirrors chat's
// PlanBlock, trimmed (no parent-plan nesting hint — this view is read-mostly).
function PlanBlock({ plan, live }: { plan: PlanPayload; live: boolean }) {
  const items: PlanItemRef[] = plan.items ?? [];
  return (
    <div className="mx-auto w-full max-w-[85%] rounded-lg border border-border bg-card/60 px-3 py-2">
      <div className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground mb-1.5">
        <ListChecks className="h-3.5 w-3.5" aria-hidden="true" />
        <span>Plan{items.length > 0 ? ` · ${items.length} step${items.length === 1 ? "" : "s"}` : ""}</span>
      </div>
      {items.length === 0 ? (
        <div className="text-xs text-muted-foreground">Plan updated.</div>
      ) : (
        <ul className="flex flex-col gap-1">
          {items.map((it) => {
            const dim = isDoneStatus(it.status);
            return (
              <li key={it.id} className="flex items-start gap-1.5 text-xs leading-snug">
                <span className="mt-0.5">{planItemIcon(it.status, live)}</span>
                <span className={cn("break-words", dim ? "text-muted-foreground" : "text-card-foreground")}>{it.label}</span>
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}

// Bubble is the shared agent/user speech-bubble shell — mirrors chat's Bubble.
function Bubble({ isUser, children }: { isUser: boolean; children: React.ReactNode }) {
  return (
    <div className={cn("flex", isUser ? "justify-end" : "justify-start")}>
      <div
        className={cn(
          "max-w-[75%] rounded-2xl px-3.5 py-2 text-sm leading-relaxed",
          isUser
            ? "bg-primary text-primary-foreground rounded-br-sm"
            : "bg-card border border-border text-card-foreground rounded-bl-sm",
        )}
      >
        {children}
      </div>
    </div>
  );
}

// AttachmentChips renders each attached artifact as a same-origin download
// link — mirrors chat's AttachmentChips.
function AttachmentChips({ ns, name, attachments }: { ns: string; name: string; attachments: AttachmentRef[] }) {
  return (
    <div className="flex flex-wrap gap-1.5">
      {attachments.map((a, i) => {
        const params = new URLSearchParams({ artifactId: a.artifactId ?? "", sessionRef: `${ns}/${name}`, fn: a.filename });
        return (
          <a
            key={i}
            href={`/artifact-download?${params.toString()}`}
            download={a.filename}
            className="inline-flex items-center gap-1.5 rounded-lg border border-border bg-card px-2.5 py-1 text-xs text-card-foreground hover:bg-accent hover:text-accent-foreground"
          >
            <Download className="h-3.5 w-3.5 shrink-0" aria-hidden="true" />
            <span className="break-all">{a.filename}</span>
          </a>
        );
      })}
    </div>
  );
}

// TimelineLine renders one committed entry by role.
function TimelineLine({
  line,
  ns,
  name,
  live,
  registerWidgetFrame,
}: {
  line: Line;
  ns: string;
  name: string;
  live: boolean;
  registerWidgetFrame: RegisterWidgetFrame;
}) {
  if (line.role === "widget") {
    // hostUrl is absent when a live widget_offer's server-side token mint
    // failed (logged server-side, see live.go) — render nothing rather than
    // an iframe with no src.
    return line.hostUrl ? <WidgetFrame id={line.id} hostUrl={line.hostUrl} registerFrame={registerWidgetFrame} /> : null;
  }
  if (line.role === "plan" && line.plan) {
    return <PlanBlock plan={line.plan} live={live} />;
  }
  if (line.role === "system" || line.role === "error") {
    return (
      <div
        className={cn(
          "mx-auto max-w-[85%] text-center text-xs py-1",
          line.role === "error" ? "text-destructive" : "text-muted-foreground",
        )}
      >
        <span className="whitespace-pre-wrap break-words">{line.text}</span>
      </div>
    );
  }
  const attachments = line.attachments ?? [];
  return (
    <div className={cn("flex flex-col gap-1", line.role === "user" ? "items-end" : "items-start")}>
      <Bubble isUser={line.role === "user"}>
        <Markdown>{line.text}</Markdown>
      </Bubble>
      {attachments.length > 0 && <AttachmentChips ns={ns} name={name} attachments={attachments} />}
    </div>
  );
}

// WorkingIndicator is the "agent is working" throbber — mirrors chat's.
function WorkingIndicator() {
  return (
    <div className="flex justify-start" role="status" aria-label="Agent is working">
      <div className="flex items-center gap-2 rounded-2xl rounded-bl-sm border border-border bg-card px-3.5 py-2.5">
        <div className="flex items-center gap-1">
          {[0, 150, 300].map((delay) => (
            <span
              key={delay}
              className="h-1.5 w-1.5 rounded-full bg-muted-foreground animate-bounce"
              style={{ animationDelay: `${delay}ms` }}
            />
          ))}
        </div>
      </div>
    </div>
  );
}

// CaptionLine is the agent's live status caption — mirrors chat's CaptionLine.
function CaptionLine({ text }: { text: string }) {
  return (
    <div className="flex items-center justify-center text-[11px] text-muted-foreground py-0.5">
      <span className="animate-pulse">{text}</span>
    </div>
  );
}

// NOTICE_TONE_STYLES maps a NoticeWire.tone (channelinteractions.Tone,
// denormalised onto the wire precisely so a surface needs no registry) to this
// page's icon + colour. The tones are NOT an ordered severity — privacy is a
// different question, not a hotter degraded (see pkg/channels/channelinteractions/
// tone.go) — so this is a lookup, not a ramp. An unrecognised tone falls back
// to the neutral row: a renderer must never be the reason a message goes
// silent.
const NOTICE_TONE_STYLES: Record<string, { Icon: typeof Info; color: string }> = {
  critical: { Icon: AlertTriangle, color: "text-destructive" },
  privacy: { Icon: Lock, color: "text-warning" },
  degraded: { Icon: AlertCircle, color: "text-warning" },
  waiting: { Icon: Clock, color: "text-muted-foreground" },
  routine: { Icon: Info, color: "text-muted-foreground" },
  housekeeping: { Icon: Info, color: "text-muted-foreground" },
  resolved: { Icon: CheckCircle2, color: "text-success" },
};
const NEUTRAL_NOTICE_TONE = { Icon: Info, color: "text-muted-foreground" };

// NOTICE_GLYPH_ICONS honours NoticeWire.glyph (channelinteractions.Glyph), the
// optional semantic override that REPLACES the tone's icon (its colour is
// kept). An unknown glyph is ignored rather than blanking the mark.
const NOTICE_GLYPH_ICONS: Record<string, typeof Info> = { money: CircleDollarSign, clock: Clock };

// NoticeBanner renders one channelevents.NoticeWire returned by a 200 from
// /interact, in the same slot as the sendError banner — directly above the
// composer, where the user is looking after pressing Send.
//
// It is deliberately NOT folded into sendError: a notice is not an error (a
// continuation into a new session is routine housekeeping), and painting one
// destructive-red would misreport it. Every field is rendered because each is
// load-bearing somewhere — `body` is the whole message for a continuation,
// `excerpt` carries the failure reason for a refusal, and `nextStep` is what
// keeps a stuck user unstuck (Tone.RequiresNextStep makes it mandatory at the
// critical / privacy / degraded tones).
//
// SECURITY CONTRACT: `notice.excerpt` is UNTRUSTED — it renders as literal
// text inside a fenced <pre><code>, never as markup. Every other field is
// publisher-authored/trusted.
function NoticeBanner({ notice }: { notice: NoticeWire }) {
  const tone = NOTICE_TONE_STYLES[notice.tone ?? ""] ?? NEUTRAL_NOTICE_TONE;
  const Icon = NOTICE_GLYPH_ICONS[notice.glyph ?? ""] ?? tone.Icon;
  return (
    <div
      className="flex flex-col gap-1.5 px-4 py-2 text-xs border-t border-border bg-card"
      data-testid="notice-banner"
      data-tone={notice.tone || ""}
    >
      <div className={cn("flex items-center gap-2 font-medium", tone.color)}>
        <Icon className="h-3.5 w-3.5 shrink-0" aria-hidden="true" />
        <span className="break-words text-card-foreground">{notice.lead}</span>
      </div>
      {notice.body && <div className="text-muted-foreground">{notice.body}</div>}
      {notice.fields?.map((f, i) => (
        // Keyed by label+index: two fields can share a label, and a bare-label
        // key would collide and misrender the rows.
        <div key={`${f.label}:${i}`} className="leading-snug">
          <span className="font-medium text-card-foreground">{f.label}:</span>{" "}
          <span className="text-muted-foreground">{f.value}</span>
        </div>
      ))}
      {notice.excerpt && (
        <pre className="max-h-40 overflow-auto whitespace-pre-wrap break-words rounded-md border border-border bg-background px-2 py-1.5 text-[11px] leading-snug text-card-foreground">
          <code>
            {notice.excerpt.label ? `${notice.excerpt.label}:\n` : ""}
            {notice.excerpt.content}
          </code>
        </pre>
      )}
      {notice.nextStep && <div className="font-medium text-card-foreground">{notice.nextStep}</div>}
    </div>
  );
}

// Composer is the message input: a growing textarea plus a send button.
// Enter sends; Shift+Enter inserts a newline. Mirrors chat/ui/ChatInput.tsx.
function Composer({ disabled, placeholder, onSend }: { disabled: boolean; placeholder: string; onSend: (text: string) => void }) {
  const [text, setText] = useState("");

  const submit = () => {
    const trimmed = text.trim();
    if (!trimmed || disabled) return;
    onSend(trimmed);
    setText("");
  };

  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      submit();
    }
  };

  return (
    <div className="flex items-end gap-2 px-4 py-3 border-t border-border bg-card">
      <textarea
        rows={1}
        value={text}
        disabled={disabled}
        placeholder={placeholder}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={onKeyDown}
        className={cn(
          "flex-1 resize-none max-h-40 rounded-md border border-input bg-transparent px-3 py-2 text-sm shadow-sm",
          "placeholder:text-muted-foreground focus:outline-none focus:ring-1 focus:ring-ring",
          "disabled:cursor-not-allowed disabled:opacity-50",
        )}
      />
      <Button size="icon" className="h-9 w-9 shrink-0" disabled={disabled || !text.trim()} onClick={submit} title="Send">
        <SendHorizontal className="h-4 w-4" />
      </Button>
    </div>
  );
}

// timelineToLines maps the history snapshot to this view's Line model — one
// plan card per plan name, everything else a role/text bubble. Shared by the
// initial connect and every reconnect (each opens a fresh socket that gets
// its own fresh snapshot — see useLiveSocket's doc comment above).
function timelineToLines(entries: TimelineEntry[], newId: () => string): Line[] {
  return entries.map((e) =>
    e.kind === "plan" && e.plan
      ? { id: `plan:${e.plan.planName}`, role: "plan", text: "", plan: e.plan }
      : { id: newId(), role: e.role === "user" ? "user" : "agent", text: e.text ?? "" },
  );
}

// SessionView is the session-scoped shared live view: the transcript (history
// snapshot + live updates) plus a compose box that POSTs to
// /session/{ns}/{name}/interact with NO artifactId — see interact/handlers.go
// step 8, which mints the Via from viewurn.TypeSession on precisely that
// (empty-artifactId) branch.
export function SessionView(props: SessionViewProps) {
  const { ns, name } = props;

  const [lines, setLines] = useState<Line[]>([]);
  const [working, setWorking] = useState(false);
  const [notice, setNotice] = useState("");
  const [sending, setSending] = useState(false);
  const [sendError, setSendError] = useState<string | null>(null);
  // sendNotice holds the structured notice a 200 from /interact answered with
  // (interactResponse.Notice). Held apart from sendError because the two are
  // different things — an error is this page failing to send, a notice is the
  // server telling the user what happened to a send it DID accept — and only
  // one of the two is ever set per attempt.
  const [sendNotice, setSendNotice] = useState<NoticeWire | null>(null);

  // surfaceSendFailure and surfaceSendNotice are the ONLY two ways an
  // out-of-band send result reaches the banner slot above the composer. Each
  // reports the outcome of the LATEST attempt and clears the other, so a stale
  // notice can never sit beside a fresh error (or vice versa) — the widget
  // paths have no clear-on-start of their own, unlike onSend. A notice with an
  // empty lead is nothing to draw (NoticeWire.IsZero's exact rule), so it
  // empties the slot rather than rendering a blank banner.
  const surfaceSendFailure = useCallback((msg: string) => {
    setSendNotice(null);
    setSendError(msg);
  }, []);
  const surfaceSendNotice = useCallback((n: NoticeWire | undefined) => {
    setSendError(null);
    setSendNotice(n?.lead ? n : null);
  }, []);

  // turnActiveRef gates a notification/plan frame that lands after the turn
  // already ended, so a late tick can't re-arm the throbber on an idle
  // session — mirrors chat's turnActiveRef.
  const turnActiveRef = useRef(false);

  const nextId = useRef(0);
  const newId = useCallback(() => `l${nextId.current++}`, []);
  const appendLine = useCallback(
    (role: "user" | "agent" | "system" | "error", text: string, attachments?: AttachmentRef[]) => {
      setLines((prev) => [...prev, { id: newId(), role, text, attachments }]);
    },
    [newId],
  );

  const handleFrame = useCallback(
    (f: LiveFrame) => {
      if (f.type === "snapshot") {
        // A fresh snapshot (initial connect OR a post-reconnect resync) is
        // authoritative for the CONVERSATION: replace the transcript and
        // reset the live-turn state, since a durable snapshot carries no "is
        // a turn active right now" signal — the next live frame
        // re-establishes it if so. Widget lines are NOT part of the
        // snapshot (this page has no widget history replay — only the
        // activeWidgets bootstrap prop + live widget_offer events produce
        // them), so any already appended are preserved, re-appended after
        // the replayed history since there's no ordering info to interleave
        // them with.
        setLines((prev) => [...timelineToLines(f.history ?? [], newId), ...prev.filter((l) => l.role === "widget")]);
        turnActiveRef.current = false;
        setWorking(false);
        setNotice("");
        return;
      }
      const env = f.event;
      if (!env) return;
      switch (env.kind) {
        case "user_message": {
          const p = env.payload as OutboundUserMessagePayload;
          const attachments = (p.attachments ?? []).filter((a) => a.artifactId);
          appendLine("agent", p.text, attachments.length ? attachments : undefined);
          break;
        }
        case "user_echo": {
          const p = env.payload as UserEchoPayload;
          appendLine("user", p.text);
          break;
        }
        case "plan_update": {
          const p = env.payload as PlanPayload;
          if (turnActiveRef.current) setWorking(true);
          const planId = `plan:${p.planName}`;
          setLines((prev) => {
            const idx = prev.findIndex((l) => l.role === "plan" && l.id === planId);
            if (idx >= 0) {
              const copy = prev.slice();
              copy[idx] = { ...prev[idx], plan: p };
              return copy;
            }
            return [...prev, { id: planId, role: "plan", text: "", plan: p }];
          });
          break;
        }
        case "notification": {
          if (!turnActiveRef.current) break;
          const p = env.payload as NotificationPayload;
          setWorking(true);
          setNotice(p.text);
          break;
        }
        case "turn_activity": {
          const p = env.payload as TurnActivityPayload;
          if (p.active) {
            turnActiveRef.current = true;
            setWorking(true);
          } else {
            turnActiveRef.current = false;
            setWorking(false);
            setNotice("");
          }
          break;
        }
        case "widget_offer": {
          const p = env.payload as WidgetOfferClientPayload;
          if (!p.hostUrl) break; // server-side mint failed (logged) — nothing to frame yet
          const id = widgetLineID(p.artifactId);
          setLines((prev) => (prev.some((l) => l.id === id) ? prev : [...prev, { id, role: "widget", text: "", hostUrl: p.hostUrl }]));
          break;
        }
        default:
          // Outside this view's rendered vocabulary (this page has no
          // artifact, no interactive-tool surface) — not an error.
          break;
      }
    },
    [appendLine, newId],
  );

  // Bootstrap any MCP-UI widgets persisted BEFORE this page loaded
  // (status.activeWidgets, read server-side in page.go's shellBuild) — a
  // widget offered AFTER load arrives live instead (the "widget_offer" case
  // above). Runs once on mount; a fresh snapshot never wipes these (see the
  // snapshot branch's `prev.filter(l => l.role === "widget")` above), so
  // mount order relative to the first snapshot frame doesn't matter.
  useEffect(() => {
    const widgets = props.activeWidgets ?? [];
    if (widgets.length === 0) return;
    setLines((prev) => {
      const existing = new Set(prev.map((l) => l.id));
      const seeded = widgets
        .filter((w) => !existing.has(widgetLineID(w.artifactId)))
        .map((w) => ({ id: widgetLineID(w.artifactId), role: "widget" as const, text: "", hostUrl: w.hostUrl }));
      return seeded.length ? [...prev, ...seeded] : prev;
    });
    // Bootstrap props are the initial page load only — deliberately runs once.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // widgetFramesRef maps each currently-mounted widget's Line.id -> its outer
  // iframe element. Unlike ArtifactView.tsx (one artifact, one ref), this
  // page can host MULTIPLE WidgetFrames at once, so the trusted-side
  // postMessage listener below needs to check an inbound message's `e.source`
  // against every mounted widget, not a single ref. registerWidgetFrame is
  // the ref callback WidgetFrame's iframe uses to add/remove itself; it never
  // triggers a re-render (a plain ref, not state) since the map's identity is
  // never read by render — only by the listener, which reads it live off the
  // ref on each incoming message.
  const widgetFramesRef = useRef<Map<string, HTMLIFrameElement>>(new Map());
  const registerWidgetFrame = useCallback<RegisterWidgetFrame>((id, el) => {
    if (el) {
      widgetFramesRef.current.set(id, el);
    } else {
      widgetFramesRef.current.delete(id);
    }
  }, []);

  const { state: connState } = useLiveSocket(ns, name, handleFrame);

  // A sustained outage means no further frames will arrive — say so once,
  // rather than leaving the throbber (if any) spinning with no explanation.
  // Mirrors chat's lostShownRef; re-arms once the socket reconnects.
  const lostShownRef = useRef(false);
  useEffect(() => {
    if (connState === "connected") {
      lostShownRef.current = false;
      return;
    }
    if (connState === "offline" && !lostShownRef.current) {
      lostShownRef.current = true;
      appendLine("system", "Connection lost — retrying…");
    }
  }, [connState, appendLine]);

  // onSend POSTs a free-text user_message to /session/{ns}/{name}/interact,
  // session-scoped (no artifactId — see interact/handlers.go step 8's
  // empty-ArtifactID branch, which mints the Via from viewurn.TypeSession
  // gated on CheckInteract only). The sent text is never appended
  // optimistically — it renders once its user_echo round-trips on the live
  // stream, same as artifactview's chat panel.
  const onSend = useCallback(
    async (text: string) => {
      setSendError(null);
      setSendNotice(null);
      setSending(true);
      try {
        const r = await fetch(`/session/${ns}/${name}/interact`, {
          method: "POST",
          credentials: "same-origin",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ kind: "user_message", payload: { text } }),
        });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) {
          const msg = (data as { error?: string }).error || `Failed to send (${r.status}).`;
          surfaceSendFailure(msg);
          return;
        }
        // A 200 carrying a notice means the send was NOT routed (refused,
        // denied, forked) or continued in a new session — the notice is the
        // only account of that the user will ever get, so it must be shown.
        surfaceSendNotice((data as { outcome?: string; notice?: NoticeWire }).notice);
      } catch {
        surfaceSendFailure("Network error while sending your message.");
      } finally {
        setSending(false);
      }
    },
    [ns, name, surfaceSendFailure, surfaceSendNotice],
  );

  // handleAppToolCall answers a widget's CORRELATED `tool` UIAction —
  // mcpuihost/McpUiHost.tsx's sendAppToolCall posted
  // {ap:"mcpui", cmd:"appToolCall", corrId, result} up and is awaiting a
  // matching {ap:"mcpui", cmd:"appToolCallReply", corrId, response} back, on
  // a Promise that only resolves via that reply (or its own ~35s client-side
  // timeout backstop). This function's ONE job is to never leave that
  // Promise unresolved: every path below — the fetch's non-2xx gate
  // failure, the not_found fallback, and a network throw — ends by posting
  // a reply, in addition to (never instead of) surfacing failures via
  // setSendError like onSend/the cmd:"action" branch below do. replyTo/corrId
  // are captured by the caller BEFORE this async function starts (the
  // MessageEvent itself is not guaranteed to stay valid across an await).
  const handleAppToolCall = useCallback(
    async (result: UIActionResultToolCall, corrId: string, replyTo: Window, artifactId: string) => {
      const replyDown = (response: CallToolResultLike) => {
        if (!props.sandboxOrigin) return; // can't happen (this branch is only reachable once sandboxOrigin gated the inbound message) — guard keeps TS happy
        try {
          replyTo.postMessage({ ap: "mcpui", cmd: "appToolCallReply", corrId, response }, props.sandboxOrigin);
        } catch (err) {
          // Mirrors logUIAction's try/catch on the other side of this bridge:
          // no other channel exists to report a postMessage throw here.
          console.error("app-tool-call reply postMessage failed", err);
        }
      };

      try {
        const r = await fetch(`/session/${ns}/${name}/app-tool-call`, {
          method: "POST",
          credentials: "same-origin",
          headers: { "Content-Type": "application/json" },
          // artifactId identifies the CALLING widget. The server resolves it
          // against this session's status.activeWidgets to learn which MCP
          // server authored the widget, and pins the call to that server's
          // tools — the app-tool registry is one flat map across every origin,
          // so without it the tool name alone decided.
          body: JSON.stringify({
            toolName: result.payload?.toolName,
            args: result.payload?.params ?? {},
            artifactId,
          }),
        });

        if (!r.ok) {
          // A non-2xx here is a GATE failure (auth/CSRF/malformed body) —
          // every OTHER outcome (denied, requires_approval, not_found,
          // rate_limited, a successful ok/IsError result) is HTTP 200 per
          // appToolCallHandler's contract.
          const body = (await r.json().catch(() => ({}))) as { error?: string };
          console.error("app-tool-call send failed", r.status, body);
          surfaceSendFailure(body.error || `The widget action could not be sent (${r.status}).`);
          replyDown({ content: [{ type: "text", text: "The action could not be sent." }], isError: true });
          return;
        }

        const resp = (await r.json()) as AppToolCallResponse;

        if (resp.status === "not_found") {
          // Not an app-visible tool (unknown, or not opted into this
          // session's AppTools registry) — fall back to the existing Plan-5
          // /interact turn (the agent disposes), exactly like the
          // cmd:"action" branch below, then ack the widget so its Promise
          // still resolves.
          try {
            const fallback = await fetch(`/session/${ns}/${name}/interact`, {
              method: "POST",
              credentials: "same-origin",
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify(interactRequestFromUIAction(result)),
            });
            if (!fallback.ok) {
              const body = (await fallback.json().catch(() => ({}))) as { error?: string };
              console.error("widget action send failed (not_found fallback)", fallback.status, body);
              surfaceSendFailure(body.error || `The widget action could not be sent (${fallback.status}).`);
            } else {
              const fbRes = (await fallback.json().catch(() => ({}))) as { notice?: NoticeWire };
              surfaceSendNotice(fbRes.notice);
            }
          } catch (err) {
            console.error("widget action send failed (not_found fallback, network)", err);
            surfaceSendFailure("The server was unreachable while sending the widget action.");
          }
          replyDown({ content: [{ type: "text", text: "Sent to the agent." }], isError: false });
          return;
        }

        replyDown(appToolResponseToCallResult(resp));
      } catch (err) {
        console.error("app-tool-call send failed (network)", err);
        surfaceSendFailure("The server was unreachable while sending the widget action.");
        replyDown({ content: [{ type: "text", text: "The server was unreachable." }], isError: true });
      }
    },
    [ns, name, props.sandboxOrigin, surfaceSendFailure, surfaceSendNotice],
  );

  // Relay a widget's proposed action from its sandbox-origin /mcpui-host
  // frame up to the agent — the confused-deputy crossing this task exists
  // for. McpUiHost.tsx's logUIAction posts {ap:"mcpui", cmd:"action", result}
  // to window.parent using the SAME trusted origin this shell was served
  // from (widgets.go's mcpUiHostBootstrap.trustedOrigin), never "*". The
  // postMessage payload is UNTRUSTED (any script could forge one), so every
  // check below runs BEFORE d.result is ever touched, in the order the
  // confused-deputy threat model demands: (1) origin must equal THIS
  // session's sandbox origin; (2) source must be the contentWindow of one of
  // the CURRENTLY MOUNTED widget iframes — widgetFramesRef, not a single
  // ref, because this page can frame several widgets at once, unlike
  // ArtifactView.tsx's one artifact iframe; (3) the message must carry the
  // exact {ap:"mcpui", cmd:"action"} tag with a truthy result. Only once all
  // three hold is the action flattened (interactRequestFromUIAction) and
  // POSTed to /session/{ns}/{name}/interact — session-scoped, no artifactId
  // (mirrors onSend above) — where pkg/channels/interact's mcp_ui_action Kind is the
  // REAL backstop: it re-clamps every field and wraps it in an
  // untrusted-delimited envelope before it ever reaches the LLM. This
  // listener is defense-in-depth on top of that, not a replacement for it. A
  // non-2xx or network failure is surfaced via the same sendError banner
  // onSend uses — never swallowed, and never retried automatically.
  //
  // A `tool` UIAction takes a SEPARATE, CORRELATED branch (below) —
  // McpUiHost.tsx's routeUIAction sends {cmd:"appToolCall", corrId, result}
  // for those instead of the fire-and-forget {cmd:"action"} handled here,
  // because the widget is awaiting a live CallToolResult reply rather than
  // firing a turn and moving on. Both branches share the SAME origin +
  // mounted-widget-source + {ap:"mcpui"} tag validation below.
  useEffect(() => {
    const onMsg = (e: MessageEvent) => {
      if (!props.sandboxOrigin || e.origin !== props.sandboxOrigin) return;
      // Which mounted widget sent this, not merely whether one did. The map is
      // keyed by Line.id, so the matching entry names the calling widget's
      // artifact — the identity the app-tool-call POST must carry so the server
      // can pin the call to that widget's own MCP server.
      const sender = Array.from(widgetFramesRef.current.entries()).find(([, f]) => f.contentWindow === e.source);
      if (!sender) return;
      const d = e.data as { ap?: string; cmd?: string; result?: UIActionResult; corrId?: unknown };
      if (d?.ap !== "mcpui") return;

      if (d.cmd === "appToolCall" && typeof d.corrId === "string" && d.result?.type === "tool" && e.source) {
        void handleAppToolCall(
          d.result as UIActionResultToolCall,
          d.corrId,
          e.source as Window,
          artifactIdFromWidgetLineID(sender[0]),
        );
        return;
      }

      if (d.cmd !== "action" || !d.result) return;

      fetch(`/session/${ns}/${name}/interact`, {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(interactRequestFromUIAction(d.result)),
      })
        .then(async (r) => {
          if (!r.ok) {
            const body = (await r.json().catch(() => ({}))) as { error?: string };
            console.error("widget action send failed", r.status, body);
            surfaceSendFailure(body.error || `The widget action could not be sent (${r.status}).`);
            return;
          }
          const res = (await r.json().catch(() => ({}))) as { notice?: NoticeWire };
          surfaceSendNotice(res.notice);
        })
        .catch((err) => {
          console.error("widget action send failed (network)", err);
          surfaceSendFailure("The server was unreachable while sending the widget action.");
        });
    };
    window.addEventListener("message", onMsg);
    return () => window.removeEventListener("message", onMsg);
  }, [props.sandboxOrigin, ns, name, handleAppToolCall, surfaceSendFailure, surfaceSendNotice]);

  const endRef = useRef<HTMLDivElement>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 120;
    if (!nearBottom) return;
    endRef.current?.scrollIntoView?.({ block: "end" });
  }, [lines.length, working, notice]);

  return (
    <TooltipProvider delayDuration={200}>
      <div className="h-screen flex flex-col bg-background text-foreground">
        <div className="flex items-center gap-3 px-3 py-2 min-h-[52px] bg-card border-b border-border">
          <MessageCircle className="h-4 w-4 text-muted-foreground shrink-0" aria-hidden="true" />
          <span className="text-[13px] font-semibold truncate" title={`${ns}/${name}`}>
            {ns}/{name}
          </span>
          <div className="ml-auto shrink-0">
            <ConnectionIndicator state={connState} />
          </div>
        </div>

        <div ref={scrollRef} className="flex-1 min-h-0 overflow-y-auto px-4 py-4">
          <div className="flex flex-col gap-2.5 max-w-3xl mx-auto">
            {lines.length === 0 && (
              <div className="flex flex-1 flex-col items-center justify-center gap-2 py-16 text-muted-foreground">
                <MessageCircle className="h-8 w-8" aria-hidden="true" />
                <span className="text-sm">No messages yet.</span>
              </div>
            )}
            {lines.map((l) => (
              <div key={l.id} data-testid="session-view-line" data-role={l.role}>
                <TimelineLine line={l} ns={ns} name={name} live={working} registerWidgetFrame={registerWidgetFrame} />
              </div>
            ))}
            {working && !notice && <WorkingIndicator />}
            {working && notice && <CaptionLine text={notice} />}
            <div ref={endRef} />
          </div>
        </div>

        {sendError && (
          <div className="flex items-center gap-2 px-4 py-2 text-xs text-destructive border-t border-border bg-card">
            <AlertCircle className="h-3.5 w-3.5 shrink-0" aria-hidden="true" />
            <span className="break-words">{sendError}</span>
          </div>
        )}

        {sendNotice && <NoticeBanner notice={sendNotice} />}

        <Composer disabled={sending} placeholder="Message the agent…" onSend={onSend} />
      </div>
    </TooltipProvider>
  );
}
