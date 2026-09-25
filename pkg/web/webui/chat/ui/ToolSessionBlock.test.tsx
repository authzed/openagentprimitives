import * as React from "react";
import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import { ToolSessionBlock } from "./ToolSessionBlock";
import { emptyToolSession, parseAnsi, type ToolSessionState } from "./toolSession";

afterEach(cleanup);

// live builds a streaming (not yet done) session carrying the given raw lines
// as stdout, so each test states only what it cares about.
function live(over: Partial<ToolSessionState> = {}, lines: string[] = []): ToolSessionState {
  return {
    ...emptyToolSession("call-1"),
    outerTool: "claude",
    reason: "running unit tests",
    lines: lines.map((t) => ({ kind: "stdout" as const, spans: parseAnsi(t) })),
    ...over,
  };
}

describe("ToolSessionBlock", () => {
  it("heads the block with the dispatched tool and the call's reason", () => {
    render(<ToolSessionBlock state={live()} />);
    expect(screen.getByText(/claude/)).toBeTruthy();
    expect(screen.getByText(/running unit tests/)).toBeTruthy();
  });

  it("renders transcript text without leaking ANSI escape codes", () => {
    render(<ToolSessionBlock state={live({}, ["[31mFAIL[0m TestParse"])} />);
    const body = screen.getByTestId("tool-session-body");
    expect(body.textContent).toContain("FAIL TestParse");
    expect(body.textContent).not.toContain("");
    expect(body.textContent).not.toContain("[31m");
  });

  it("colours a span from its parsed ANSI foreground", () => {
    render(<ToolSessionBlock state={live({}, ["[31mFAIL[0m"])} />);
    const coloured = screen.getByText("FAIL");
    expect(coloured.getAttribute("style")).toMatch(/color/);
  });

  it("says how much earlier output was dropped rather than presenting a silent truncation", () => {
    render(<ToolSessionBlock state={live({ elided: 120 }, ["tail line"])} />);
    expect(screen.getByText(/120/)).toBeTruthy();
    expect(screen.getByText(/elided/i)).toBeTruthy();
  });

  it("shows no elision note when nothing was dropped", () => {
    render(<ToolSessionBlock state={live({}, ["only line"])} />);
    expect(screen.queryByText(/elided/i)).toBeNull();
  });

  it("reports a successful result with its duration and cost", () => {
    render(<ToolSessionBlock state={live({ done: true, ok: true, durationMs: 18200, costUsd: 0.03 })} />);
    const footer = screen.getByTestId("tool-session-footer");
    expect(footer.textContent).toMatch(/18\.2s/);
    expect(footer.textContent).toMatch(/\$0\.03/);
  });

  it("reports a failure with its exit code", () => {
    render(<ToolSessionBlock state={live({ done: true, ok: false, exitCode: 2, exitReason: "failed" })} />);
    const footer = screen.getByTestId("tool-session-footer");
    expect(footer.textContent).toMatch(/exit 2/);
    expect(footer.textContent).toMatch(/failed/);
  });

  it("stays expanded while streaming", () => {
    render(<ToolSessionBlock state={live({}, ["working"])} />);
    expect(screen.getByTestId("tool-session-body")).toBeTruthy();
  });

  // A finished, successful run is noise in a long conversation; a FAILED one is
  // the thing the user actually wants to read. Collapsing both would hide it.
  it("auto-collapses a successful run but leaves a failed one open", () => {
    const { unmount } = render(<ToolSessionBlock state={live({ done: true, ok: true }, ["quiet"])} />);
    expect(screen.queryByTestId("tool-session-body")).toBeNull();
    unmount();

    render(<ToolSessionBlock state={live({ done: true, ok: false, exitCode: 1 }, ["boom"])} />);
    expect(screen.getByTestId("tool-session-body")).toBeTruthy();
  });

  it("lets the user re-open a collapsed transcript", () => {
    render(<ToolSessionBlock state={live({ done: true, ok: true }, ["hidden detail"])} />);
    expect(screen.queryByTestId("tool-session-body")).toBeNull();

    fireEvent.click(screen.getByTestId("tool-session-toggle"));
    expect(screen.getByTestId("tool-session-body").textContent).toContain("hidden detail");
  });

  it("marks stderr lines so they are distinguishable from stdout", () => {
    const state = live();
    state.lines = [
      { kind: "stdout", spans: parseAnsi("normal") },
      { kind: "stderr", spans: parseAnsi("warning") },
    ];
    render(<ToolSessionBlock state={state} />);
    expect(screen.getByText("warning").closest("[data-stream]")?.getAttribute("data-stream")).toBe("stderr");
  });

  it("labels a tool_use line with the streaming agent's inner tool", () => {
    const state = live();
    state.lines = [{ kind: "event", spans: parseAnsi("pkg/sample/sample_test.go"), toolName: "Read" }];
    render(<ToolSessionBlock state={state} />);
    expect(screen.getByText("Read")).toBeTruthy();
  });
});
