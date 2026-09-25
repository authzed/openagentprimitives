import { createContext, useContext } from "react";
import type {
  ChatFrame,
  InteractionAppliedPayload,
  InteractionRequestInner,
  InteractionRequestPayload,
  NotificationPayload,
  OperationActivityInner,
  OperationActivityPayload,
  PlanUpdateInner,
  PlanUpdatePayload,
  SessionStartupPayload,
  StreamDeltaPayload,
  TurnActivityPayload,
  UserMessagePayload,
} from "./types";

// SessionSignals is the session socket folded down to the small set of facts
// the agent-defined view's FALLBACKS render when the agent hasn't declared a
// UI for something: `awaitingReply` (plus `lastAgentReply`) is what a
// fallback "ask the person" affordance keys on; `composingUpdate` is the
// "the agent is drafting a page update" line shown while an update_view tool
// call streams; `activity`/`plan` are the live default-region content (the
// running-operation line and the plan checklist) a view with no agent-declared
// region falls back to; `notice` is the latest mid-turn status caption; and
// `ended` marks the conversation over.
//
// This is derived PURELY from wire frames — turn_activity, stream_delta,
// operation_activity, plan_update, user_message/user_echo, notification,
// session_ended — with no agent cooperation: an agent that declares no view at
// all still gets a correct set of signals, because nothing here depends on
// anything the agent chose to render.
//
// The exact frame shapes folded here are pinned by a Go↔TS golden
// (pkg/web/webui/chat/frames_golden_internal_test.go writes
// ui/testdata/frames.golden.json from the REAL toFrame output;
// sessionSignals.test.ts folds those same bytes) — a rename of a wire field on
// either side breaks the golden comparison rather than silently drifting.
export interface SessionSignals {
  // turnActive is the last turn_activity frame's `active`: true while the
  // agent is actively turning.
  turnActive: boolean;
  // awaitingReply is true only when the last turn ended because the agent is
  // waiting on the person (cause "awaiting_reply") — as opposed to ending for
  // any other reason (approval, retry, completion, failure).
  awaitingReply: boolean;
  // pausedIdle is true only when the last turn ended on the runner's idle
  // exit (cause "idle" — channelevents.PauseCauseIdle): the agent finished
  // what it was asked, replied, and stopped WITHOUT asking anything. This is
  // not an ask — nobody is owed an answer, unlike awaitingReply — but it is
  // still a pause a person must be able to see and continue from: an agent
  // that stops mid-conversation with no visible cue looks like a dead page,
  // even though its reply landed in the transcript. Mirrors awaitingReply's
  // derivation but keys on "idle" instead of "awaiting_reply", and the two
  // are mutually exclusive (one turn_activity(active:false) carries exactly
  // one cause).
  pausedIdle: boolean;
  // lastAgentReply is the latest finalized agent reply (a user_message frame)
  // since the turn started, with a per-reply monotonic seq so a fallback can
  // tell a genuinely new reply from a re-render of the same one. The seq
  // comes from `replySeq` below — it is session-wide, not turn-local, so a
  // fallback's dismissedSeq comparison survives across turn boundaries (see
  // replySeq).
  lastAgentReply: { text: string; seq: number } | null;
  // replySeq is a count of user_message frames this session — NEVER reset
  // (in particular, not on turn start, which nulls lastAgentReply but leaves
  // this alone). A fallback "ask the person" modal compares
  // lastAgentReply.seq against its own last-dismissed seq to decide whether
  // to reopen; if seq restarted at 1 every turn, a second question in the
  // same session would carry the same seq as the first and the modal would
  // never reopen for it.
  replySeq: number;
  // composingUpdate is true while a stream_delta reports the agent mid-way
  // through an update_view tool call (tool_use_start with that tool name),
  // and false again at that call's tool_use_stop, at a stream "stop", or at
  // turn end — those three and nothing else. The painted view arrives on the
  // agent-UI plugin's own live channel, which this reducer never sees, so the
  // page landing cannot be what clears the cue.
  composingUpdate: boolean;
  // activity is the latest operation_activity snapshot, and null once a tick
  // reports `cleared` (no operation active) — the fallback's live
  // "running: <op>" line.
  activity: OperationActivityInner | null;
  // plan is the latest plan_update snapshot — the fallback's plan checklist.
  plan: PlanUpdateInner | null;
  // notice is the latest mid-turn notification text this turn; null once a
  // new turn starts.
  notice: string | null;
  // ended is true once a session_ended frame lands; terminal.
  ended: boolean;
  // startup is the health watcher's startup line while the session has not
  // started: what is in the way (or the generic lead), and whether the wait
  // has outlived the short startup grace. null once the session started, on
  // session_ended, and on reconnect (the watcher re-sends it on its next tick
  // if the session is still pre-start, so a missed `started` never leaves a
  // stale line).
  startup: { text: string; short: string | null; stillTrying: boolean } | null;
  // pendingInteractions is the live set of interaction_request prompts still
  // awaiting a decision, in arrival order, unique by requestRef — a second
  // request frame for the same requestRef replaces the existing entry rather
  // than adding a duplicate. It is what lets the agent-defined view surface a
  // pending decision the agent is blocked on (a "Needs your decision" region
  // above the declared page) with the SAME InteractionCard the chat renders
  // inline, for a view that has no timeline of its own to render one into.
  // An entry is removed by its matching interaction_applied, and the whole
  // list is cleared by session_ended and the reconnect reset — a resurfaced
  // prompt re-arrives as a fresh frame after reconnect, the same mechanism
  // the chat itself relies on.
  pendingInteractions: InteractionRequestInner[];
}

