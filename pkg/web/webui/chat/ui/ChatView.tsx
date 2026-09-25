import { useCallback, useEffect, useRef, useState } from "react";
import { Button, TooltipProvider, cn } from "@ap/design";
import type {
  ChatAttachment,
  ChatFrame,
  ChatLine,
  InteractionAppliedPayload,
  InteractionDecisionRejectedPayload,
  ToolSessionDeltaPayload,
  ToolSessionEventPayload,
  InteractionRequestPayload,
  InterruptAppliedPayload,
  LiveViewOfferPayload,
  MsgAttachment,
  SessionRef,
  MessageResponse,
  MessagesResponse,
  NoticeWire,
  NotificationPayload,
  OperationActivityNode,
  OperationActivityPayload,
  PlanUpdatePayload,
  SendErrorPayload,
  SessionDetail,
  SessionNotice,
  SessionEndedPayload,
  TimelineItem,
  StreamDeltaPayload,
  TurnActivityPayload,
  TurnProgressInner,
  TurnProgressPayload,
  UserMessagePayload,
  UserEchoPayload,
} from "./types";
import { progressCaption } from "./format";
import { reduceToolSessions, type ToolSessionMap } from "./toolSession";
import { useSessionConnState, useSessionFrames } from "./SessionSocket";
import { useSessionSignals } from "./sessionSignals";
import { ConnectionIndicator } from "./ConnectionIndicator";
import { MessageList } from "./MessageList";
import { ChatInput } from "./ChatInput";
import { failureText, sessionPath } from "./sessionMessage";
import { useInteractionDecision } from "./useInteractionDecision";
import { interactionRejectionText } from "./interactionText";

// endedText maps a session_ended frame's reason to the friendly system line
// shown in the timeline. Mirrors chat's terminalNotice reasons: "succeeded" |
// "failed" | "idle_timeout" | "server_shutdown" (pkg/web/webui/chat/registry.go).
export function endedText(p: SessionEndedPayload): string {
  switch (p.reason) {
    case "succeeded":
      return "Conversation ended.";
    case "failed":
      return `Conversation ended: ${p.failureMessage || p.failureReason || "the agent failed"}.`;
    case "idle_timeout":
      return "Conversation ended after being idle too long.";
    case "server_shutdown":
      return "Conversation ended: the server is restarting.";
    default:
      return "Conversation ended.";
  }
}

// timelineToLines maps a replayed transcript (GET .../messages) to the UI's
// ChatLine model — a plan snapshot keyed stably by plan name, everything else
// a role/text bubble. Shared by the initial load and the reconnect resync so
// both rebuild the timeline identically.
function timelineToLines(timeline: TimelineItem[], newId: () => string): ChatLine[] {
  return timeline.map((it) =>
    it.kind === "plan"
      ? { id: `plan:${it.plan.planName}`, role: "plan" as const, text: "", plan: it.plan }
      : { id: newId(), role: it.role, text: it.text },
  );
}

// HISTORY_UNAVAILABLE is what a transcript load that failed for any reason
// other than "this server cannot replay" says. It states the loss AND what
// still works, because the live socket attaches independently: a silent empty
// transcript would be indistinguishable from a conversation with no history.
const HISTORY_UNAVAILABLE = "Earlier messages could not be loaded. The live conversation still works.";

// STARTING_NOTICE is the placeholder caption shown the instant a send fires,
// before any real status arrives. Unlike a real update_status caption it is
// ephemeral — the first genuine activity (stream token or progress tick) decays
// it so it never lingers as a stale "Starting…" through a long turn.
const STARTING_NOTICE = "Starting…";

// WORKING_NOTICE is the neutral caption an ephemeral notice decays to on the
// first activity frame after it appears. A transient one-shot announcement (the
// runner's l.Notify — "Picking up N messages you sent.", "<agent> thinking…")
// shows briefly, then collapses to "Working…" rather than sticking as the
// caption for the rest of the turn. "Working…" itself holds only until the next
// real status update (a persistent update_status caption) supersedes it, or the
// turn ends. A persistent update_status caption never decays here.
const WORKING_NOTICE = "Working…";

// Notice is the live status caption plus whether it is ephemeral. An ephemeral
// notice (STARTING_NOTICE, WORKING_NOTICE, or any l.Notify announcement flagged
// NotificationPayload.ephemeral) decays to WORKING_NOTICE on the next activity
// frame; a persistent notice (an update_status caption) stays until superseded.
type Notice = { text: string; ephemeral: boolean };
const EMPTY_NOTICE: Notice = { text: "", ephemeral: false };

// decayEphemeral is the setNotice updater applied on the first activity frame
// (stream token / progress tick): an ephemeral notice collapses to the neutral
// WORKING_NOTICE, while a persistent update_status caption is left untouched.
// Idempotent — a notice already at WORKING_NOTICE returns the same reference so
// repeated frames don't churn state.
function decayEphemeral(cur: Notice): Notice {
  return cur.ephemeral && cur.text !== WORKING_NOTICE ? { text: WORKING_NOTICE, ephemeral: true } : cur;
}

// artifactDownloadURL builds the same-origin download link for an attached
// artifact. webd serves both this view and /artifact-download, so a relative URL
// works — gated by the logged-in session + the viewer's CheckView, no signed
// link needed. Content-Disposition on the endpoint forces the download.
function artifactDownloadURL(session: SessionRef, a: MsgAttachment): string {
  const params = new URLSearchParams({
    artifactId: a.artifactId,
    sessionRef: `${session.namespace}/${session.name}`,
    fn: a.filename || a.artifactId,
  });
  return `/artifact-download?${params.toString()}`;
}

