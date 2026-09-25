import { describe, it, expect } from "vitest";
import { reduceToolSessions, MAX_TRANSCRIPT_LINES, type ToolSessionMap } from "./toolSession";
import type { ChatFrame } from "./types";

const SESSION = { namespace: "default", name: "sess-1" };

// b64 encodes a JS string as the base64 Go's []byte marshals to, so tests
// exercise the real wire shape rather than a convenient fiction.
function b64(s: string): string {
  const bytes = new TextEncoder().encode(s);
  let bin = "";
  for (const byte of bytes) bin += String.fromCharCode(byte);
  return btoa(bin);
}

// b64Bytes encodes raw bytes — used to split a multi-byte UTF-8 character
// across two deltas, which a naive per-chunk decode mangles.
function b64Bytes(bytes: number[]): string {
  let bin = "";
  for (const byte of bytes) bin += String.fromCharCode(byte);
  return btoa(bin);
}

// delta builds a tool_session_delta frame. The wsFrame payload double-nests
// (sink.go's toFrame does `Payload: m`, and m is builtin.MsgToolSessionDelta
// which itself has {session, payload}) — verified against the Go types.
function delta(
  ref: string,
  opts: { data?: string; stream?: string; terminal?: boolean; exitCode?: number; exitReason?: string },
): ChatFrame {
  return {
    type: "tool_session_delta",
    session: SESSION,
    payload: {
      session: SESSION,
      payload: {
        toolCallRef: ref,
        stream: opts.stream ?? "stdout",
        data: opts.data,
        terminal: opts.terminal,
        exitCode: opts.exitCode,
        exitReason: opts.exitReason,
      },
    },
  } as ChatFrame;
}

function event(ref: string, inner: Record<string, unknown>): ChatFrame {
  return {
    type: "tool_session_event",
    session: SESSION,
    payload: { session: SESSION, payload: { toolCallRef: ref, ...inner } },
  } as ChatFrame;
}

// text flattens a line's styled spans back to plain text, which is what the
// assertions are actually about.
function text(line: { spans: { text: string }[] }): string {
  return line.spans.map((sp) => sp.text).join("");
}

const empty: ToolSessionMap = {};

describe("reduceToolSessions — line assembly", () => {
  it("joins a line split across two chunks instead of inventing two lines", () => {
    let s = reduceToolSessions(empty, delta("r1", { data: b64("ok  git") }));
    expect(s.r1.lines).toHaveLength(0);

    s = reduceToolSessions(s, delta("r1", { data: b64("hub.com/sample\n") }));
    expect(s.r1.lines).toHaveLength(1);
    expect(s.r1.lines[0].spans.map((sp) => sp.text).join("")).toBe("ok  github.com/sample");
  });

  it("holds a trailing partial line until its newline arrives", () => {
    const s = reduceToolSessions(empty, delta("r1", { data: b64("no newline yet") }));
    expect(s.r1.lines).toHaveLength(0);
  });

  it("commits every complete line in a multi-line chunk", () => {
    const s = reduceToolSessions(empty, delta("r1", { data: b64("one\ntwo\nthree\n") }));
    expect(s.r1.lines.map((l) => l.spans.map((sp) => sp.text).join(""))).toEqual(["one", "two", "three"]);
  });

  it("strips a trailing CR so CRLF output does not leave a stray carriage return", () => {
    const s = reduceToolSessions(empty, delta("r1", { data: b64("windows\r\n") }));
    expect(s.r1.lines[0].spans.map((sp) => sp.text).join("")).toBe("windows");
  });

  it("keeps stdout and stderr partials from corrupting each other", () => {
    let s = reduceToolSessions(empty, delta("r1", { data: b64("OUT-"), stream: "stdout" }));
    s = reduceToolSessions(s, delta("r1", { data: b64("ERR-"), stream: "stderr" }));
    s = reduceToolSessions(s, delta("r1", { data: b64("done\n"), stream: "stdout" }));
    s = reduceToolSessions(s, delta("r1", { data: b64("bad\n"), stream: "stderr" }));

    const text = s.r1.lines.map((l) => `${l.kind}:${l.spans.map((sp) => sp.text).join("")}`);
    expect(text).toEqual(["stdout:OUT-done", "stderr:ERR-bad"]);
  });

  it("decodes a UTF-8 character split across two chunks", () => {
    // "é" is 0xC3 0xA9 — split so a per-chunk decode would yield replacement chars.
    let s = reduceToolSessions(empty, delta("r1", { data: b64Bytes([0xc3]) }));
    s = reduceToolSessions(s, delta("r1", { data: b64Bytes([0xa9, 0x0a]) }));
    expect(s.r1.lines[0].spans.map((sp) => sp.text).join("")).toBe("é");
  });
});

describe("reduceToolSessions — ANSI", () => {
  it("parses colour into spans rather than leaking escape codes into the text", () => {
    const s = reduceToolSessions(empty, delta("r1", { data: b64("[31mFAIL[0m ok\n") }));
    const line = s.r1.lines[0];

    expect(line.spans.map((sp) => sp.text).join("")).toBe("FAIL ok");
    expect(line.spans.map((sp) => sp.text).join("")).not.toContain("");
    expect(line.spans[0].fg).toBeTruthy();
  });

  it("survives an escape sequence split across two chunks", () => {
    let s = reduceToolSessions(empty, delta("r1", { data: b64("[3") }));
    s = reduceToolSessions(s, delta("r1", { data: b64("1mFAIL[0m\n") }));

    const line = s.r1.lines[0];
    expect(line.spans.map((sp) => sp.text).join("")).toBe("FAIL");
    expect(line.spans[0].fg).toBeTruthy();
  });
});