// INITIAL_SESSION_SIGNALS is what a session with no frames yet folded looks
// like: nothing active, nothing to show.
export const INITIAL_SESSION_SIGNALS: SessionSignals = {
  turnActive: false,
  awaitingReply: false,
  pausedIdle: false,
  lastAgentReply: null,
  replySeq: 0,
  composingUpdate: false,
  activity: null,
  plan: null,
  notice: null,
  ended: false,
  startup: null,
  pendingInteractions: [],
};

// repliesEqual compares lastAgentReply by value (text+seq), not identity, so a
// frame that would rebuild an identical reply doesn't force a new signals
// object.
function repliesEqual(a: SessionSignals["lastAgentReply"], b: SessionSignals["lastAgentReply"]): boolean {
  if (a === b) return true;
  if (a === null || b === null) return false;
  return a.text === b.text && a.seq === b.seq;
}

// snapshotEqual compares an activity/plan snapshot by VALUE
// (JSON.stringify), not reference: each is cheap (a handful of scalar fields,
// or a short items list) at the sizes a single tick ever carries, so the cost
// of stringifying beats forcing a new signals object — and a re-render — on
// two ticks that republish byte-identical content.
function snapshotEqual<T>(a: T, b: T): boolean {
  if (a === b) return true;
  if (a === null || b === null) return false;
  return JSON.stringify(a) === JSON.stringify(b);
}

// pendingInteractionsEqual compares two pendingInteractions lists slot by
// slot, by REFERENCE, not by deep value: computeNext never mutates an
// existing request object in place — a requestRef either passes through
// untouched (same reference) or is replaced wholesale by a freshly-parsed
// frame's payload (a new object, even when every field happens to match) —
// so per-slot identity is exactly "did this card's content change", without
// paying for a JSON.stringify on every fold the way snapshotEqual does for
// activity/plan.
function pendingInteractionsEqual(a: InteractionRequestInner[], b: InteractionRequestInner[]): boolean {
  if (a === b) return true;
  if (a.length !== b.length) return false;
  return a.every((r, i) => r === b[i]);
}