// PRE_START_NOTICE_KINDS are the sessionnotice kinds that duplicate the
// shell's startup line: "startup" (StartupWait) and "scheduling"
// (SandboxScheduling=False). The shell's startup line owns the pre-start
// window — while it is showing (signals.startup is set) the same blocker
// must not appear a second time, in different words, as a card. The degraded
// kinds (awaiting_retry, restart_denied) are not startup captions and stay.
const PRE_START_NOTICE_KINDS = new Set(["startup", "scheduling"]);

// ChatView is the transcript: the conversation, the composer, and the live
// socket for one session. It is the DEFAULT content-region view — most agents
// declare no AgentUI — and it is a VIEW, not an app: it renders no identity,
// no session list and no chrome, because those are the shell's.
//
// ns/name come from the shell's server-resolved selection and are the only
// session this component ever addresses; there is no in-view session
// switching, so the URL and the socket cannot disagree about which session is
// open.
//
// readOnly is true for a session the ladder resolved as ended. The transcript
// still renders — Registry.authorizeRead admits a terminal session precisely
// so a finished conversation stays readable — and the composer is disabled.
// The start-a-replacement control is the SHELL's, beside this view.
//
// This view SUBSCRIBES to the session socket rather than opening one: the
// socket is the shell's (SessionSocket.tsx), held for as long as the session is
// selected, so what the live channel says does not depend on which view is on
// screen. Anything the shell itself must act on — the tab title, the away
// notification — it reads from that same socket, in its own subscriber.
//
// The two callbacks are what this view reports to whoever hosts it.
// onUserSend is the viewer typing into THIS composer — the one real user
// gesture the page has, and the only thing that can arm a browser's
// notification-permission request. onSessionEnded is this view's answer that
// the conversation is over, from a session_ended frame or a send the server
// refused as no longer routable; the host needs it because its own props are
// frozen at page load. Both are optional so the view stays renderable on its
// own, and it never depends on a host acting on them — `endedLive` below
// disables its own composer whether or not anyone is listening.
export interface ChatViewProps {
  ns: string;
  name: string;
  readOnly: boolean;
  onUserSend?: () => void;
  onSessionEnded?: () => void;
}

