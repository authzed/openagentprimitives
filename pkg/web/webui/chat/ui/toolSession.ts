// Live interactive-tool ("tool session") transcript state.
//
// The runner streams a dispatched tool's output two ways, both keyed by
// ToolCallRef: KindToolSessionEvent (parsed — the claude-stream-json path) and
// KindToolSessionDelta (raw stdout/stderr bytes). channelsd's builtin kind
// forwards both to the browser as `tool_session_event` / `tool_session_delta`
// frames. This module folds those frames into one transcript per ToolCallRef.
//
// It is deliberately pure and React-free: the hard parts here are byte-level
// (chunk boundaries, split escape sequences, split multi-byte characters) and
// are far easier to get right — and to test — without a DOM in the way.
// ToolSessionBlock.tsx renders whatever this produces; ChatView owns the store.

import Anser from "anser";
import type { ChatFrame } from "./types";

// MAX_TRANSCRIPT_LINES bounds what the browser holds per tool call. Slack is
// forced to truncate by message-size limits; the browser has no such forcing
// function, so without a cap a long `claude` run grows unbounded and the
// re-render cost grows with it. The full transcript stays durable in operator
// memory — this bounds only the live view.
export const MAX_TRANSCRIPT_LINES = 500;

// AnsiSpan is one run of text sharing a colour/decoration, parsed from ANSI.
// Structured spans (not an HTML string) are the point: this is UNTRUSTED tool
// output, so it must never reach dangerouslySetInnerHTML.
export interface AnsiSpan {
  text: string;
  fg?: string;
  bg?: string;
  bold?: boolean;
  italic?: boolean;
  underline?: boolean;
  dim?: boolean;
}

export type ToolSessionLineKind = "event" | "stdout" | "stderr";

export interface ToolSessionLine {
  kind: ToolSessionLineKind;
  spans: AnsiSpan[];
  // toolName is the streaming agent's own internal tool (Write, Read, …) on
  // tool_use lines — NOT the dispatched outer tool. Absent on raw output.
  toolName?: string;
}

export interface ToolSessionState {
  ref: string;
  outerTool: string;
  reason: string;
  lines: ToolSessionLine[];
  // elided counts lines dropped off the head by the cap, so the UI can say so
  // rather than silently presenting a truncated transcript as complete.
  elided: number;
  // pending holds each stream's not-yet-terminated trailing BYTES. Bytes, not
  // text: a chunk can split a multi-byte UTF-8 character, and decoding each
  // chunk independently would turn it into replacement characters. Buffering
  // undecoded bytes and decoding only completed lines fixes that and the
  // split-escape-sequence problem in one move.
  pending: { stdout: Uint8Array; stderr: Uint8Array };
  done: boolean;
  ok: boolean;
  exitCode?: number;
  exitReason?: string;
  durationMs?: number;
  costUsd?: number;
}

export type ToolSessionMap = Record<string, ToolSessionState>;

const EMPTY_BYTES = new Uint8Array(0);

export function emptyToolSession(ref: string): ToolSessionState {
  return {
    ref,
    outerTool: "",
    reason: "",
    lines: [],
    elided: 0,
    pending: { stdout: EMPTY_BYTES, stderr: EMPTY_BYTES },
    done: false,
    ok: false,
  };
}

// decodeBase64 turns the wire's base64 back into bytes. Go marshals []byte as
// base64, so `data` arrives encoded — Slack's renderer never meets this because
// its sender decodes server-side. Returns empty on malformed input rather than
// throwing: one bad chunk must not take down the whole chat socket handler.
function decodeBase64(data: string): Uint8Array {
  try {
    const bin = atob(data);
    const out = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  } catch {
    return EMPTY_BYTES;
  }
}

function concatBytes(a: Uint8Array, b: Uint8Array): Uint8Array {
  if (a.length === 0) return b;
  if (b.length === 0) return a;
  const out = new Uint8Array(a.length + b.length);
  out.set(a, 0);
  out.set(b, a.length);
  return out;
}