// signalsEqual is a full-field comparison used to decide whether a reduction
// actually changed anything.
function signalsEqual(a: SessionSignals, b: SessionSignals): boolean {
  return (
    a.turnActive === b.turnActive &&
    a.awaitingReply === b.awaitingReply &&
    a.pausedIdle === b.pausedIdle &&
    repliesEqual(a.lastAgentReply, b.lastAgentReply) &&
    a.replySeq === b.replySeq &&
    a.composingUpdate === b.composingUpdate &&
    snapshotEqual(a.activity, b.activity) &&
    snapshotEqual(a.plan, b.plan) &&
    a.notice === b.notice &&
    a.ended === b.ended &&
    snapshotEqual(a.startup, b.startup) &&
    pendingInteractionsEqual(a.pendingInteractions, b.pendingInteractions)
  );
}

// RECONNECT_RESET is NOT a wire type: nothing on the socket sends it and the
// server knows nothing about it. It is the one action this fold accepts from
// the outside, dispatched by SessionSocketProvider when a dropped connection
// comes back (see SessionSocket.tsx).
//
// It exists because the socket has no replay. A turn_activity(active:false)
// that landed while the connection was away is simply gone, and nothing else
// ever clears turnActive — the default progress region would spin "Working…"
// for the rest of the session. So a reconnect fails toward "not working": it
// clears only what a missed frame could have falsified, and leaves
// awaitingReply / lastAgentReply / plan alone, because a gap does not make
// those wrong — at worst stale, and an unanswered ask is still unanswered.
export const RECONNECT_RESET = { type: "__reconnect" } as unknown as ChatFrame;

// reduceSessionSignals folds one ChatFrame into the running signals. Pure:
// same inputs always produce an equal output, and it never throws — an
// unrecognized frame type, or one whose payload is missing an expected field,
// falls through to the unchanged input rather than a crash (every payload
// access below is optional-chained or defaulted, mirroring ChatView's
// handleFrame). It returns the SAME object (by reference) when nothing
// changed, so a subscriber built on this (useSessionSignals below) doesn't
// re-render on a frame that carries no news.
export function reduceSessionSignals(s: SessionSignals, f: ChatFrame): SessionSignals {
  const next = computeNext(s, f);
  return next === s || signalsEqual(s, next) ? s : next;
}