export function ChatView({ ns, name, readOnly, onUserSend, onSessionEnded }: ChatViewProps) {
  const [lines, setLines] = useState<ChatLine[]>([]);
  // endedLive is what the LIVE conversation observed: a session_ended frame,
  // or a send the server answered as no-longer-routable. `readOnly` is the
  // server's own answer at page-load. The two are ORed rather than mirrored
  // into one state, so neither can be silently overwritten by the other.
  const [endedLive, setEndedLive] = useState(false);
  // notices are the statements this session's status implies, kept so a session
  // that is parked, starting up or waiting on capacity can SAY so. channelsd
  // sessionWatcher skips every client-hosted session (clientHostedHere) and
  // `browser` is one — surfaced by webd, not channelsd — so that envelope is
  // envelopes are never sent here and this surface must speak for itself.
  // Without it the transcript simply stops and the person sits looking at it.
  const [notices, setNotices] = useState<SessionNotice[]>([]);
  // The shell's startup line (SessionShell, over the same session socket) says
  // the pre-start blocker in its own words while signals.startup is set; while
  // it is showing, the matching sessionnotice cards yield rather than repeat
  // it. A host that renders this view outside a SessionShell gets no signals
  // provider, so startup stays null and every notice shows, unchanged.
  const { startup } = useSessionSignals();
  const visibleNotices =
    startup === null ? notices : notices.filter((n) => !PRE_START_NOTICE_KINDS.has(n.kind));
  const ended = readOnly || endedLive;

  // markEnded is the ONE place the live channel's "this conversation is over"
  // is recorded, so the local composer lock and the host's report can never
  // disagree about having happened. The local state is kept even though the
  // host may re-derive `readOnly` from the same event: a host that wires no
  // callback must still get a disabled composer, and the local flag lands in
  // the same commit as the timeline line that announces the end, while the
  // host's answer is a render behind.
  const markEnded = useCallback(() => {
    setEndedLive(true);
    onSessionEnded?.();
  }, [onSessionEnded]);

  // openRef names the session this view is CURRENTLY addressing, and stillOpen
  // is what every async handler checks after its awaits before writing state.
  //
  // Not reachable today — the shell swaps sessions by navigating, so a new
  // session means a new mount — but every write below is a write into whatever
  // session is open at the moment it lands, not the one it was started for.
  // The first time selection becomes client-side, the symptom would be "I
  // opened session B and session A's transcript appeared", which is expensive
  // to diagnose and cheap to rule out here. The mount effect owns the value, so
  // it flips in the same commit that clears the previous session's timeline.
  const openRef = useRef(`${ns}/${name}`);
  const stillOpen = useCallback((key: string) => key === openRef.current, []);

  // Live turn state, driven by the streaming/progress/activity frames:
  //   working       — the agent is actively turning (throbber + status line).
  //   streamingText — the in-progress assistant reply, appended token-by-token
  //                   and finalized when the completed user_message lands.
  //   progress      — the latest turn_progress snapshot for the status line.
  const [working, setWorking] = useState(false);
  // notice is the latest mid-flight status caption (builtin.MsgNotification is a
  // mid-flight status, not a durable message). It shows — pulsing — on its own
  // CaptionLine while the turn is active, INDEPENDENT of the streaming bubble
  // (mirroring Slack's persistent setStatus), and is cleared the moment the turn
  // completes, so status chatter does NOT accumulate in the timeline. A
  // persistent update_status caption ("Reading files…") stays until superseded;
  // an ephemeral one-shot announcement ("Picking up N messages you sent.")
  // decays to WORKING_NOTICE on the next activity frame — see Notice. Plans
  // (role:"plan" lines) are the opposite: committed and never hidden.
  const [notice, setNotice] = useState<Notice>(EMPTY_NOTICE);
  const [streamingText, setStreamingText] = useState("");
  const [progress, setProgress] = useState<TurnProgressInner | null>(null);
  // opTree is the runner's live operation-activity tree (operation_activity
  // frames), rendered nested under the notice caption while the turn is
  // active. Cleared at every point `notice` is cleared (see EMPTY_NOTICE call
  // sites below) — it's exactly as transient as the mid-flight status.
  const [opTree, setOpTree] = useState<OperationActivityNode[]>([]);
  // toolSessions holds one live transcript per ToolCallRef (see toolSession.ts).
  // Kept OUT of `lines` because a tool session is updated continuously by many
  // frames while the timeline entry that positions it is written once — the
  // timeline holds only a `toolSessionRef` pointer, so a chunk arriving does not
  // rewrite the line array.
  const [toolSessions, setToolSessions] = useState<ToolSessionMap>({});
  // streamingRef mirrors streamingText so a flush-on-finalize can read the
  // current draft synchronously without nesting one setState inside another's
  // updater (which React discourages). setStreaming keeps the two in lockstep.
  const streamingRef = useRef("");
  const setStreaming = useCallback((updater: (cur: string) => string) => {
    const next = updater(streamingRef.current);
    streamingRef.current = next;
    setStreamingText(next);
  }, []);

  // pendingInterrupt is the requestID of an in-flight "Interrupt & Send Now"
  // POST, awaiting its interrupt_applied answer. Non-null both disables the
  // button (prevents a double-fire) and is the correlation key handleFrame
  // checks the incoming frame's requestID against. pendingInterruptRef
  // mirrors it for the same reason streamingRef mirrors streamingText: the
  // handleFrame closure (built once per render via useCallback deps) must
  // read the CURRENT value at frame-arrival time, not a stale one captured
  // when the callback was created.
  const [pendingInterrupt, setPendingInterruptState] = useState<string | null>(null);
  const pendingInterruptRef = useRef<string | null>(null);
  const setPendingInterrupt = useCallback((rid: string | null) => {
    pendingInterruptRef.current = rid;
    setPendingInterruptState(rid);
  }, []);

  const nextId = useRef(0);
  const newId = useCallback(() => `l${nextId.current++}`, []);
  const appendLine = useCallback(
    (role: ChatLine["role"], text: string, opts?: { queued?: boolean; attachments?: ChatAttachment[] }) => {
      setLines((prev) => [...prev, { id: newId(), role, text, queued: opts?.queued, attachments: opts?.attachments }]);
    },
    [newId],
  );

  // appendNotice commits a server notice (MessageResponse.notice) to the
  // timeline as a tone-styled card. A notice with an empty lead is nothing to
  // draw — NoticeWire.IsZero's exact rule — and is skipped rather than
  // rendering an empty card. Deliberate SILENCE never reaches a wire at all
  // (notice.Suppressed decides that server-side and logs why), so an absent
  // notice here is genuinely "no message", not a dropped one.
  const appendNotice = useCallback(
    (n: NoticeWire) => {
      if (!n.lead) return;
      setLines((prev) => [...prev, { id: newId(), role: "notice", text: "", notice: n }]);
    },
    [newId],
  );

  // droppedRef latches that the live socket dropped (reconnecting/offline) so
  // the return to "connected" is recognized as a RECONNECT and triggers a
  // transcript resync — never on the first connect.
  const droppedRef = useRef(false);

  // turnActiveRef tracks whether a turn is currently in flight. turn_progress /
  // operation_activity are render-only spinner ticks that must NOT drive the
  // "working" throbber on their own — a tick published just before the runner
  // yielded can arrive AFTER turn_activity(active:false) and would otherwise
  // re-arm the throbber on an idle session. Set on send and on
  // turn_activity(active:true); cleared on turn_activity(active:false).
  const turnActiveRef = useRef(false);

  // armLiveTurn and resetLiveTurn are the ONLY two transitions of the live-turn
  // state. turnActiveRef is the single source of truth for "a turn is in
  // flight"; `working` is driven in lockstep with it here so no terminal path
  // can clear one and leave the other (the bug where a late tick re-armed a
  // throbber the turn-end had cleared, or a mid-stream error left a blinking
  // cursor bubble). Arm on every send; reset at every turn-terminal point.
  const armLiveTurn = useCallback(() => {
    turnActiveRef.current = true;
    setWorking(true);
    setProgress(null);
    setNotice({ text: STARTING_NOTICE, ephemeral: true });
    setStreaming(() => "");
  }, [setStreaming]);
  const resetLiveTurn = useCallback(() => {
    turnActiveRef.current = false;
    setWorking(false);
    setProgress(null);
    setOpTree([]);
    setNotice(EMPTY_NOTICE);
    setStreaming(() => "");
  }, [setStreaming]);

  // flushStreaming commits any in-progress streamed draft as a finalized agent
  // line — used at a turn boundary that streamed tokens but never sent a
  // terminal user_message. It only COMMITS the draft; resetLiveTurn clears it,
  // so flushStreaming must run before resetLiveTurn.
  const flushStreaming = useCallback(() => {
    const leftover = streamingRef.current;
    if (leftover) appendLine("agent", leftover);
  }, [appendLine]);

  // ungrayQueued clears the `queued` flag on every held user line in place, so a
  // grayed "Queued" bubble un-grays to a normal bubble once the runner drains
  // the held queue (at turn end, or on an accepted interrupt) — one render, not
  // a disappear-and-reappear.
  const ungrayQueued = useCallback(() => {
    setLines((prev) => prev.map((l) => (l.queued ? { ...l, queued: false } : l)));
  }, []);

  // syncOnReconnect catches the timeline up after the live socket dropped and
  // came back. The sink is live-only (no replay), so any frame emitted while
  // the socket was down — the agent's reply, the turn-complete — was lost.
  // Refetch the transcript (authoritative, from operator memory) and reconcile
  // the live-turn state from the session's real phase: exactly what a manual
  // reload does, done automatically. Runs only on a genuine reconnect.
  const syncOnReconnect = useCallback(async () => {
    const base = sessionPath(ns, name);
    const key = `${ns}/${name}`;
    try {
      const [msgsResp, detailResp] = await Promise.all([fetch(`${base}/messages`), fetch(`${base}/detail`)]);
      // A failed half is logged rather than dropped, and does NOT abandon the
      // other half: recovering the timeline without the phase (or the reverse)
      // is strictly better than recovering neither.
      if (!msgsResp.ok) {
        console.error("chat: resync transcript failed", { ns, name, status: msgsResp.status });
      } else {
        const data = (await msgsResp.json()) as MessagesResponse;
        // Every await has resolved — apply the recovered state ONLY if this is
        // still the open conversation (see stillOpen).
        if (!stillOpen(key)) return;
        setLines(timelineToLines(data.timeline ?? [], newId));
      }
      if (!detailResp.ok) {
        console.error("chat: resync session detail failed", { ns, name, status: detailResp.status });
        return;
      }
      const detail = (await detailResp.json()) as SessionDetail;
      if (!stillOpen(key)) return;
      // A settled session (anything but actively Running) is not working:
      // reset the throbber + live-turn state that a missed turn_activity
      // would otherwise have cleared, so the UI doesn't hang on "working".
      if (detail.phase !== "Running") resetLiveTurn();
      setNotices(detail.notices ?? []);
    } catch (err) {
      // The live socket is still attached, so a later frame or a manual reload
      // will reconcile — but the attempt failing is never silent.
      console.error("chat: resync after reconnect errored", { ns, name, err });
    }
  }, [ns, name, newId, resetLiveTurn, stillOpen]);

  // The transcript load. Runs on mount and whenever the addressed session
  // changes, replaying this session's prior turns from GET .../messages.
  //
  // A 501 means this server cannot replay transcripts at all (no webd memory
  // token) — the conversation still attaches live, and the timeline says so
  // rather than showing an unexplained empty history. Every other failure is
  // reported the same way, because an empty transcript that is really a
  // FAILED transcript is indistinguishable from a conversation with no
  // history, which is the silent-failure shape this repo forbids.
  useEffect(() => {
    let cancelled = false;
    // The session this view is addressing changes HERE, in the same commit that
    // clears the previous one's timeline — so an async handler started for the
    // old session can tell (stillOpen) that its result is no longer wanted.
    openRef.current = `${ns}/${name}`;
    setLines([]);
    setEndedLive(false);
    resetLiveTurn();
    setPendingInterrupt(null);
    droppedRef.current = false;

    void (async () => {
      try {
        // Detail rides alongside the transcript so a session already parked when
        // the view opens says so immediately, not only after a reconnect.
        void (async () => {
          try {
            const d = await fetch(`${sessionPath(ns, name)}/detail`);
            if (cancelled || !d.ok) return;
            const detail = (await d.json()) as SessionDetail;
            if (!cancelled) setNotices(detail.notices ?? []);
          } catch (err) {
            console.error("chat: load session detail errored", { ns, name, err });
          }
        })();
        const resp = await fetch(`${sessionPath(ns, name)}/messages`);
        if (cancelled) return;
        if (resp.ok) {
          const data = (await resp.json()) as MessagesResponse;
          if (cancelled) return;
          setLines(timelineToLines(data.timeline ?? [], newId));
          return;
        }
        console.error("chat: load transcript failed", { ns, name, status: resp.status });
        const text =
          resp.status === 501
            ? "History replay is unavailable here; resuming live."
            : await failureText(resp, HISTORY_UNAVAILABLE);
        if (cancelled) return;
        setLines([{ id: newId(), role: resp.status === 501 ? "system" : "error", text }]);
      } catch (err) {
        console.error("chat: load transcript errored", { ns, name, err });
        if (cancelled) return;
        setLines([{ id: newId(), role: "error", text: HISTORY_UNAVAILABLE }]);
      }
    })();

    return () => {
      cancelled = true;
    };
  }, [ns, name, newId, resetLiveTurn, setPendingInterrupt]);

  // handleFrame renders each pushed ChatFrame by type. Push-only: the client
  // never sends over this socket (see pkg/web/webui/chat/handlers.go's wsHandler
  // doc comment) — sends always go through POST .../message. Missing/partial
  // payload fields are tolerated (optional-chained + defaulted) so a frame that
  // omits a field never throws.
  const handleFrame = useCallback(
    (f: ChatFrame) => {
      switch (f.type) {
        case "user_message": {
          // Despite the name this carries the AGENT's reply text (see
          // builtin.MsgUserMessage). It finalizes any in-progress streamed
          // draft.
          const p = f.payload as UserMessagePayload;
          const replyText = p.text;
          setStreaming(() => "");
          // Map any attached artifacts to download chips (same-origin links).
          const attachments = (p.attachments ?? [])
            .filter((a) => a.artifactId)
            .map((a) => ({ filename: a.filename || a.artifactId, mime: a.mime, url: artifactDownloadURL(p.session, a) }));
          appendLine("agent", replyText, attachments.length ? { attachments } : undefined);
          break;
        }
        case "user_echo": {
          // A user message another view of this session typed (the artifact
          // view, or another chat tab). Mirror it into this timeline so every
          // view sees the same conversation. Dedup by requestID: an echo whose
          // requestID matches a line THIS tab already added is the mirror of its
          // own optimistic send — skip it. Echoes from other surfaces carry a
          // requestID this tab never used (or none), so they render as a new
          // user line.
          const p = f.payload as UserEchoPayload;
          setLines((prev) => {
            if (p.requestID && prev.some((l) => l.requestId === p.requestID)) return prev;
            return [...prev, { id: newId(), role: "user", text: p.text }];
          });
          break;
        }
        case "notification": {
          // Mid-flight status caption: surface it in the pulsing working
          // indicator, NOT as a committed timeline line — it's cleared when the
          // turn ends (turn_activity active:false / session_ended / send_error),
          // so completed status is hidden. An ephemeral one-shot announcement
          // (l.Notify — "Picking up N messages you sent.") is marked so the next
          // activity frame decays it to WORKING_NOTICE; a persistent
          // update_status caption (ephemeral unset) stays until superseded.
          // Ignore a status caption that lands after the turn ended
          // (turnActive=false) so a late frame can't re-arm the throbber on an
          // idle session — same gate as turn_progress / operation_activity.
          if (!turnActiveRef.current) break;
          const np = f.payload as NotificationPayload;
          setWorking(true);
          setNotice({ text: np.text, ephemeral: np.ephemeral === true });
          break;
        }
        case "plan_update": {
          const inner = (f.payload as PlanUpdatePayload).payload;
          // A plan is a COMMITTED timeline line, so always render/update it —
          // but only re-arm the throbber while a turn is genuinely active, so a
          // plan frame landing after turn-end doesn't resurrect a stuck spinner.
          if (turnActiveRef.current) setWorking(true);
          // One card per plan NAME, positioned at first appearance and updated
          // in place — so a resumed conversation (which reconstructs cards the
          // same way) renders identically. Keyed by a stable id `plan:<name>`.
          const planId = `plan:${inner.planName}`;
          setLines((prev) => {
            const idx = prev.findIndex((l) => l.role === "plan" && l.id === planId);
            if (idx >= 0) {
              const copy = prev.slice();
              copy[idx] = { ...prev[idx], plan: inner };
              return copy;
            }
            return [...prev, { id: planId, role: "plan", text: "", plan: inner }];
          });
          break;
        }
        case "stream_delta": {
          // Ignore a delta that lands after the turn ended (turnActive=false):
          // appending it would both re-arm the throbber AND resurrect a
          // blinking-cursor streaming bubble on an idle session.
          if (!turnActiveRef.current) break;
          const inner = (f.payload as StreamDeltaPayload).payload;
          if (inner?.eventType === "text_delta" && inner.text) {
            setWorking(true);
            // Real output is flowing — decay an ephemeral placeholder/announcement
            // to the neutral "Working…", but never a persistent update_status
            // caption (which must stay visible while the agent streams, mirroring
            // Slack's persistent setStatus).
            setNotice(decayEphemeral);
            setStreaming((cur) => cur + inner.text);
          }
          break;
        }
        case "turn_progress":
          // Render-only spinner tick. Ignore one that lands after the turn
          // already ended (turnActive=false) so a late tick can't re-arm the
          // throbber on an idle session.
          if (!turnActiveRef.current) break;
          setWorking(true);
          setNotice(decayEphemeral);
          setProgress((f.payload as TurnProgressPayload).payload);
          break;
        case "operation_activity": {
          // A throttled snapshot of the runner's live operation subtree. Like
          // turn_progress it is render-only: a snapshot that lands after the turn
          // ended (turnActive=false) must not show stale op nodes — clear the
          // tree instead. A Cleared tick (or empty list) also tears it down.
          if (!turnActiveRef.current) {
            setOpTree([]);
            break;
          }
          const inner = (f.payload as OperationActivityPayload).payload;
          setOpTree(inner.cleared ? [] : (inner.operations ?? []));
          break;
        }
        case "turn_activity": {
          const active = (f.payload as TurnActivityPayload).active;
          if (active) {
            turnActiveRef.current = true;
            setWorking(true);
          } else {
            // Turn finished / awaiting the user. Flush any leftover streamed
            // text (a turn that streamed but sent no final user_message), then
            // reset the whole live-turn state, then un-gray the held queue (the
            // runner has drained / will-drain those messages by now).
            flushStreaming();
            resetLiveTurn();
            ungrayQueued();
          }
          break;
        }
        case "interrupt_applied": {
          // The runner's answer to a POST .../interrupt we issued. Ignore a
          // frame that doesn't match the request we're currently waiting on
          // (a stray/late answer to an already-resolved or superseded
          // interrupt) — only react to, and clear, OUR pending one.
          const p = f.payload as InterruptAppliedPayload;
          if (p.requestID !== pendingInterruptRef.current) break;
          setPendingInterrupt(null);
          if (p.outcome === "interrupted") {
            appendLine("system", "Interrupting — sending your message(s) now.");
            // Same un-gray as a normal turn-end: the runner is about to drain
            // the held queue immediately rather than at the next turn_activity
            // boundary, so don't leave the queued lines grayed until then.
            ungrayQueued();
          } else {
            appendLine("system", "Couldn't interrupt — " + (p.reason || "the runner declined."));
          }
          break;
        }
        case "send_error":
          // A failure the backend surfaced mid-turn. Show it AND fully reset the
          // live-turn state — otherwise the user stares at a spinner that will
          // never resolve (the no-silent-hang fix). resetLiveTurn also drops
          // turnActive, so a later tick can't re-arm the throbber, and clears
          // the stream draft so a mid-stream error leaves no orphaned
          // blinking-cursor bubble.
          resetLiveTurn();
          appendLine("error", (f.payload as SendErrorPayload).err || "The agent failed to process a message.");
          break;
        case "session_ended": {
          markEnded();
          // Commit any leftover streamed draft first, then reset the live-turn
          // state, then post the friendly end-of-conversation line.
          flushStreaming();
          resetLiveTurn();
          appendLine("system", endedText(f.payload as SessionEndedPayload));
          break;
        }
        case "live_view_offer": {
          // The agent offered a browser live view of an artifact
          // (artifact_offer_view). Render it as a "View live" link. An empty
          // url means the viewer origin isn't configured — a clean skip.
          const url = (f.payload as LiveViewOfferPayload).url;
          if (url) setLines((prev) => [...prev, { id: newId(), role: "system", text: "", liveViewUrl: url }]);
          break;
        }
        case "tool_session_event":
        case "tool_session_delta": {
          // A dispatched tool streaming its output (claude and friends). Both
          // frame kinds fold into the same per-ToolCallRef transcript — the
          // parsed events and the raw stdout/stderr are two views of one run,
          // exactly as Slack's per-ToolCallRef message merges them.
          setToolSessions((prev) => reduceToolSessions(prev, f));

          // Position the block at the ref's FIRST appearance so it sits in
          // chronological order among the messages, then leave the line alone —
          // every subsequent chunk updates `toolSessions`, not the timeline.
          const ref = (f.payload as ToolSessionDeltaPayload | ToolSessionEventPayload | undefined)?.payload?.toolCallRef;
          if (!ref) break;
          const blockId = `toolsession:${ref}`;
          setLines((prev) =>
            prev.some((l) => l.id === blockId)
              ? prev
              : [...prev, { id: blockId, role: "toolSession", text: "", toolSessionRef: ref }],
          );
          break;
        }
        case "interaction_request": {
          // The generic interaction prompt (credential_link today,
          // identity_choice in the next slice) — the fix for the "agent
          // requests credentials, webchat hangs forever" bug: previously
          // nothing rendered this at all. One card per requestRef, positioned
          // at first appearance and updated in place — same pattern as
          // plan_update above — so a re-published (regenerated/resurfaced)
          // request for the same requestRef refreshes the existing card
          // rather than duplicating it.
          const inner = (f.payload as InteractionRequestPayload).payload;
          const cardId = `interaction:${inner.requestRef}`;
          setLines((prev) => {
            const idx = prev.findIndex((l) => l.role === "interaction" && l.id === cardId);
            if (idx >= 0) {
              const copy = prev.slice();
              copy[idx] = { ...prev[idx], interactionRequest: inner };
              return copy;
            }
            return [...prev, { id: cardId, role: "interaction", text: "", interactionRequest: inner }];
          });
          break;
        }
        case "interaction_applied": {
          // Resolves the matching card (by requestRef) in place: the card
          // stays in the timeline showing the outcome instead of its actions.
          // A frame for a requestRef with no matching card (e.g. one that
          // arrived before this tab attached) is a no-op — nothing to resolve.
          //
          // KNOWN SLICE-1 GAP: credential_link's out-of-band "linked"
          // confirmation (CredentialLinkedWatcher.emit, in
          // pkg/channels/channelsd/pipeline/credential_linked.go) sets RequestRef to
          // the credentialName, not the mintRequestID() this card was keyed
          // by (see the "interaction_request" case above). So that frame's
          // cardId never matches an existing "Connect your accounts" card
          // and always hits this no-op path — the card simply never
          // auto-resolves here. Not a hang: the session still resumes
          // once the credential is linked, and Slack's independent ephemeral
          // still confirms it there. Correctly fixing this needs
          // multi-credential correlation (mapping credentialName back to the
          // live requestRef) — out of scope for Slice 1, tracked for the
          // credential-polish follow-up.
          const inner = (f.payload as InteractionAppliedPayload).payload;
          const cardId = `interaction:${inner.requestRef}`;
          setLines((prev) => prev.map((l) => (l.id === cardId ? { ...l, interactionApplied: inner } : l)));
          break;
        }
        case "interaction_decision_rejected": {
          // A per-clicker decision rejection: THIS tab's click was refused
          // (lacked standing, the prompt was already resolved by someone
          // else, a category mismatch, or the bound handler errored — e.g. a
          // tool_approval grant-write failure). The shared interaction card
          // is left intact (a rejected click never resolves it) — render a
          // visible timeline note instead, so a real failure (handler_error)
          // is never silently dropped. already_resolved is informational
          // ("system"); every other class is a genuine failure ("error").
          const inner = (f.payload as InteractionDecisionRejectedPayload).payload;
          appendLine(inner.class === "already_resolved" ? "system" : "error", interactionRejectionText(inner));
          break;
        }
        default:
          // Outside v1 chat's rendered vocabulary (tool/approval/permission
          // frames from the interactive-tool + multiplayer surfaces) — not an
          // error, just nothing to render here yet.
          break;
      }
    },
    [appendLine, newId, setStreaming, setPendingInterrupt, markEnded, resetLiveTurn, flushStreaming, ungrayQueued],
  );

  useSessionFrames(handleFrame);
  const connState = useSessionConnState();

  // Stop the throbber and say so when the live socket drops for good: a
  // sustained outage ("offline") means no further frames will arrive, so a
  // still-spinning throbber would hang forever with no explanation. Gated on
  // !ended so a normal end-of-conversation socket close (which also trips
  // "offline" a few seconds later) doesn't tack on a spurious "connection
  // lost". lostShownRef de-dupes across re-renders and re-arms on reconnect.
  const lostShownRef = useRef(false);
  useEffect(() => {
    if (connState === "connected") {
      lostShownRef.current = false;
      // A return to "connected" after a drop is a RECONNECT: the sink is
      // live-only, so resync the transcript + phase to recover any frames the
      // socket missed while down (the "stuck starting / stuck working" fix).
      if (droppedRef.current) {
        droppedRef.current = false;
        void syncOnReconnect();
      }
      return;
    }
    if (connState === "connecting") {
      lostShownRef.current = false;
      return;
    }
    // reconnecting | offline: the socket is down — latch it so the next
    // "connected" resyncs.
    droppedRef.current = true;
    if (connState === "offline" && !ended && !lostShownRef.current) {
      lostShownRef.current = true;
      resetLiveTurn();
      appendLine("error", "Connection lost — reload the page to reconnect.");
    }
  }, [connState, ended, appendLine, syncOnReconnect, resetLiveTurn]);

  // markSend transitions one user line's delivery status in place (by id), so
  // an optimistic bubble can resolve pending → sent / failed without disturbing
  // the rest of the timeline.
  const markSend = useCallback(
    (id: string, sendStatus: NonNullable<ChatLine["sendStatus"]>, sendError?: string) => {
      setLines((prev) => prev.map((l) => (l.id === id ? { ...l, sendStatus, sendError } : l)));
    },
    [],
  );

  // postMessage POSTs one already-rendered user line and resolves its bubble to
  // sent or failed. The composer is never blocked on it: the input is ready
  // again in ~one frame and the pending bubble carries the in-flight state
  // itself, so a slow accept (a cold cluster's multi-second inbound path,
  // bounded by a 30s view_message timeout) never freezes typing or reads as a
  // failure until it truly fails. Shared by handleSend and Retry.
  //
  // The body carries text + the idempotency key and NOTHING that names a
  // session: the URL path does that, and it is the same path the route's
  // Authorize gate checked, so a body naming a different session than the gate
  // saw is unrepresentable rather than merely ignored.
  const postMessage = useCallback(
    async (id: string, text: string, requestId: string) => {
      const key = `${ns}/${name}`;
      markSend(id, "pending");
      try {
        // requestId is the idempotency key: minted once per user line and reused
        // by Retry, so channelsd collapses a re-send of an already-delivered
        // message into a no-op returning the prior decision.
        const resp = await fetch(`${sessionPath(ns, name)}/message`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ text, requestId }),
        });
        // The bubble this resolves belongs to the session it was sent from, and
        // so does resetLiveTurn's throbber: neither may land on a newer one.
        if (!stillOpen(key)) return;
        if (!resp.ok) {
          console.error("chat: send failed", { ns, name, status: resp.status });
          const reason = await failureText(resp, `Failed to send (${resp.status}).`);
          if (!stillOpen(key)) return;
          // The failure rides ON the message bubble (red + Retry), not as a
          // separate bottom line divorced from what failed.
          markSend(id, "failed", reason);
          resetLiveTurn();
          return;
        }
        const res = (await resp.json()) as MessageResponse;
        if (!stillOpen(key)) return;
        markSend(id, "sent");
        // The notice is the ONLY copy of this message — nothing is published
        // over the websocket for the non-routed outcomes (refused, denied by
        // permission, fork pending) or for a takeover/inherit continuation, so
        // not reading it here IS the silence.
        if (res.notice) appendNotice(res.notice);
        if (res.ended) {
          markEnded();
          resetLiveTurn();
        }
      } catch (err) {
        console.error("chat: send errored", { ns, name, err });
        if (!stillOpen(key)) return;
        markSend(id, "failed", "Network error while sending your message.");
        resetLiveTurn();
      }
    },
    [ns, name, markSend, appendNotice, markEnded, resetLiveTurn, stillOpen],
  );

  const handleSend = useCallback(
    (text: string) => {
      // Reported up before anything else: the shell arms its away-notification
      // permission prompt from this, and browsers only honor that request from
      // inside a real user gesture.
      onUserSend?.();
      // Optimistic echo: render the user's own bubble immediately in the
      // "pending" state — the websocket only ever pushes the agent's replies
      // (MsgUserMessage) and notices, never an echo of what the user just sent
      // (see pkg/web/webui/chat/sink.go's toFrame). Mark it `queued` when a turn is
      // already active so, once accepted, it renders grayed-and-held rather
      // than as a duplicate.
      const id = newId();
      // Mint the idempotency key now and stash it ON the line so Retry re-sends
      // the SAME key — the dedup only works if the retry key matches the first.
      const requestId = crypto.randomUUID();
      setLines((prev) => [...prev, { id, role: "user", text, queued: working, sendStatus: "pending", requestId }]);
      // Arm the live turn immediately with a "Starting…" label — the throbber
      // shouldn't wait for the first turn_activity/turn_progress frame. The
      // first notification/status/stream frame overwrites this label; a
      // send_error/session_ended frame resets it with the real error.
      armLiveTurn();
      void postMessage(id, text, requestId);
    },
    [working, newId, armLiveTurn, onUserSend, postMessage],
  );

  // handleRetry re-sends a failed bubble in place: the same POST, flipping the
  // bubble back to pending, with the same working-affordance reset as a fresh
  // send. Text is carried from the failed line so a retry needs no re-typing.
  const handleRetry = useCallback(
    (id: string, text: string, requestId: string) => {
      armLiveTurn();
      // Reuse the failed line's original idempotency key: if the first send in
      // fact landed and only its reply timed out, channelsd returns the cached
      // decision rather than re-delivering the message a second time.
      void postMessage(id, text, requestId);
    },
    [armLiveTurn, postMessage],
  );

  // handleInterrupt is the "Interrupt & Send Now" click handler: mints a
  // requestID, records it as the pending interrupt (disabling the button
  // until the runner answers via an interrupt_applied frame — see
  // handleFrame), and POSTs it. Mirrors handleSend's fetch/error pattern: a
  // failed POST is surfaced as an error line rather than left silent, and
  // clears the pending state so the button doesn't stay disabled forever on
  // a transport failure the runner never even saw.
  const handleInterrupt = useCallback(async () => {
    if (pendingInterruptRef.current) return;
    const key = `${ns}/${name}`;
    const rid = crypto.randomUUID();
    setPendingInterrupt(rid);
    try {
      const resp = await fetch(`${sessionPath(ns, name)}/interrupt`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ requestId: rid }),
      });
      if (!resp.ok) {
        console.error("chat: interrupt failed", { ns, name, status: resp.status });
        const reason = await failureText(resp, `Failed to interrupt (${resp.status}).`);
        // A failure belongs in the timeline it was raised from — see stillOpen.
        if (!stillOpen(key)) return;
        appendLine("error", reason);
        setPendingInterrupt(null);
      }
      // On success, leave pendingInterrupt set — handleFrame clears it (and
      // reports the outcome) when the matching interrupt_applied frame
      // arrives.
    } catch (err) {
      console.error("chat: interrupt errored", { ns, name, err });
      if (!stillOpen(key)) return;
      appendLine("error", "Network error while trying to interrupt.");
      setPendingInterrupt(null);
    }
  }, [ns, name, appendLine, setPendingInterrupt, stillOpen]);

  // onDecisionError commits a failed/errored decision to the timeline it was
  // raised from — see stillOpen — mirroring every other fetch handler here.
  const onDecisionError = useCallback(
    (text: string) => {
      if (!stillOpen(`${ns}/${name}`)) return;
      appendLine("error", text);
    },
    [ns, name, stillOpen, appendLine],
  );

  // handleDecision is the onDecision callback wired to every InteractionCard,
  // posting through the ONE decision route (POST .../decision) the shared
  // useInteractionDecision hook owns — see its own doc comment for the
  // fetch/error contract.
  const handleDecision = useInteractionDecision(ns, name, onDecisionError);

  const statusText = working ? progressCaption(progress) : null;
  const placeholder = ended ? "This conversation has ended." : "Message the agent…";

  return (
    <TooltipProvider delayDuration={200}>
      <div className="flex min-h-0 flex-1 flex-col bg-background text-foreground" data-testid="chat-view">
        {/* The socket's own health belongs to the transcript, not to chrome:
            it reports whether THIS view's live channel is delivering, which
            is a different fact from anything the shell knows about the
            session. */}
        <div className="flex items-center justify-end px-3 py-1 border-b border-border bg-card/40">
          <ConnectionIndicator state={connState} />
        </div>

        <MessageList
          lines={lines}
          streamingText={streamingText}
          working={working}
          statusText={statusText}
          notice={notice.text}
          opTree={opTree}
          toolSessions={toolSessions}
          onRetry={handleRetry}
          onDecision={handleDecision}
        />

        {working && lines.some((l) => l.queued) && (
          <div className="flex justify-center px-4 pt-2 bg-card">
            <Button variant="secondary" size="sm" disabled={pendingInterrupt !== null} onClick={handleInterrupt}>
              Interrupt & Send Now
            </Button>
          </div>
        )}
        {visibleNotices.length > 0 && (
          // One row per statement the session's status implies. Rendered here,
          // just above the composer, because that is where a person looks when
          // the transcript has stopped and they are deciding what to do.
          //
          // `tone` is emphasis only — a notice whose colour is dropped still
          // reads correctly, because `lead` says what it is and `nextStep` says
          // what to do about it.
          <div className="mx-4 mb-2 flex flex-col gap-1.5">
            {visibleNotices.map((n, i) => (
              <div
                key={`${n.kind}:${i}`}
                data-testid={`session-notice-${n.kind}`}
                className={cn(
                  "rounded-md border px-3 py-2 text-xs leading-snug",
                  n.tone === "degraded"
                    ? "border-warning/30 bg-warning/10 text-warning"
                    : "border-border/60 bg-muted/40 text-muted-foreground",
                )}
              >
                <div>{n.lead}</div>
                {n.nextStep && <div className="mt-0.5 font-medium">{n.nextStep}</div>}
                {n.body && (
                  // The machine-generated cause, for the curious. Mono and
                  // never truncated: it is the string someone greps for.
                  <div className="mt-1 break-all font-mono text-[10px] opacity-70">{n.body}</div>
                )}
              </div>
            ))}
          </div>
        )}
        <ChatInput disabled={ended} placeholder={placeholder} onSend={handleSend} />
      </div>
    </TooltipProvider>
  );
}