const decoder = new TextDecoder();

// parseAnsi splits one COMPLETE line into styled spans. Parsing whole lines
// (never raw chunks) is what makes a split escape sequence a non-issue: by the
// time this runs, the sequence has been reassembled.
export function parseAnsi(text: string): AnsiSpan[] {
  if (text === "") return [{ text: "" }];
  return Anser.ansiToJson(text, { use_classes: false, json: true, remove_empty: true }).map((chunk) => {
    const span: AnsiSpan = { text: chunk.content };
    if (chunk.fg) span.fg = `rgb(${chunk.fg})`;
    if (chunk.bg) span.bg = `rgb(${chunk.bg})`;
    const deco = chunk.decoration ?? undefined;
    const decos: string[] = Array.isArray((chunk as { decorations?: string[] }).decorations)
      ? ((chunk as { decorations?: string[] }).decorations as string[])
      : deco
        ? [deco]
        : [];
    if (decos.includes("bold")) span.bold = true;
    if (decos.includes("italic")) span.italic = true;
    if (decos.includes("underline")) span.underline = true;
    if (decos.includes("dim")) span.dim = true;
    return span;
  });
}

// pushLine appends a line, enforcing the cap by dropping from the head and
// counting the loss.
function pushLine(s: ToolSessionState, line: ToolSessionLine): void {
  s.lines.push(line);
  if (s.lines.length > MAX_TRANSCRIPT_LINES) {
    const over = s.lines.length - MAX_TRANSCRIPT_LINES;
    s.lines.splice(0, over);
    s.elided += over;
  }
}

// commitCompleteLines moves every newline-terminated line out of a stream's
// pending bytes and into the transcript, leaving the unterminated remainder
// buffered for the next chunk.
function commitCompleteLines(s: ToolSessionState, stream: "stdout" | "stderr", incoming: Uint8Array): void {
  let buf = concatBytes(s.pending[stream], incoming);

  let start = 0;
  for (let i = 0; i < buf.length; i++) {
    if (buf[i] !== 0x0a) continue; // '\n'
    let end = i;
    if (end > start && buf[end - 1] === 0x0d) end--; // strip CR of a CRLF
    pushLine(s, { kind: stream, spans: parseAnsi(decoder.decode(buf.subarray(start, end))) });
    start = i + 1;
  }
  s.pending[stream] = start === 0 ? buf : buf.subarray(start);
}

// flushPending commits any unterminated trailing bytes. Called when the stream
// terminates: a tool whose last line has no newline must still show that line,
// not silently drop it.
function flushPending(s: ToolSessionState): void {
  for (const stream of ["stdout", "stderr"] as const) {
    const rest = s.pending[stream];
    if (rest.length === 0) continue;
    pushLine(s, { kind: stream, spans: parseAnsi(decoder.decode(rest)) });
    s.pending[stream] = EMPTY_BYTES;
  }
}

// cloneForUpdate produces the mutable working copy. The reducer returns a new
// object for every frame it owns so React re-renders, but mutates that copy
// internally — the alternative (spreading on every line) is O(n) per chunk.
function cloneForUpdate(prev: ToolSessionState | undefined, ref: string): ToolSessionState {
  const base = prev ?? emptyToolSession(ref);
  return { ...base, lines: base.lines.slice(), pending: { ...base.pending } };
}

interface EventInner {
  toolCallRef?: string;
  eventType?: string;
  text?: string;
  toolName?: string;
  outerTool?: string;
  reason?: string;
  summary?: string;
  ok?: boolean;
  durationMs?: number;
  costUsd?: number;
}

interface DeltaInner {
  toolCallRef?: string;
  stream?: string;
  data?: string;
  terminal?: boolean;
  exitReason?: string;
  exitCode?: number;
}