function computeNext(s: SessionSignals, f: ChatFrame): SessionSignals {
  switch (f.type) {
    case "turn_activity": {
      const p = f.payload as TurnActivityPayload;
      if (p?.active) {
        return { ...s, turnActive: true, awaitingReply: false, pausedIdle: false, composingUpdate: false, lastAgentReply: null, notice: null };
      }
      return { ...s, turnActive: false, awaitingReply: p?.cause === "awaiting_reply", pausedIdle: p?.cause === "idle", composingUpdate: false };
    }
    case "user_message": {
      const p = f.payload as UserMessagePayload;
      // replySeq is session-monotonic (never reset — see its doc comment),
      // unlike lastAgentReply itself, which turn_activity(active:true) nulls
      // every turn: deriving seq from replySeq rather than from the prior
      // lastAgentReply.seq is what keeps a fallback's dismissedSeq comparison
      // meaningful across a turn boundary.
      const replySeq = s.replySeq + 1;
      return { ...s, lastAgentReply: { text: p?.text ?? "", seq: replySeq }, replySeq };
    }
    case "user_echo": {
      // Someone answered on another surface (another tab, the artifact view):
      // this session is no longer waiting on THIS view's ask. The payload
      // carries who/where, which no signal here needs.
      return { ...s, lastAgentReply: null, awaitingReply: false };
    }
    case "stream_delta": {
      const inner = (f.payload as StreamDeltaPayload)?.payload;
      if (inner?.eventType === "tool_use_start" && inner.toolName === "update_view") {
        return { ...s, composingUpdate: true };
      }
      if (inner?.eventType === "tool_use_stop" || inner?.eventType === "stop") {
        return { ...s, composingUpdate: false };
      }
      return s;
    }
    case "operation_activity": {
      const inner = (f.payload as OperationActivityPayload)?.payload;
      return { ...s, activity: inner?.cleared ? null : (inner ?? null) };
    }
    case "plan_update": {
      const inner = (f.payload as PlanUpdatePayload)?.payload;
      return { ...s, plan: inner ?? null };
    }
    case "notification": {
      const p = f.payload as NotificationPayload;
      return { ...s, notice: p?.text ?? null };
    }
    case "interaction_request": {
      const inner = (f.payload as InteractionRequestPayload)?.payload;
      if (!inner?.requestRef) return s;
      const idx = s.pendingInteractions.findIndex((r) => r.requestRef === inner.requestRef);
      const pendingInteractions =
        idx >= 0
          ? s.pendingInteractions.map((r, i) => (i === idx ? inner : r))
          : [...s.pendingInteractions, inner];
      return { ...s, pendingInteractions };
    }
    case "interaction_applied": {
      const inner = (f.payload as InteractionAppliedPayload)?.payload;
      if (!inner?.requestRef) return s;
      return { ...s, pendingInteractions: s.pendingInteractions.filter((r) => r.requestRef !== inner.requestRef) };
    }
    case "__reconnect":
      // See RECONNECT_RESET: fail toward "not working", for exactly the signals
      // a frame missed during the outage could have left wrong. A prompt this
      // view never learned was resolved during the gap would otherwise sit as
      // a stale card forever; a still-open one re-arrives as a fresh
      // interaction_request once the connection is back (the same replay-free
      // contract every other signal here relies on).
      return { ...s, turnActive: false, pausedIdle: false, composingUpdate: false, activity: null, startup: null, pendingInteractions: [] };
    case "session_startup": {
      const p = f.payload as SessionStartupPayload;
      // An empty caption is "say nothing", never a wordless spinner: a missing
      // payload, the started frame, and a caption-less frame all clear
      // startup rather than leaving a blank line rendered.
      if (!p || p.started || !p.text) return { ...s, startup: null };
      return { ...s, startup: { text: p.text ?? "", short: p.short ?? null, stillTrying: Boolean(p.stillTrying) } };
    }
    case "session_ended":
      return { ...s, ended: true, turnActive: false, awaitingReply: false, pausedIdle: false, composingUpdate: false, startup: null, pendingInteractions: [] };
    default:
      // Outside this reducer's vocabulary (permission/tool-session/interaction
      // frames the fallback view has no signal for) — not an error, nothing
      // to fold.
      return s;
  }
}

// SessionSignalsContext carries ONE fold per session, run by
// SessionSocketProvider (SessionSocket.tsx) from the moment the socket opens.
//
// The fold belongs to the provider rather than to each consumer because the
// socket has no per-subscriber replay: every subscriber sees every frame from
// the moment it subscribes, and nothing re-sends what it missed. A consumer
// that folded for itself therefore started at INITIAL_SESSION_SIGNALS no
// matter how long the session had been running — so the agent-defined view,
// which the shell mounts lazily the first time the viewer opens that tab,
// showed no pending decision for a prompt that had arrived while they were
// reading the transcript, and had no way to learn of one short of a reconnect.
//
// The default is INITIAL_SESSION_SIGNALS: a render with no provider above it
// has no socket, and "nothing has happened" is the honest reading of no
// frames. Unlike SessionSocketContext's default this does not log — a
// subscriber outside a provider is the thing worth saying once, and
// useSessionFrames already says it.
export const SessionSignalsContext = createContext<SessionSignals>(INITIAL_SESSION_SIGNALS);

// useSessionSignals is the fallback view's one hook: the session's folded
// signals as the provider has them, current from socket open rather than from
// this component's own mount.
export function useSessionSignals(): SessionSignals {
  return useContext(SessionSignalsContext);
}