describe("reduceToolSessions — bounding", () => {
  it("keeps only the most recent lines and counts what it dropped", () => {
    const total = MAX_TRANSCRIPT_LINES + 25;
    let s = empty;
    for (let i = 0; i < total; i++) {
      s = reduceToolSessions(s, delta("r1", { data: b64(`line-${i}\n`) }));
    }
    expect(s.r1.lines).toHaveLength(MAX_TRANSCRIPT_LINES);
    expect(s.r1.elided).toBe(25);
    // The newest line survives; the oldest does not.
    expect(s.r1.lines[s.r1.lines.length - 1].spans.map((sp) => sp.text).join("")).toBe(`line-${total - 1}`);
    expect(s.r1.lines[0].spans.map((sp) => sp.text).join("")).toBe("line-25");
  });
});

describe("reduceToolSessions — correlation and lifecycle", () => {
  it("keeps interleaved tool calls in separate transcripts", () => {
    let s = reduceToolSessions(empty, delta("r1", { data: b64("from-one\n") }));
    s = reduceToolSessions(s, delta("r2", { data: b64("from-two\n") }));

    expect(s.r1.lines[0].spans.map((sp) => sp.text).join("")).toBe("from-one");
    expect(s.r2.lines[0].spans.map((sp) => sp.text).join("")).toBe("from-two");
  });

  it("takes the header from any event, since Reason rides on every one", () => {
    const s = reduceToolSessions(
      empty,
      event("r1", { eventType: "tool_use_start", outerTool: "claude", reason: "running unit tests", toolName: "Bash" }),
    );
    expect(s.r1.outerTool).toBe("claude");
    expect(s.r1.reason).toBe("running unit tests");
  });

  // The gloss rides on `summary`, NOT `text` — the runner copies
  // toolkitstream.Event.Summary there and leaves Text empty on tool_use events
  // (pkg/tools/toolkitstream/claude/parser.go). Reading `text` here rendered every
  // line as a bare "Bash" tag with no command after it.
  it("records a tool_use_start tagged with the inner tool AND its input gloss", () => {
    const s = reduceToolSessions(
      empty,
      event("r1", { eventType: "tool_use_start", toolName: "Read", summary: "Reading parser_test.go" }),
    );
    expect(s.r1.lines[0].kind).toBe("event");
    expect(s.r1.lines[0].toolName).toBe("Read");
    expect(text(s.r1.lines[0])).toBe("Reading parser_test.go");
  });

  it("marks a successful tool_use_stop with its result gloss", () => {
    const s = reduceToolSessions(empty, event("r1", { eventType: "tool_use_stop", ok: true, summary: "Packages: +412" }));
    expect(text(s.r1.lines[0])).toContain("✓");
    expect(text(s.r1.lines[0])).toContain("Packages: +412");
  });

  it("marks a failed tool_use_stop so a failure is not read as a success", () => {
    const s = reduceToolSessions(empty, event("r1", { eventType: "tool_use_stop", ok: false, summary: "exit status 1" }));
    expect(text(s.r1.lines[0])).toContain("✗");
    expect(text(s.r1.lines[0])).not.toContain("✓");
  });

  // A stop carrying no gloss must still show its outcome; rendering nothing
  // leaves a blank row that reads as a rendering glitch.
  it("still shows the outcome mark on a tool_use_stop with no gloss", () => {
    const s = reduceToolSessions(empty, event("r1", { eventType: "tool_use_stop", ok: true }));
    expect(text(s.r1.lines[0]).trim()).toBe("✓");
  });

  it("closes the session on the result event with its outcome and cost", () => {
    const s = reduceToolSessions(
      empty,
      event("r1", { eventType: "result", ok: true, durationMs: 18200, costUsd: 0.03 }),
    );
    expect(s.r1.done).toBe(true);
    expect(s.r1.ok).toBe(true);
    expect(s.r1.durationMs).toBe(18200);
    expect(s.r1.costUsd).toBeCloseTo(0.03);
  });

  it("closes the session on a terminal delta with its exit detail", () => {
    const s = reduceToolSessions(empty, delta("r1", { terminal: true, exitCode: 2, exitReason: "failed" }));
    expect(s.r1.done).toBe(true);
    expect(s.r1.ok).toBe(false);
    expect(s.r1.exitCode).toBe(2);
    expect(s.r1.exitReason).toBe("failed");
  });

  it("flushes a partial trailing line when the stream terminates", () => {
    let s = reduceToolSessions(empty, delta("r1", { data: b64("no trailing newline") }));
    s = reduceToolSessions(s, delta("r1", { terminal: true, exitCode: 0, exitReason: "completed" }));
    expect(s.r1.lines.map((l) => l.spans.map((sp) => sp.text).join(""))).toEqual(["no trailing newline"]);
  });

  it("returns the same map reference for frames it does not own, so React can bail out", () => {
    const s = reduceToolSessions(empty, { type: "turn_progress", session: SESSION, payload: {} } as ChatFrame);
    expect(s).toBe(empty);
  });
});