// innerOf unwraps the wsFrame's double nesting: sink.go's toFrame sets
// `Payload: m` where m is the builtin.Msg* struct, which itself carries
// {session, payload}. So the real payload is frame.payload.payload.
function innerOf<T>(frame: ChatFrame): T | undefined {
  const outer = frame.payload as { payload?: T } | undefined;
  return outer?.payload;
}

function applyEvent(s: ToolSessionState, p: EventInner): void {
  // Reason and OuterTool ride on EVERY event precisely so the header needs no
  // accumulated state — take them whenever present.
  if (p.outerTool) s.outerTool = p.outerTool;
  if (p.reason) s.reason = p.reason;

  switch (p.eventType) {
    case "result":
      flushPending(s);
      s.done = true;
      s.ok = p.ok ?? false;
      if (p.durationMs !== undefined) s.durationMs = p.durationMs;
      if (p.costUsd !== undefined) s.costUsd = p.costUsd;
      if (p.summary) pushLine(s, { kind: "event", spans: parseAnsi(p.summary) });
      return;
    // Both tool_use events carry their gloss on `summary`; `text` is only ever
    // set on text_delta (see toolkitstream.Event). Reading `text` here left
    // every line blank, so a start rendered as a bare "Bash" tag and a stop as
    // an empty row. Content matches the Slack and TUI renderers
    // (slack/sender_tool_session.go, cmd/ap/chat_tui.go); the inner tool's name
    // is the line's toolName tag rather than an inline "→ " prefix, which is
    // the one thing this renderer styles structurally instead of in the text.
    case "tool_use_start":
      pushLine(s, { kind: "event", spans: parseAnsi(p.summary ?? ""), toolName: p.toolName });
      return;
    // A stop carries no toolName — it correlates by ToolID, which the transcript
    // renders positionally. Indented under its start, and always marked: a stop
    // with no gloss must still show its outcome rather than a blank row.
    case "tool_use_stop":
      pushLine(s, {
        kind: "event",
        spans: parseAnsi("  " + [p.ok ? "✓" : "✗", p.summary].filter(Boolean).join(" ")),
      });
      return;
    default:
      // text_delta and anything newer: render the text when there is any, and
      // ignore silently when there is not, so an unknown future event type is
      // never a crash.
      if (p.text) pushLine(s, { kind: "event", spans: parseAnsi(p.text) });
  }
}

function applyDelta(s: ToolSessionState, p: DeltaInner): void {
  const stream: "stdout" | "stderr" = p.stream === "stderr" ? "stderr" : "stdout";
  if (p.data) commitCompleteLines(s, stream, decodeBase64(p.data));
  if (!p.terminal) return;

  flushPending(s);
  s.done = true;
  s.ok = (p.exitCode ?? 0) === 0 && p.exitReason !== "failed";
  if (p.exitCode !== undefined) s.exitCode = p.exitCode;
  if (p.exitReason) s.exitReason = p.exitReason;
}

// reduceToolSessions folds one chat frame into the per-ToolCallRef transcript
// map. Frames it does not own return the SAME map reference, so ChatView's state
// setter can bail out instead of re-rendering the timeline on every unrelated
// frame.
export function reduceToolSessions(prev: ToolSessionMap, frame: ChatFrame): ToolSessionMap {
  if (frame.type === "tool_session_event") {
    const p = innerOf<EventInner>(frame);
    if (!p?.toolCallRef) return prev;
    const s = cloneForUpdate(prev[p.toolCallRef], p.toolCallRef);
    applyEvent(s, p);
    return { ...prev, [p.toolCallRef]: s };
  }

  if (frame.type === "tool_session_delta") {
    const p = innerOf<DeltaInner>(frame);
    if (!p?.toolCallRef) return prev;
    const s = cloneForUpdate(prev[p.toolCallRef], p.toolCallRef);
    applyDelta(s, p);
    return { ...prev, [p.toolCallRef]: s };
  }

  return prev;
}
